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

### Deploying migrations 000021 to 000028

The migrations were renumbered before reaching any shared database. 000021 (session versions) and 000022 (device tokens) are fixed. 000023 keys notifications by parent. 000024 stores banner pictures. 000025 adds the table of notifications the admin sends to parents, and 000026 renames it and its column to `broadcasts` and `broadcast_id`. 000027 adds the school closures (holidays and pauses). Phone normalisation is 000028, which must stay last until Staging's phone data has been checked.

**Order on Staging** (Staging is at 26):
```bash
migrate -path db/migrations -database "$DATABASE_URL" version    # 26 (20 to 25 on an older database)
migrate -path db/migrations -database "$DATABASE_URL" goto 27    # applies whatever is missing of 21 to 27
# deploy the code right after
psql "$DATABASE_URL" -X -f db/scripts/check_phone_formats.sql    # read-only; send the result before 000028
migrate -path db/migrations -database "$DATABASE_URL" up         # 000028, after the check
```

1. **Older code runs unchanged on 000021–000025 and 000027** (000026 renames only what the broadcast endpoints use); the new code needs 000021–000027 and fails without them.
   - 000021 only adds defaulted `session_version` columns.
   - 000022 only adds the `device_tokens` table, backfilled from `students.fcm_token`, which it leaves in place.
   - 000023 only adds a nullable `notifications.parent_id`, its indexes and an insert trigger. `parent_phone` is unchanged.
   - 000024 only adds the `banner_images` table. The banner routes and the parent banner list read it.
   - 000025 only adds the table that 000026 renames to `broadcasts`, and a nullable `notifications` column (renamed `broadcast_id`) with its index. Every notification the old code inserts leaves it NULL.
   - **000027 only adds** the `school_closures` table, which no earlier code reads, so Staging goes to 27 before the deploy. Without it the new code answers 500 on the daily views and stores punches without notifications.
   - **000026 only renames** that table, its sequence, constraints and index, and the column, keeping every row and link. Between 000026 and the deploy, only the old code's admin endpoints for sending and listing broadcasts fail, and nothing uses them yet. Punch and absence notifications, the parent notifications list and every other route keep working, because no other code reads or writes the renamed table or column. Deploy right after migrating to keep the window short.
2. **Notifications written during the rollout window** need no re-backfill. Until the new code is deployed, the old code inserts notifications without `parent_id`; 000023's trigger fills it from `parent_phone` at insert time.
3. **Unmatched notifications:** rows whose `parent_phone` matches no parent keep `parent_id` NULL. They were visible to no parent before, and still aren't; nothing is deleted. The migration reports their number as a NOTICE; to see it again (read-only): `SELECT COUNT(*) FROM notifications WHERE parent_id IS NULL;`. A notification inserted for a phone number before any parent has it stays unlinked, whereas the old phone join used to show it once such a parent was created. Real code only writes notifications for existing parents, so this only concerns rows inserted by hand.
4. **Deploy promptly after migrating.** Tokens the old code stores in `students.fcm_token` after 000022's backfill are not copied; those phones get pushes again once the app calls `PUT /api/mobile/device-token`.
5. **After the deploy, every parent and admin must log in once,** because tokens without a session version are rejected.
6. **Before 000028, check the stored numbers** with the read-only `db/scripts/check_phone_formats.sql`. It prints number shapes and counts but no phone numbers, and lists the parent ids 000028 would abort on.
7. **While 000028 is not applied,** the new code normalises what users type but compares it with the stored strings:
   - A parent whose number is stored in another format, such as `07XXXXXXXXX`, cannot log in.
   - Creating a student with that parent's number and a `parent_pin` makes a second parent in canonical form, which 000028 then reports as a collision; without a `parent_pin` the request is refused with 400, because the server treats the number as a new parent.
   - The check script's `would change` count is the number of parents affected; when it is 0, nothing is affected.
8. **000028 normalises stored phone numbers,** including `notifications.parent_phone`, which keeps history matching for any code that still reads it (a rolled-back deploy, or 000023's trigger). It aborts without changing anything if a parent's number is not an Iraqi mobile number, or if two parents hold the same number in different formats; the error lists the parent ids. Because the whole file runs as one transaction, the schema stays at version 27, but golang-migrate records version 28 as dirty. Fix the listed parents, run `migrate force 27`, then `migrate up` again.
9. **Rolling back 000027** deletes every registered closure (the notifications already sent for them stay); with the code before closures, reports count every weekday as a school day again. **Rolling back 000026** only restores the first names (no data is lost). **Rolling back 000025** deletes the broadcasts sent log; the notifications parents received stay but lose their link to it. **Rolling back 000024** deletes every uploaded banner picture. Those banners stay but are deactivated, so the old code never shows parents a banner without a picture. Back up first (see `db/migrations/README.md`).
10. **A later migration, after this deploy,** drops `students.fcm_token`, `notifications.parent_phone` and the 000023 trigger, which the new code does not use.

### Seeding Staging with fake data

`cmd/seed-staging` fills Staging with fake data for the Flutter developers: 47 students (`rfid_tag` 1004 to 1050) for 37 parents (`+9647000002001` to `+9647000002037`, parent credential `Sd7!Kx4p#Wm9qT2h`; parents seeded before that keep their earlier `314159`), weekly schedules, the fake device `SEED-FAKE-0001`, leaves, attendance history since the first day of the previous month, and 3 banners. Everything goes through the real API; it never touches students 1001 to 1003, the real device or the settings. It refuses any `BASE_URL` other than `https://futurekids-staging.up.railway.app`.

```bash
read -r -p "Admin username: " ADMIN_USERNAME
read -r -s -p "Admin password: " ADMIN_PASSWORD; echo
export BASE_URL=https://futurekids-staging.up.railway.app ADMIN_USERNAME ADMIN_PASSWORD
go run ./cmd/seed-staging            # dry run: prints the plan, changes nothing
go run ./cmd/seed-staging --apply    # seeds; a second run creates and sends nothing
unset ADMIN_PASSWORD
```

`read -s` keeps the password out of the screen and the shell history; never pass it on the command line. The output ends with the parents' phone numbers and PIN for the parent app.

- **Seeded data cannot be removed through the API.** Students can only be deactivated, punches and notifications are never deleted, and parents stay. Plan on keeping it, or clean it up with SQL after a backup.
- **Notifications:** the attendance history creates one check-in and one check-out notification per present day (about 1,800 history rows, dated the day you seed). Afterwards, the noon absence job notifies the parent of every seeded student without a punch, each school day.
- **If a run stops midway,** it prints what was done. Re-running `--apply` is safe; if the history of students created in the stopped run was not sent, add `--resume-history`.

### Settings

| Variable | Default | Purpose |
|---|---|---|
| `TRUSTED_PROXY_CIDRS` | empty (also on Railway) | Proxies whose client-IP headers are believed (comma-separated CIDRs or addresses) |
| `SHUTDOWN_TIMEOUT` | `25s` | Deadline for the whole graceful shutdown |
| `ABSENCE_CRON_SCHEDULE` | `0 12 * * 0-4` | When absence notifications go out (Asia/Baghdad; Sunday–Thursday at noon). The job does nothing on a day that is not a school day (weekend, holiday or pause) |
| `FAKE_TODAY` | empty | Tests only: the date (`YYYY-MM-DD`) the server takes as today, at the real time of day. Refused on Railway; the server logs a WARN when it is set |
| `RAILWAY_DEPLOYMENT_DRAINING_SECONDS` | Railway: `0` | Time Railway waits after `SIGTERM`; set to `30` |

