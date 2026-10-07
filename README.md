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

### Deploying migrations 000021 to 000024

The migrations were renumbered before reaching any shared database. 000021 (session versions) and 000022 (device tokens) are fixed. 000023 keys notifications by parent. Phone normalisation is 000024, which must stay last until Staging's phone data has been checked.

**Order on Staging:**
```bash
migrate -path db/migrations -database "$DATABASE_URL" version    # 20, 21 or 22
migrate -path db/migrations -database "$DATABASE_URL" goto 23    # applies whatever is missing of 21, 22, 23
# deploy the code
psql "$DATABASE_URL" -X -f db/scripts/check_phone_formats.sql    # read-only; send the result before 000024
migrate -path db/migrations -database "$DATABASE_URL" up         # 000024, after the check
```

1. **The old code runs unchanged on 000021–000023;** the new code needs all three and fails without them.
   - 000021 only adds defaulted `session_version` columns.
   - 000022 only adds the `device_tokens` table, backfilled from `students.fcm_token`, which it leaves in place.
   - 000023 only adds a nullable `notifications.parent_id`, its indexes and an insert trigger. `parent_phone` is unchanged.
2. **Notifications written during the rollout window** need no re-backfill. Until the new code is deployed, the old code inserts notifications without `parent_id`; 000023's trigger fills it from `parent_phone` at insert time.
3. **Unmatched notifications:** rows whose `parent_phone` matches no parent keep `parent_id` NULL. They were visible to no parent before, and still aren't; nothing is deleted. The migration reports their number as a NOTICE; to see it again (read-only): `SELECT COUNT(*) FROM notifications WHERE parent_id IS NULL;`. A notification inserted for a phone number before any parent has it stays unlinked, whereas the old phone join used to show it once such a parent was created. Real code only writes notifications for existing parents, so this only concerns rows inserted by hand.
4. **Deploy promptly after migrating.** Tokens the old code stores in `students.fcm_token` after 000022's backfill are not copied; those phones get pushes again once the app calls `PUT /api/mobile/device-token`.
5. **After the deploy, every parent and admin must log in once,** because tokens without a session version are rejected.
6. **Before 000024, check the stored numbers** with the read-only `db/scripts/check_phone_formats.sql`. It prints number shapes and counts but no phone numbers, and lists the parent ids 000024 would abort on.
7. **While 000024 is not applied,** the new code normalises what users type but compares it with the stored strings:
   - A parent whose number is stored in another format, such as `07XXXXXXXXX`, cannot log in.
   - Creating a student with that parent's number and a `parent_pin` makes a second parent in canonical form, which 000024 then reports as a collision; without a `parent_pin` the request is refused with 400, because the server treats the number as a new parent.
   - The check script's `would change` count is the number of parents affected; when it is 0, nothing is affected.
8. **000024 normalises stored phone numbers,** including `notifications.parent_phone`, which keeps history matching for any code that still reads it (a rolled-back deploy, or 000023's trigger). It aborts without changing anything if a parent's number is not an Iraqi mobile number, or if two parents hold the same number in different formats; the error lists the parent ids. Because the whole file runs as one transaction, the schema stays at version 23, but golang-migrate records version 24 as dirty. Fix the listed parents, run `migrate force 23`, then `migrate up` again.
9. **A later migration, after this deploy,** drops `students.fcm_token`, `notifications.parent_phone` and the 000023 trigger, which the new code does not use.

### Settings

| Variable | Default | Purpose |
|---|---|---|
| `TRUSTED_PROXY_CIDRS` | empty (also on Railway) | Proxies whose client-IP headers are believed (comma-separated CIDRs or addresses) |
| `SHUTDOWN_TIMEOUT` | `25s` | Deadline for the whole graceful shutdown |
| `ABSENCE_CRON_SCHEDULE` | `0 12 * * 0-4` | When absence notifications go out (Asia/Baghdad; Sunday–Thursday at noon) |
| `RAILWAY_DEPLOYMENT_DRAINING_SECONDS` | Railway: `0` | Time Railway waits after `SIGTERM`; set to `30` |

