# FutureKids - School Attendance System 


This system is designed to serve a specific elementary school by automating the student attendance process.

## API contract

[`future_kids_api.yaml`](future_kids_api.yaml) is the contract (RULES.md §4). Notes for the Flutter app are in [docs/FLUTTER_HANDOFF.md](docs/FLUTTER_HANDOFF.md).

- Lint: `npx @redocly/cli lint` (rules in `redocly.yaml`).
- Contract test: `TEST_DATABASE_URL=postgres://localhost:5432/postgres go test ./tests/contract/` runs the real server on a throwaway database and fails when a route in `cmd/api/main.go` and the spec disagree, or when any response does not match its documented status, headers or schema.

## Operations

### Graceful shutdown

On `SIGTERM` (every Railway deploy) or `SIGINT`, the server shuts down in this order, logging each phase (`Shutdown: …`):

1. Stops accepting connections and lets in-flight requests finish — including an ADMS batch mid-transaction.
2. Stops the absence cron job and waits for a run in progress.
3. Waits for background work: push notifications, notification history, device `last_sync`.
4. Closes the database pool, then exits `0`.

All phases share one deadline, `SHUTDOWN_TIMEOUT` (default `25s`). If it is exceeded the server logs an ERROR and exits `1` immediately; PostgreSQL rolls back any open transaction, so an interrupted batch is never stored partially.

**Railway:** its default draining time is 0 seconds (immediate `SIGKILL`), which skips all of the above. Set the service variable `RAILWAY_DEPLOYMENT_DRAINING_SECONDS=30` (anything above `SHUTDOWN_TIMEOUT`). The server logs a warning at startup when it runs on Railway without enough draining time.

### Login protection and sessions

- **Parent login:** 5 failed attempts per phone number per 15 minutes, counted on the normalised number (every format shares one count).
- **Admin login:** 5 failures per username from one client IP and 20 failures per client IP, per 15 minutes. Only failures count, so the admin can always log in from an IP that has not failed. Unknown usernames take as long as wrong passwords.
- **Limiter memory:** each limiter tracks at most 100,000 keys. When full it refuses new keys (429) rather than forget an existing lockout.
- **Client IP:** forwarding headers (`X-Real-IP`, then the rightmost `X-Forwarded-For` entry) are believed only on connections from `TRUSTED_PROXY_CIDRS`. When a trusted proxy names no client, the per-IP limit is skipped rather than shared by everyone. The variable is empty unless set, on Railway too. Until it is set there, every request appears to come from Railway's proxy, so the admin-login limits are shared by all clients (the server warns at startup). To choose the value, read the `Observed client address` log lines: the first 20 requests after each start log `remote_addr`, `x_real_ip`, `x_forwarded_for` and the resolved `client_ip`.
- **Sessions:** changing a parent's PIN through the admin API, or rotating the admin password with `db/scripts/rotate_admin_password.sql`, raises that account's `session_version`, and every token issued before gets 401. Each authenticated request makes one indexed lookup of the version.

### Deploying migrations 000021 and 000022

The two migrations are independent and can be applied at different times.

1. **000021 adds session versions.** Apply only this one with `migrate up 1`, then deploy the code. The old code runs unchanged on this schema; the new code needs it and fails without it. After the deploy, every parent and admin must log in once, because tokens without a session version are rejected.
2. **Before 000022, check the stored numbers.** Run the read-only `db/scripts/check_phone_formats.sql`. It prints number shapes and counts but no phone numbers, and lists the parent ids 000022 would abort on.
3. **While only 000021 is applied,** the new code normalises what users type but compares it with the stored strings. A parent whose number is stored in another format, such as `07XXXXXXXXX`, cannot log in. Creating a student with that parent's number makes a second parent in canonical form, which 000022 then reports as a collision. The check script's `would change` count is the number of parents affected; when it is 0, nothing is affected.
4. **000022 normalises stored phone numbers.** It aborts without changing anything if a parent's number is not an Iraqi mobile number, or if two parents hold the same number in different formats. The error lists the parent ids. Because the whole file runs as one transaction, the schema stays at version 21, but golang-migrate records version 22 as dirty. Fix the listed parents, run `migrate force 21`, then `migrate up` again.

### Settings

| Variable | Default | Purpose |
|---|---|---|
| `TRUSTED_PROXY_CIDRS` | empty (also on Railway) | Proxies whose client-IP headers are believed (comma-separated CIDRs or addresses) |
| `SHUTDOWN_TIMEOUT` | `25s` | Deadline for the whole graceful shutdown |
| `ABSENCE_CRON_SCHEDULE` | `0 12 * * 0-4` | When absence notifications go out (Asia/Baghdad; Sunday–Thursday at noon) |
| `RAILWAY_DEPLOYMENT_DRAINING_SECONDS` | Railway: `0` | Time Railway waits after `SIGTERM`; set to `30` |

