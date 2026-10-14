#!/usr/bin/env bash
# Encrypted PostgreSQL backup: pg_dump (custom format) piped straight into age, so a plaintext dump
# never touches disk. Used by .github/workflows/db-backup.yml; see docs/PRODUCTION_RUNBOOK.md §8.
#
# Inputs (environment only):
#   BACKUP_DATABASE_URL   postgres:// URL of the database (never printed)
#   BACKUP_AGE_RECIPIENT  age public key (age1...) the dump is encrypted to
#   BACKUP_OUT_DIR        directory for the encrypted file (created if missing)
#   BACKUP_LABEL          staging | production
# Output: $BACKUP_OUT_DIR/futurekids-<label>-<UTC timestamp>.dump.age (mode 0600).
# Exit codes: 0 written; 1 the dump or encryption failed; 2 refused (missing or invalid input or tool).
set -euo pipefail
umask 077

readonly MIN_BYTES=1024

refuse() {
	echo "db-backup: $*" >&2
	exit 2
}

missing=()
for name in BACKUP_DATABASE_URL BACKUP_AGE_RECIPIENT BACKUP_OUT_DIR BACKUP_LABEL; do
	if [[ -z "${!name:-}" ]]; then
		missing+=("$name")
	fi
done
if ((${#missing[@]} > 0)); then
	refuse "missing environment variable(s): ${missing[*]}"
fi
case "$BACKUP_LABEL" in
staging | production) ;;
*) refuse "BACKUP_LABEL must be staging or production" ;;
esac
if [[ ! "$BACKUP_AGE_RECIPIENT" =~ ^age1[02-9ac-hj-np-z]{58}$ ]]; then
	refuse "BACKUP_AGE_RECIPIENT is not an age public key (age1 followed by 58 lowercase characters)"
fi
for tool in pg_dump age; do
	command -v "$tool" >/dev/null 2>&1 || refuse "$tool is not installed (or not on PATH)"
done
case "$BACKUP_DATABASE_URL" in
postgres://* | postgresql://*) ;;
*) refuse "BACKUP_DATABASE_URL must be a postgres:// or postgresql:// URL" ;;
esac

# The parts of the URL that must never appear in output, longest first.
url_rest=${BACKUP_DATABASE_URL#*://}
url_authority=${url_rest%%/*}
url_authority=${url_authority%%\?*}
url_userinfo=""
if [[ "$url_authority" == *@* ]]; then
	url_userinfo=${url_authority%@*}
fi
url_host=${url_authority##*@}
url_host=${url_host%:*}
url_user=${url_userinfo%%:*}
url_password=""
if [[ "$url_userinfo" == *:* ]]; then
	url_password=${url_userinfo#*:}
fi
url_db=""
if [[ "$url_rest" == */* ]]; then
	url_db=${url_rest#*/}
	url_db=${url_db%%\?*}
fi
redact() {
	local text=$1 part
	for part in "$BACKUP_DATABASE_URL" "$url_password" "$url_userinfo" "$url_user" "$url_db" "$url_host"; do
		if [[ -n "$part" ]]; then
			text=${text//"$part"/<redacted>}
		fi
	done
	printf '%s\n' "$text"
}

mkdir -p "$BACKUP_OUT_DIR"
name="futurekids-${BACKUP_LABEL}-$(date -u +%Y%m%dT%H%M%SZ).dump.age"
final="$BACKUP_OUT_DIR/$name"
partial="$final.partial"
work=$(mktemp -d)
errfile="$work/pg_dump.err"
cleanup() {
	rm -f "$partial"
	rm -rf "$work"
}
trap cleanup EXIT

# TLS unless the caller or the URL's own sslmode says otherwise (libpq: the URL wins).
export PGSSLMODE="${PGSSLMODE:-require}"

set +e
pg_dump --no-owner --no-acl -Fc --dbname="$BACKUP_DATABASE_URL" 2>"$errfile" | age -r "$BACKUP_AGE_RECIPIENT" -o "$partial"
status=("${PIPESTATUS[@]}")
set -e
if ((status[0] != 0)); then
	echo "db-backup: pg_dump failed (exit ${status[0]}); its messages, with the connection details redacted:" >&2
	redact "$(cat "$errfile")" >&2
	exit 1
fi
if ((status[1] != 0)); then
	echo "db-backup: age failed to encrypt the dump (exit ${status[1]})" >&2
	exit 1
fi

size=$(wc -c <"$partial" | tr -d ' ')
if ((size < MIN_BYTES)); then
	echo "db-backup: encrypted dump is smaller than 1 KB ($size bytes); refusing it" >&2
	exit 1
fi
mv "$partial" "$final"
echo "Backup written: $name ($size bytes, $(pg_dump --version))"
