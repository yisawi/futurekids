# FutureKids - School Attendance System 


This system is designed to serve a specific elementary school by automating the student attendance process.

## API contract

[`future_kids_api.yaml`](future_kids_api.yaml) is the contract (RULES.md §4). Notes for the Flutter app are in [docs/FLUTTER_HANDOFF.md](docs/FLUTTER_HANDOFF.md).

- Lint: `npx @redocly/cli lint` (rules in `redocly.yaml`).
- Contract test: `TEST_DATABASE_URL=postgres://localhost:5432/postgres go test ./tests/contract/` runs the real server on a throwaway database and fails when a route in `cmd/api/main.go` and the spec disagree, or when any response does not match its documented status, headers or schema.

## Operations

**Production:** bring-up, release process, backups, monitoring and rollback are in [docs/PRODUCTION_RUNBOOK.md](docs/PRODUCTION_RUNBOOK.md). **Backups:** a daily encrypted backup runs in GitHub Actions (`.github/workflows/db-backup.yml`, `scripts/db-backup.sh`); setup, restore and the rule to keep Production backups off while the repository is public are in [runbook section 8](docs/PRODUCTION_RUNBOOK.md#8-backups). In the Railway environment named `production` the server refuses to start with an unsafe configuration (see `RAILWAY_ENVIRONMENT_NAME` below).

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

### Deploying migrations 000021 to 000029

The migrations were renumbered before reaching any shared database. 000021 (session versions) and 000022 (device tokens) are fixed. 000023 keys notifications by parent. 000024 stores banner pictures. 000025 adds the table of notifications the admin sends to parents, and 000026 renames it and its column to `broadcasts` and `broadcast_id`. 000027 adds the school closures (holidays and pauses). 000028 adds the admin activity log. Phone normalisation is 000029, which must stay last until Staging's phone data has been checked.

**Order on Staging** (Staging is at 27):
```bash
migrate -path db/migrations -database "$DATABASE_URL" version    # expect 27
migrate -path db/migrations -database "$DATABASE_URL" goto 28    # activity log; additive, the old code runs on it
# deploy the code immediately after
psql "$DATABASE_URL" -X -f db/scripts/check_phone_formats.sql    # read-only
migrate -path db/migrations -database "$DATABASE_URL" up         # 000029, after the check
```

1. **Older code runs unchanged on 000021–000025, 000027 and 000028** (000026 renames only what the broadcast endpoints use); the new code needs 000021–000028 and fails without them.
   - 000021 only adds defaulted `session_version` columns.
   - 000022 only adds the `device_tokens` table, backfilled from `students.fcm_token`, which it leaves in place.
   - 000023 only adds a nullable `notifications.parent_id`, its indexes and an insert trigger. `parent_phone` is unchanged.
   - 000024 only adds the `banner_images` table. The banner routes and the parent banner list read it.
   - 000025 only adds the table that 000026 renames to `broadcasts`, and a nullable `notifications` column (renamed `broadcast_id`) with its index. Every notification the old code inserts leaves it NULL.
   - **000028 only adds** the `admin_activity_log` table, which no earlier code reads. Without it the new code still answers every request, but records nothing (one ERROR per admin write) and the activity log page answers 500.
   - **000027 only adds** the `school_closures` table, which no earlier code reads, so Staging goes to 27 before the deploy. Without it the new code answers 500 on the daily views and stores punches without notifications.
   - **000026 only renames** that table, its sequence, constraints and index, and the column, keeping every row and link. Between 000026 and the deploy, only the old code's admin endpoints for sending and listing broadcasts fail, and nothing uses them yet. Punch and absence notifications, the parent notifications list and every other route keep working, because no other code reads or writes the renamed table or column. Deploy right after migrating to keep the window short.
2. **Notifications written during the rollout window** need no re-backfill. Until the new code is deployed, the old code inserts notifications without `parent_id`; 000023's trigger fills it from `parent_phone` at insert time.
3. **Unmatched notifications:** rows whose `parent_phone` matches no parent keep `parent_id` NULL. They were visible to no parent before, and still aren't; nothing is deleted. The migration reports their number as a NOTICE; to see it again (read-only): `SELECT COUNT(*) FROM notifications WHERE parent_id IS NULL;`. A notification inserted for a phone number before any parent has it stays unlinked, whereas the old phone join used to show it once such a parent was created. Real code only writes notifications for existing parents, so this only concerns rows inserted by hand.
4. **Deploy promptly after migrating.** Tokens the old code stores in `students.fcm_token` after 000022's backfill are not copied; those phones get pushes again once the app calls `PUT /api/mobile/device-token`.
5. **After the deploy, every parent and admin must log in once,** because tokens without a session version are rejected.
6. **Before 000029, check the stored numbers** with the read-only `db/scripts/check_phone_formats.sql`. It prints number shapes and counts but no phone numbers, and lists the parent ids 000029 would abort on.
7. **While 000029 is not applied,** the new code normalises what users type but compares it with the stored strings:
   - A parent whose number is stored in another format, such as `07XXXXXXXXX`, cannot log in.
   - Creating a student with that parent's number and a `parent_pin` makes a second parent in canonical form, which 000029 then reports as a collision; without a `parent_pin` the request is refused with 400, because the server treats the number as a new parent.
   - The check script's `would change` count is the number of parents affected; when it is 0, nothing is affected.
8. **000029 normalises stored phone numbers,** including `notifications.parent_phone`, which keeps history matching for any code that still reads it (a rolled-back deploy, or 000023's trigger). It aborts without changing anything if a parent's number is not an Iraqi mobile number, or if two parents hold the same number in different formats; the error lists the parent ids. Because the whole file runs as one transaction, the schema stays at version 28, but golang-migrate records version 29 as dirty. Fix the listed parents, run `migrate force 28`, then `migrate up` again.
9. **Rolling back 000028** deletes the whole activity log. **Rolling back 000027** deletes every registered closure (the notifications already sent for them stay); with the code before closures, reports count every weekday as a school day again. **Rolling back 000026** only restores the first names (no data is lost). **Rolling back 000025** deletes the broadcasts sent log; the notifications parents received stay but lose their link to it. **Rolling back 000024** deletes every uploaded banner picture. Those banners stay but are deactivated, so the old code never shows parents a banner without a picture. Back up first (see `db/migrations/README.md`).
10. **000030 and 000031** drop `students.fcm_token`, `notifications.parent_phone` and the 000023 trigger, which the new code does not use (see the next section).

### Deploying migrations 000030 and 000031

000030 only makes `notifications.parent_phone` nullable. 000031 drops `notifications.parent_phone` (with `idx_notifications_phone` and 000023's trigger `notifications_fill_parent_id` and its function), `notifications.parent_phone_original` (from 000029) and `students.fcm_token`. No API field changes: login's `fcm_token` still registers a device token.

**Production** is built from an empty database through all 31 migrations in one go, so this split only matters for Staging.

**Order on Staging** (Staging is at 29):

1. **Back up:** `pg_dump "$DATABASE_URL" > backup.sql`. After step 5 it is the only copy of the dropped values.
2. **Migrate to 30** while the old code runs (it still writes the phone, which is allowed), and note the newest notification id as `N`:
   ```bash
   migrate -path db/migrations -database "$DATABASE_URL" version    # expect 29
   migrate -path db/migrations -database "$DATABASE_URL" goto 30
   psql "$DATABASE_URL" -X -c "SELECT MAX(id) AS n FROM notifications;"
   ```
3. **Deploy the new code** right after. It needs 30: on 29, punch and absence notifications are missing from the history (one ERROR each) and broadcasts and closures with parents answer 500.
4. **Check** (with `BASE_URL=https://futurekids-staging.up.railway.app`):
   - **Preflight, read-only, before the punch:**
     ```sql
     -- the device exists and is enabled: one row, active = t
     SELECT serial_number, COALESCE(is_active, true) AS active FROM devices WHERE serial_number = 'SEED-FAKE-0001';
     -- 1004 is an active student with a parent: is_active = t, parent_id set, phone_number +9647000002001
     SELECT s.id, s.is_active, s.parent_id, p.phone_number FROM students s LEFT JOIN parents p ON p.id = s.parent_id WHERE s.rfid_tag = '1004';
     -- today is a school day: dow 0-4 (Sunday to Thursday) and closures = 0
     SELECT EXTRACT(DOW FROM d) AS dow, (SELECT COUNT(*) FROM school_closures WHERE start_date <= d AND (end_date IS NULL OR end_date >= d)) AS closures
     FROM (SELECT (now() AT TIME ZONE 'Asia/Baghdad')::date AS d) t;
     -- 1004's punches today; none may fall in the window you punch in
     -- (check-in 06:30:00-09:30:59, check-out 11:30:00-13:30:59, Baghdad time)
     SELECT a.check_time FROM attendance_logs a JOIN students s ON s.id = a.student_id
     WHERE s.rfid_tag = '1004' AND a.check_time::date = (now() AT TIME ZONE 'Asia/Baghdad')::date ORDER BY a.check_time;
     ```
     If any check fails, don't send the punch:
     - **The device is missing or disabled:** use another active device listed by `SELECT serial_number FROM devices WHERE COALESCE(is_active, true);`, never the real one.
     - **1004 is inactive, has no parent, or another number:** pick another seeded student (`rfid_tag` 1005 to 1050) that passes, and use its parent's number in the broadcast and login checks.
     - **1004 already punched in the window:** pick another student, or wait for the other window.
     - **Today is not a school day:** skip the punch check (on such a day a punch is stored but never notified) and do it on the next school day; the broadcast and login checks still run.
   - **(a) Punch:** inside the check-in or check-out window. This writes a real `attendance_logs` row for 1004 on Staging.
     ```bash
     curl -s -X POST "$BASE_URL/iclock/cdata?SN=SEED-FAKE-0001&table=ATTLOG" --data-binary $'1004\t'"$(TZ=Asia/Baghdad date '+%F %T')"$'\t1\t1\n'   # OK
     ```
     ```sql
     SELECT n.id, n.parent_id, n.title FROM notifications n JOIN students s ON s.parent_id = n.parent_id WHERE s.rfid_tag = '1004' AND n.id > N;  -- one row
     SELECT COUNT(*) FROM notifications WHERE id > N AND parent_id IS NULL;  -- 0
     ```
   - **(b) Broadcast** to the fake parent:
     ```bash
     ADMIN=$(curl -s -X POST "$BASE_URL/api/admin/login" -H 'Content-Type: application/json' -d "{\"username\":\"$ADMIN_USERNAME\",\"password\":\"$ADMIN_PASSWORD\"}" | jq -r .data.token)
     curl -s -X POST "$BASE_URL/api/admin/broadcasts" -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
       -d '{"title":"M1 check","body":"فحص الترحيل","audience":{"type":"parent","parent_phone":"+9647000002001"}}'   # data.recipient_count 1, data.id
     ```
     ```sql
     SELECT b.recipient_count, COUNT(n.id), COUNT(n.parent_id) FROM broadcasts b JOIN notifications n ON n.broadcast_id = b.id WHERE b.id = <data.id> GROUP BY b.id;  -- 1, 1, 1
     ```
   - **(c) Login with `fcm_token`**, then remove the test token:
     ```bash
     PARENT=$(curl -s -X POST "$BASE_URL/api/mobile/login" -H 'Content-Type: application/json' \
       -d '{"phone":"07000002001","pin":"Sd7!Kx4p#Wm9qT2h","fcm_token":"m1-staging-check-token-0000000001"}' | jq -r .data.token)
     curl -s "$BASE_URL/api/mobile/notifications" -H "Authorization: Bearer $PARENT"   # lists "M1 check"
     ```
     ```sql
     SELECT parent_id FROM device_tokens WHERE token = 'm1-staging-check-token-0000000001';  -- the parent of +9647000002001
     ```
     ```bash
     curl -s -X DELETE "$BASE_URL/api/mobile/device-token" -H "Authorization: Bearer $PARENT" -H 'Content-Type: application/json' -d '{"token":"m1-staging-check-token-0000000001"}'
     ```
5. **Only after step 4 passes, migrate to 31.** The first query counts notifications that match no parent: they are kept, but lose their last identifying field. The second counts the token values about to be dropped.
   ```sql
   SELECT COUNT(*) FROM notifications WHERE parent_id IS NULL;
   SELECT COUNT(*) FROM students WHERE fcm_token IS NOT NULL;
   ```
   ```bash
   migrate -path db/migrations -database "$DATABASE_URL" up         # 000031
   migrate -path db/migrations -database "$DATABASE_URL" version    # expect 31
   ```
   ```sql
   SELECT table_name, column_name FROM information_schema.columns
   WHERE (table_name, column_name) IN (('notifications', 'parent_phone'), ('notifications', 'parent_phone_original'), ('students', 'fcm_token'));  -- no rows
   SELECT tgname FROM pg_trigger WHERE tgname = 'notifications_fill_parent_id';  -- no rows
   ```
6. **Repeat step 4** with a new `N`, the title `M1 check 2` (an identical broadcast within a minute is refused) and the punch in the other window or on another student.

**Rolling back:**
- **Step 2:** `migrate goto 29` (with the old code running). Nothing is lost.
- **Step 3:** redeploy the previous code; it runs on 30 and reads notifications by `parent_id`.
- **Step 5:** `migrate goto 30`. The columns, index and trigger come back empty; the new code is unaffected.
- **Never run `goto 29` while the new code runs** (its notification inserts would fail): redeploy the old code first.
- **Not recoverable without the backup** once 000031 has run: every notification phone number and original phone number, and every `students.fcm_token`. A later rollback of 000030 fills each notification's phone with its parent's current number (an empty string when it has no parent), and 000029's rollback then restores no notification phone formats.

### Deploying migration 000032 (manual attendance)

000032 only adds `attendance_logs.source` (default `device`) and `attendance_logs.note`, makes `device_sn` nullable for manual rows only (a check keeps it required on every device row), and adds the checks. The old code runs unchanged on it: its punches are stored as `device` rows and nothing it reads changes. **Production** is built from an empty database through all migrations in one go; this order is for Staging.

**Order on Staging** (Staging is at 31; with `BASE_URL=https://futurekids-staging.up.railway.app`):

1. **Back up:** `pg_dump "$DATABASE_URL" > backup.sql`.
2. **Migrate** while the old code runs:
   ```bash
   migrate -path db/migrations -database "$DATABASE_URL" version    # expect 31
   migrate -path db/migrations -database "$DATABASE_URL" goto 32
   ```
3. **Deploy the new code.** It needs 32: on 31 the daily report and the Excel export answer 500.
4. **Verify** on a seeded fake student. Run it on a school day after 08:00 Baghdad (the default check-in time must not be in the future).
   - **Preflight, read-only:**
     ```sql
     -- 1004 is active, has a parent (note the phone) and existed before today
     SELECT s.id, s.is_active, s.parent_id, p.phone_number, s.created_at::date FROM students s LEFT JOIN parents p ON p.id = s.parent_id WHERE s.rfid_tag = '1004';
     -- today is a school day: dow 0-4 and closures 0
     SELECT EXTRACT(DOW FROM d) AS dow, (SELECT COUNT(*) FROM school_closures WHERE start_date <= d AND (end_date IS NULL OR end_date >= d)) AS closures
     FROM (SELECT (now() AT TIME ZONE 'Asia/Baghdad')::date AS d) t;
     -- 1004 has no punch today in the check-in window (06:30:00-09:30:59)
     SELECT a.id, a.check_time, a.source FROM attendance_logs a JOIN students s ON s.id = a.student_id
     WHERE s.rfid_tag = '1004' AND a.check_time >= (now() AT TIME ZONE 'Asia/Baghdad')::date ORDER BY a.check_time;
     ```
     If 1004 is inactive or has no parent, or already has a check-in today, use another seeded student (`rfid_tag` 1005 to 1050) that passes, and its parent's number below. If today is not a school day, wait for one.
   - **Admin token and the student id:**
     ```bash
     ADMIN=$(curl -s -X POST "$BASE_URL/api/admin/login" -H 'Content-Type: application/json' -d "{\"username\":\"$ADMIN_USERNAME\",\"password\":\"$ADMIN_PASSWORD\"}" | jq -r .data.token)
     SID=$(psql "$DATABASE_URL" -X -At -c "SELECT id FROM students WHERE rfid_tag = '1004'")
     ```
   - **(a) Preview, then create the check-in.** The preview gives `Preview only`, `id` null and the student's name, grade and section; the create gives `Manual attendance recorded`, `time` `08:00:00` and `notified` true.
     ```bash
     curl -s -X POST "$BASE_URL/api/admin/attendance/manual" -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
       -d "{\"student_id\":$SID,\"type\":\"in\",\"note\":\"Staging check\",\"dry_run\":true}"
     MID=$(curl -s -X POST "$BASE_URL/api/admin/attendance/manual" -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
       -d "{\"student_id\":$SID,\"type\":\"in\",\"note\":\"Staging check\"}" | jq -r .data.id)
     ```
   - **(b) The report marks it:** `check_in_time` `08:00 AM`, `check_in_source` `manual`, and `manual_punches[0].id` equal to `$MID`.
     ```bash
     curl -s "$BASE_URL/api/admin/attendance" -H "Authorization: Bearer $ADMIN" | jq ".data[] | select(.student_id == $SID)"
     ```
     ```sql
     SELECT id, check_time, source, device_sn, note FROM attendance_logs WHERE id = <MID>;   -- manual, device_sn NULL, Staging check
     ```
   - **(c) The parent got the notification** `إشعار دخول` / `تم تسجيل دخول الطالب … الساعة 08:00` (log in with the parent's number from the preflight and the seeded PIN):
     ```bash
     PARENT=$(curl -s -X POST "$BASE_URL/api/mobile/login" -H 'Content-Type: application/json' -d '{"phone":"+9647000002001","pin":"Sd7!Kx4p#Wm9qT2h"}' | jq -r .data.token)
     curl -s "$BASE_URL/api/mobile/notifications" -H "Authorization: Bearer $PARENT" | jq '.data[0]'
     ```
   - **(d) The 409 cases:** the same check-in again names the manual punch, and deleting a device punch is refused.
     ```bash
     curl -s -X POST "$BASE_URL/api/admin/attendance/manual" -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' -d "{\"student_id\":$SID,\"type\":\"in\",\"time\":\"09:00\"}"   # 409 … (manual)
     DEV=$(psql "$DATABASE_URL" -X -At -c "SELECT id FROM attendance_logs WHERE source = 'device' ORDER BY id DESC LIMIT 1")
     curl -s -X DELETE "$BASE_URL/api/admin/attendance/manual?id=$DEV" -H "Authorization: Bearer $ADMIN"   # 409 Only manual entries can be deleted …
     ```
   - **(e) Undo:** the delete gives `Manual attendance deleted`, and the report shows `check_in_source` null and empty `manual_punches` again. The parent's notification stays.
     ```bash
     curl -s -X DELETE "$BASE_URL/api/admin/attendance/manual?id=$MID" -H "Authorization: Bearer $ADMIN"
     curl -s "$BASE_URL/api/admin/attendance" -H "Authorization: Bearer $ADMIN" | jq ".data[] | select(.student_id == $SID)"
     ```
   - **Left on Staging afterwards:** the parent's check-in notification and the `attendance.manual_create`, `attendance.manual_delete` and login entries in the activity log. The manual row itself is deleted in step (e).

**Rolling back:**
- **The code:** redeploy the previous version. It runs on 32 and shows manual entries as normal punches, but cannot create or delete them.
- **The migration:** `migrate goto 31`, only after the code is rolled back (the new code needs `source`). It **deletes every manual entry** and its note: those students show absent again on those days. Only the backup has them. Notifications already sent stay.

### Deploying the rfid_tag rule (API 1.18.0, no migration)

`rfid_tag` must now be the student's ZKTeco user ID: required on create, 1 to 14 ASCII digits without a leading 0; an update may resend the stored value unchanged. A taken tag answers 409 with `suggested_rfid_tag`. There is no migration: deploy the code. **Rollback:** redeploy the previous version; nothing in the database needs undoing.

**Check on Staging after the deploy** (with `BASE_URL=https://futurekids-staging.up.railway.app` and an admin token in `$ADMIN`, as in the 000032 section). The test IDs 99990001 to 99990003 never clash with real students.

1. **Preflight, read-only:**
   ```sql
   -- the test parent exists; note its full_name and send exactly that name below
   SELECT id, full_name FROM parents WHERE phone_number = '+9647709990001';
   -- the test IDs are free: no rows
   SELECT id, rfid_tag FROM students WHERE rfid_tag IN ('99990001', '99990002', '99990003');
   ```
   If the parent doesn't exist, the create also needs a `parent_pin` (16+ characters with a letter, a digit and a symbol) and creates the parent; delete it in step 4 too. If a test ID is taken, stop and pick another free range.
2. **Run the four requests** (`PNAME` is the stored name, so the parent's name is unchanged):
   ```bash
   PNAME=$(psql "$DATABASE_URL" -X -At -c "SELECT full_name FROM parents WHERE phone_number = '+9647709990001'")
   body() { jq -nc --arg p "$PNAME" --arg t "$1" '{name:"طالب اختبار الرقم",parent_name:$p,parent_phone:"+9647709990001"} + (if $t == "-" then {} else {rfid_tag:$t} end)'; }
   # (a) valid create: 200, data.rfid_tag "99990001"
   curl -s -X POST "$BASE_URL/api/admin/students" -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' -d "$(body 99990001)"
   # (b) duplicate: 409, the unchanged message and "suggested_rfid_tag":"99990002"
   curl -s -X POST "$BASE_URL/api/admin/students" -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' -d "$(body 99990001)"
   # (c) Arabic-Indic digits: 400 "rfid_tag must be digits only (1 to 14, not starting with 0): …"
   curl -s -X POST "$BASE_URL/api/admin/students" -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' -d "$(body ٣٣)"
   # (d) no rfid_tag: 400 "rfid_tag is required"
   curl -s -X POST "$BASE_URL/api/admin/students" -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' -d "$(body -)"
   ```
3. **Confirm nothing else was written:** only the student from (a) exists.
   ```sql
   SELECT id, full_name, rfid_tag FROM students WHERE rfid_tag LIKE '9999000_';
   ```
4. **Clean up by SQL.** Deactivating through the API would keep the row, and its ID would stay taken.
   ```sql
   DELETE FROM students WHERE rfid_tag = '99990001';
   ```
5. **Left behind:** the four `student.create` entries in the activity log (results `success`, `rejected`, `rejected`, `rejected`). The parent row is unchanged because the same name was sent; a parent created in step 1 remains until you delete it.

### Deploying migration 000033 (API 1.19.0: account deletion and push switch)

000033 only adds `parents.push_enabled` (`NOT NULL DEFAULT TRUE`), so every existing parent keeps receiving pushes. The old code runs unchanged on it: it never reads the column and its inserts get the default. Account deletion (`POST /api/mobile/account/delete`) needs no migration. **Production** is built from an empty database through all migrations in one go; this order is for Staging.

**Order on Staging** (Staging is at 32; with `BASE_URL=https://futurekids-staging.up.railway.app`):

1. **Back up:** `pg_dump "$DATABASE_URL" > backup.sql`.
2. **Migrate** while the old code runs:
   ```bash
   migrate -path db/migrations -database "$DATABASE_URL" version    # expect 32
   migrate -path db/migrations -database "$DATABASE_URL" goto 33
   ```
3. **Deploy the new code.** It needs 33: on 32 the device-token registration and the settings endpoints answer 500.
4. **Verify** with a throwaway parent, because a deletion cannot be undone. The test number +9647709990011 and the test ID 99990011 never clash with real data.
   - **Preflight, read-only (both return no rows):**
     ```sql
     SELECT id FROM parents WHERE phone_number = '+9647709990011';
     SELECT id FROM students WHERE rfid_tag = '99990011';
     ```
   - **Create the throwaway parent and log in** (`$ADMIN` is an admin token, as in the 000032 section):
     ```bash
     curl -s -X POST "$BASE_URL/api/admin/students" -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
       -d '{"name":"طالب اختبار الحذف","parent_name":"ولي أمر اختبار","parent_phone":"+9647709990011","parent_pin":"Tz8#mK3q!Vw6pR2d","rfid_tag":"99990011"}'
     PARENT=$(curl -s -X POST "$BASE_URL/api/mobile/login" -H 'Content-Type: application/json' -d '{"phone":"+9647709990011","pin":"Tz8#mK3q!Vw6pR2d"}' | jq -r .data.token)
     ```
   - **(a) The push switch:** GET gives `push_enabled` true; off gives false; a device registration while off gives `"registered":false`; on, then the registration gives `"registered":true`.
     ```bash
     curl -s "$BASE_URL/api/mobile/notification-settings" -H "Authorization: Bearer $PARENT"
     curl -s -X PUT "$BASE_URL/api/mobile/notification-settings" -H "Authorization: Bearer $PARENT" -H 'Content-Type: application/json' -d '{"push_enabled":false}'
     curl -s -X PUT "$BASE_URL/api/mobile/device-token" -H "Authorization: Bearer $PARENT" -H 'Content-Type: application/json' -d '{"token":"staging-check-token-0000000001"}'
     curl -s -X PUT "$BASE_URL/api/mobile/notification-settings" -H "Authorization: Bearer $PARENT" -H 'Content-Type: application/json' -d '{"push_enabled":true}'
     curl -s -X PUT "$BASE_URL/api/mobile/device-token" -H "Authorization: Bearer $PARENT" -H 'Content-Type: application/json' -d '{"token":"staging-check-token-0000000001"}'
     ```
   - **(b) Deletion:** a wrong PIN gives 400 `Incorrect PIN` and the token keeps working; the right PIN gives `Account deleted`; the same token then gets 401.
     ```bash
     curl -s -X POST "$BASE_URL/api/mobile/account/delete" -H "Authorization: Bearer $PARENT" -H 'Content-Type: application/json' -d '{"pin":"Wrong#Pin123456789"}'
     curl -s -X POST "$BASE_URL/api/mobile/account/delete" -H "Authorization: Bearer $PARENT" -H 'Content-Type: application/json' -d '{"pin":"Tz8#mK3q!Vw6pR2d"}'
     curl -s "$BASE_URL/api/mobile/notification-settings" -H "Authorization: Bearer $PARENT"
     ```
   - **(c) The database:** the parent and its device token are gone, the student is kept without a parent, and the activity log names the student only.
     ```sql
     SELECT COUNT(*) FROM parents WHERE phone_number = '+9647709990011';                    -- 0
     SELECT COUNT(*) FROM device_tokens WHERE token = 'staging-check-token-0000000001';     -- 0
     SELECT id, parent_id FROM students WHERE rfid_tag = '99990011';                        -- parent_id NULL
     SELECT actor, action, ip, target_type, target_id, detail FROM admin_activity_log
     WHERE action = 'parent.account_delete' ORDER BY id DESC LIMIT 1;                       -- parent, NULL ip, student, its id, طالب اختبار الحذف
     ```
   - **Clean up by SQL:** `DELETE FROM students WHERE rfid_tag = '99990011';`
   - **Left on Staging afterwards:** the `student.create` and `parent.account_delete` entries in the activity log. The wrong PIN in (b) counted one failed attempt for the test number, which expires after 15 minutes.

**Rolling back:**
- **The code:** redeploy the previous version. It runs on 33 and ignores `push_enabled`, so parents who turned pushes off get them again once their app registers. Deleted accounts stay deleted: only the backup has them.
- **The migration:** `migrate goto 32`, only after the code is rolled back. It loses every parent's push choice.

### Deploying migration 000034 (API 1.20.0: teachers)

000034 only adds the tables `teachers` and `teacher_attendance_logs`. The old code runs unchanged on it: nothing it reads changes. The server never runs migrations itself: run them with the `migrate` CLI **before** deploying the code, as below.

**What changes with the code:**
- **New admin routes:** `/api/admin/teachers`, `/api/admin/teachers/attendance`, `/api/admin/teachers/specializations`.
- **Device punches:** a punch whose PIN matches no student but a teacher (`rfid_tag` 1 to 99) is now stored as a teacher punch, never notified.
- **Unknown PINs:** the line for a PIN matching neither is now INFO instead of WARN.
- **Breaking for student writes:** a new or changed student `rfid_tag` under 100 is refused (1 to 99 is the staff range). A student already holding one keeps it and may resend it unchanged.

**Order** (Staging is at 33; the same on Production):
1. **Preflight, read-only:** students holding a tag in the staff range.
   ```sql
   SELECT id, full_name, rfid_tag, is_active FROM students WHERE rfid_tag ~ '^[1-9][0-9]?$' ORDER BY rfid_tag::int;
   ```
   Those students keep working. Each such tag can't be given to a teacher (409 `rfid_tag is already used by a student`) until the student is moved to a tag of 100 or more, and the device enrolment changed to match.
2. **Back up:** `pg_dump "$DATABASE_URL" > backup.sql`.
3. **Migrate** while the old code runs:
   ```bash
   migrate -path db/migrations -database "$DATABASE_URL" version    # expect 33
   migrate -path db/migrations -database "$DATABASE_URL" goto 34
   ```
4. **Deploy the new code.** On 33 it would answer 500 on the teachers routes, and 503 to every device batch containing a PIN no student has. So the order matters.
5. **Smoke checks** (`$ADMIN` is an admin token, as in the 000032 section): a teacher create gives 200; the list and the attendance give 200; a student create with `rfid_tag` `50` gives 400.

**Rolling back:**
- **The code:** redeploy the previous version. It runs on 34; teacher punches are then skipped again as unknown PINs, as before.
- **The migration:** `migrate goto 33`, only after the code is rolled back. It deletes every teacher and teacher punch.

### Deploying migration 000035 (API 1.21.0: tuition)

000035 only adds the tables `tuition_fees`, `tuition_accounts` and `tuition_payments`, and the setting `academic_year` = `2026-2027` (kept if it already exists). The old code runs unchanged on it. The server never runs migrations itself: run them with the `migrate` CLI **before** deploying the code. No fee, discount or payment is seeded: the school manager enters them on Production.

**What changes with the code:**
- **New admin routes:** `/api/admin/tuition`, `/api/admin/tuition/student`, `/api/admin/tuition/fees`, `/api/admin/tuition/discount`, `/api/admin/tuition/payments`, `/api/admin/tuition/payments/void`.
- **New parent route:** `GET /api/mobile/tuition` (the remaining balance per child, current year only).
- **`PUT /api/admin/settings` with `academic_year`** is validated (two consecutive years) and switches the year (see below).
- **No silent default:** if the `academic_year` row is missing or invalid, every tuition route answers 500 and logs an ERROR (`the academic_year setting is missing or invalid`). To fix it, re-insert the row: `INSERT INTO settings (setting_key, setting_value) VALUES ('academic_year', '2026-2027');`.

**How balances stay fixed:** a student's account for a year (`tuition_accounts`) freezes the grade, its fee and the discount. It is created on the student's first discount or payment, and for every active student whose grade has a fee when the academic year changes. Before that, the current year follows the grade's fee live. Payments are never edited or deleted, only voided. `remaining` = fee − discount − non-voided payments, computed on every read.
- **A mistyped grade fee after accounts exist:** save the grade's fee again with `apply_to_existing: true`. It re-prices every current-year account whose student is now in that grade, or refuses (409, nothing changed) when one would go negative. The same call fixes a student moved to another grade after paying: correct the student's grade first.
- **The year switch:** change the academic year **first**, then the students' grades. Promoting first freezes the old year's unpaid balances at the old-year fee of the **new** grade (visible only in the admin view of the old year).

**Order** (Staging is at 34; the same on Production):
1. **Preflight, read-only:** the grades that will need a fee, and whether an `academic_year` setting already exists (the migration keeps an existing value; it must look like `2026-2027`).
   ```sql
   SELECT btrim(grade) AS grade, count(*) FROM students WHERE is_active = true GROUP BY 1 ORDER BY 1;
   SELECT setting_key, setting_value FROM settings WHERE setting_key = 'academic_year';
   ```
   The second query should return no row before 000035.
2. **Back up:** `pg_dump "$DATABASE_URL" > backup.sql`.
3. **Migrate** while the old code runs:
   ```bash
   migrate -path db/migrations -database "$DATABASE_URL" version    # expect 34
   migrate -path db/migrations -database "$DATABASE_URL" goto 35
   ```
4. **Deploy the new code.** On 34 it would answer 500 on every tuition route. So the order matters.
5. **Smoke checks** (`$ADMIN` is an admin token): `GET /api/admin/settings` lists `academic_year`; `GET /api/admin/tuition/fees` and `GET /api/admin/tuition` give 200. Don't record a test payment on Production: payments can't be deleted.

**Rolling back:**
- **The code:** redeploy the previous version. It runs on 35; the old settings endpoint then stores `academic_year` without validation, so don't change it meanwhile.
- **The migration:** `migrate goto 34`, only after the code is rolled back. It deletes every fee, discount and payment, and the `academic_year` setting.

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
- **Notifications:** the attendance history creates none, because the server notifies only device punches dated today and the history never includes today. Afterwards, the noon absence job notifies the parent of every seeded student without a punch, each school day.
- **If a run stops midway,** it prints what was done. Re-running `--apply` is safe; if the history of students created in the stopped run was not sent, add `--resume-history`.

### Settings

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | — (`DB_URL` is a local fallback) | PostgreSQL URL. Don't add a `timezone` parameter: the server pins every connection to Asia/Baghdad |
| `JWT_SECRET` | — (required) | Signs every token. In production it must be at least 32 characters with at least 12 distinct ones and no placeholder word (`secret`, `changeme`, `password`, …); generate it with `openssl rand -base64 48`, different per environment |
| `FIREBASE_CREDENTIALS_JSON` / `FIREBASE_CREDENTIALS_PATH` | — / `firebase-credentials.json` | Firebase service-account key (the JSON wins over the file). Checked at startup: valid JSON, `type` `service_account`, non-empty `project_id` and `client_email`, a PEM `private_key`. Use a separate key per environment |
| `RAILWAY_ENVIRONMENT_NAME` | set by Railway | `production` turns the startup checks (the two rows above, `DATABASE_URL`, and no admin account accepting the password seeded by migration 000008) into a refusal to start that lists every problem; any other environment only logs them as WARN |
| `TRUSTED_PROXY_CIDRS` | empty (also on Railway) | Proxies whose client-IP headers are believed (comma-separated CIDRs or addresses). Unset on Railway: a WARN at startup, an ERROR in production (runbook step 5) |
| `SHUTDOWN_TIMEOUT` | `25s` | Deadline for the whole graceful shutdown |
| `ABSENCE_CRON_SCHEDULE` | `0 12 * * 0-4` | When absence notifications go out (Asia/Baghdad; Sunday–Thursday at noon). The job does nothing on a day that is not a school day (weekend, holiday or pause) |
| `FAKE_TODAY` | empty | Tests only: the date (`YYYY-MM-DD`) the server takes as today, at the real time of day. Refused on Railway; the server logs a WARN when it is set |
| `RAILWAY_DEPLOYMENT_DRAINING_SECONDS` | Railway: `0` | Time Railway waits after `SIGTERM`; set to `30` |

