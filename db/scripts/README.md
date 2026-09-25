# Database Scripts

## rotate_admin_password.sql

**MANUAL EXECUTION ONLY — Do NOT automate or add to `db/migrations/`.**

Migration `000008_create_admins_table.up.sql` seeds `admin` / `admin123` for development and the e2e test. That password is public in this repo and must be rotated on every Staging/Production database before it goes live.

### Steps

1. Generate a strong random password (e.g., `openssl rand -base64 32`).
2. Copy the template to a gitignored file and edit the copy, never the template:
   ```bash
   cp db/scripts/rotate_admin_password.sql db/scripts/rotate_admin_password.with_password.sql
   ```
3. Replace `'NEW_PASSWORD_HERE'` in the copy with that password. If the placeholder is left in, the script aborts and changes nothing.
4. Run it ONLY on Staging/Production databases AFTER all migrations complete:
   ```bash
   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/scripts/rotate_admin_password.with_password.sql
   ```
   Expected output ends with `UPDATE 1` and `COMMIT`. `UPDATE 0` means no `admin` row exists.
5. Store the rotated password in a secure vault (1Password, HashiCorp Vault, etc.).
6. Delete the copy after execution:
   ```bash
   rm db/scripts/rotate_admin_password.with_password.sql
   ```

### Notes

- Do NOT version-control the password. Files matching `db/scripts/rotate_admin_password*.with_password.sql` are gitignored.
- Do NOT run this against the local development database — `tests/e2e_test.sh` logs in with `admin123`.
- The script enables the `pgcrypto` extension (bundled with PostgreSQL) for `crypt()`/`gen_salt()`. The resulting `$2a$` bcrypt hash is verified by the Go `bcrypt.CompareHashAndPassword` in `AdminLoginHandler`.
