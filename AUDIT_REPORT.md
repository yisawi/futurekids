# FutureKids Backend Audit (against RULES.md)

Audit date: 2026-09-25. Line numbers reflect the code after fixes C1–C6.

**Critical issues C1–C6 are resolved:**

| ID | Issue | Status |
|---|---|---|
| C1 | `/api/mobile/attendance/summary` always returned 500 (`generate_series` in `WHERE`) | Fixed |
| C2 | Admin student upsert reset the parent's PIN to `1234` and overwrote their name | Fixed |
| C3 | Every punch (spam, dead-zone, check-out) sent an "attendance" push | Fixed: first check-in and first check-out only, with separate messages |
| C4 | Absence cron ran on Friday and Saturday | Fixed: `0 12 * * 0-4` |
| C5 | The `000015` down-migration failed with "cannot change return type" | Fixed: `DROP FUNCTION` before `CREATE` |
| C6 | Default `admin` / `admin123` credentials | Procedure added: [db/scripts/README.md](db/scripts/README.md) |

This report lists the remaining **15 warnings**, **17 notices**, nice-to-have improvements, and the conclusion.

---

## WARNINGS

### W1 — Nullable columns are scanned into non-nullable Go types
- **Files and columns:**
  - [mobile.go:356](internal/handlers/mobile.go#L356) and [mobile.go:373](internal/handlers/mobile.go#L373): `weekly_schedules.teacher_name`
  - [admin.go:595-606](internal/handlers/admin.go#L595-L606): `devices.location_name` and `devices.is_active`
  - [hardware.go:157](internal/handlers/hardware.go#L157) and [middleware.go:53](internal/handlers/middleware.go#L53): `devices.is_active`
  - [mobile.go:444](internal/handlers/mobile.go#L444): `notifications.is_read` and `notifications.created_at`
- **Rule:** checklist item on Postgres↔Go type safety
- **Current:** a NULL value makes `Scan` fail with "cannot scan NULL into *string/bool". Schedule periods without a teacher, and devices without a location, silently disappear because the error is followed by `continue`. A device whose `is_active` is NULL has every punch ACKed and dropped by `ADMSHandler`. Separately, `DevicePayload.IsActive` ([admin.go:588](internal/handlers/admin.go#L588)) defaults to `false` when omitted, so a device registered without that field is silently disabled.
- **Expected:** add a new migration that sets `NOT NULL` with defaults on these columns (as `000016` did), or use `COALESCE` in the queries. Require `is_active` explicitly on device POST.

### W2 — Timezone depends on the database session
- **File:** [mobile.go:190](internal/handlers/mobile.go#L190), [mobile.go:279](internal/handlers/mobile.go#L279), [mobile.go:466](internal/handlers/mobile.go#L466)
- **Rule:** §3 (the database is the source of truth, so results must not depend on the environment)
- **Current:** `CURRENT_DATE` and `CURRENT_TIMESTAMP` use the session's timezone. The local Postgres runs `Asia/Baghdad`; Railway's defaults to UTC. On Railway, between 00:00 and 03:00 Baghdad time, today is missing from the monthly views. Locally, `notifications.created_at` holds Baghdad wall-clock time but is formatted as RFC3339 with `Z`, so Flutter shows it 3 hours off.
- **Expected:** pass Go's Baghdad `today` as a query parameter instead of `CURRENT_DATE`, and pin the timezone for the whole database with `ALTER DATABASE ... SET timezone = 'Asia/Baghdad'` or in the DSN.

### W3 — Gaps at the edges of the time windows
- **File:** [000015_add_check_out_time.up.sql:26](db/migrations/000015_add_check_out_time.up.sql#L26), [000015_add_check_out_time.up.sql:34](db/migrations/000015_add_check_out_time.up.sql#L34)
- **Rule:** §5
- **Current:** `<= p_date + '9h30m'` excludes 09:30:01–09:30:59, which is neither in the check-in window nor in the §5 dead zone. A punch at 09:30:30 returns **Absent** (confirmed in Postgres), and that student then gets the noon absence alert. The same gap exists at 13:30. A student with only a check-out punch comes back as `Absent` with `last_check = 12:00 PM`, which contradicts itself.
- **Expected:** use `< p_date + INTERVAL '9 hours 31 minutes'` and `< ... '13 hours 31 minutes'` in a new migration. Decide what status a check-out-only day should have.

### W4 — Code and OpenAPI spec disagree
- **File:** [types.go:9-10](internal/handlers/types.go#L9-L10), [admin.go:39-40](internal/handlers/admin.go#L39-L40), [admin.go:153-164](internal/handlers/admin.go#L153-L164), [admin.go:261-263](internal/handlers/admin.go#L261-L263)
- **Rule:** §4 (strict adherence to the OpenAPI spec)
- **Current:**
  - (a) `DailyAttendanceDTO` uses `omitempty`, so Absent students have no `check_in_time` or `check_out_time` keys. The spec declares `check_out_time` as nullable and `check_in_time` as a non-null string, so Flutter models generated from the spec will break.
  - (b) `StudentPayload` accepts `grade` and `section`, but the spec doesn't define them and the GET response doesn't return them. A GET→PUT round trip therefore wipes them to `''`, which also breaks the schedule join.
  - (c) A PUT without `rfid_tag` generates a new `admin-…` tag, which disconnects the student from their ZKTeco PIN.
- **Expected:** drop `omitempty` and use `*string` for the nullable fields. Add `grade` and `section` to the spec and to the GET response. On PUT, keep the existing `rfid_tag` when it isn't sent.

### W5 — The ADMS alias endpoint can return non-200 responses
- **File:** [main.go:101](cmd/api/main.go#L101)
- **Rule:** §5 (hardware endpoints must always return 200 `OK`)
- **Current:** `/api/attendance/push` runs `ADMSHandler` behind `DeviceAuthMiddleware`, which returns JSON 401, 403 or 500. A ZKTeco device pointed at this URL would retry forever. The spec documents those codes, so the spec conflicts with §5. `ADMSHandler` already authenticates the device, so the middleware also runs the same device query twice.
- **Expected:** remove the alias, or don't wrap it in `DeviceAuthMiddleware`.

### W6 — Unbounded request bodies and no server timeouts
- **File:** [hardware.go:204](internal/handlers/hardware.go#L204), [zkteco_handler.go:52](internal/handlers/zkteco_handler.go#L52), [main.go:145](cmd/api/main.go#L145)
- **Rule:** none (security)
- **Current:** the public, unauthenticated `/iclock/cdata` runs `io.ReadAll` with no size limit. `http.ListenAndServe` has no read or write timeouts, so slow clients can tie up connections. JSON handlers other than the hardware one also lack `MaxBytesReader`.
- **Expected:** wrap bodies in `http.MaxBytesReader` (still returning OK on overflow for ADMS). Use an `http.Server{ReadHeaderTimeout, ReadTimeout, WriteTimeout}`.

### W7 — The JSON push endpoint trusts the device serial in the request body
- **File:** [hardware.go:392](internal/handlers/hardware.go#L392)
- **Rule:** none (correctness)
- **Current:** the insert uses `req.DeviceSN` from the body, not the `SN` query parameter the middleware authenticated. `PushTime` isn't validated. Either a bad timestamp or an unknown serial (foreign-key violation) returns 500, and the device retries that punch forever.
- **Expected:** use the authenticated `SN`, parse `PushTime` in Go, and return 400 on bad input.

### W8 — Unchecked type assertion in the auth middleware
- **File:** [middleware.go:135](internal/handlers/middleware.go#L135)
- **Rule:** none (correctness)
- **Current:** `claims["parent_id"].(float64)` panics on a validly signed token that has no `parent_id`, such as tokens issued before migration `000009` (they are valid for 30 days).
- **Expected:** use the comma-ok form and return 401 if the claim is missing.

### W9 — Logins have no brute-force protection
- **File:** [mobile.go:478-514](internal/handlers/mobile.go#L478-L514), [admin.go:52](internal/handlers/admin.go#L52)
- **Rule:** none (security)
- **Current:** a 4-digit PIN has 10,000 possibilities, there is no rate limit, and most parents will have the default `1234`.
- **Expected:** add an in-memory per-phone or per-IP limiter using `sync.Map` or `x/time/rate`. This fits §2, since it needs no Redis.

### W10 — Inconsistent error handling and logging
- **Rule:** checklist (error-handling consistency)
- **Current:**
  - `rows.Err()` isn't checked after the loops at [admin.go:408](internal/handlers/admin.go#L408), [admin.go:508](internal/handlers/admin.go#L508), [admin.go:554](internal/handlers/admin.go#L554), [admin.go:606](internal/handlers/admin.go#L606), or [mobile.go:101](internal/handlers/mobile.go#L101), [219](internal/handlers/mobile.go#L219), [299](internal/handlers/mobile.go#L299), [373](internal/handlers/mobile.go#L373), [416](internal/handlers/mobile.go#L416), [462](internal/handlers/mobile.go#L462), [561](internal/handlers/mobile.go#L561). Scan errors hit `continue` without being logged, so partial data is returned as 200 `success`.
  - Many 500 paths don't log the error at all ([admin.go:74](internal/handlers/admin.go#L74), [admin.go:137](internal/handlers/admin.go#L137), and all of mobile.go except banners and login).
  - `log` and `slog` are mixed ([absent_job.go](internal/cron/absent_job.go), [notify.go:47](internal/notify/notify.go#L47), [hardware.go:343](internal/handlers/hardware.go#L343), [hardware.go:354](internal/handlers/hardware.go#L354)).
  - `"[DEBUG]"` messages are logged at Info level ([hardware.go:194](internal/handlers/hardware.go#L194), [hardware.go:333](internal/handlers/hardware.go#L333)).
- **Expected:** log with `slog` everywhere, check `rows.Err()` after every loop, and log before every 500.

### W11 — Tests wipe the dev database by default, and e2e coverage has gaps
- **File:** [e2e_test.sh:7](tests/e2e_test.sh#L7), [e2e_test.sh:75](tests/e2e_test.sh#L75), [attendance_time_test.go:18-20](tests/attendance_time_test.go#L18-L20)
- **Rule:** §6
- **Current:** both the script and `go test ./tests/...` default to the real `future_kids` database and `TRUNCATE` it, including `settings`. If `TEST_DATABASE_URL` points at Railway, they wipe production. The e2e script doesn't cover summary, monthly, schedule, notifications, leaves, student PUT/DELETE, or PIN preservation. That is how C1 and W1 went unnoticed.
- **Expected:** default to a `future_kids_test` database, and refuse to run when the host isn't localhost. Add the missing endpoints to the e2e script.

### W12 — `sslmode=disable` is forced, and pool settings are duplicated
- **File:** [db.go:60-73](internal/database/db.go#L60-L73), [main.go:39-43](cmd/api/main.go#L39-L43)
- **Rule:** none (security and configuration)
- **Current:** when the DSN has no `sslmode`, the code adds `sslmode=disable`. pgx's default, `prefer`, already works without TLS, so this change only removes TLS, including on Railway's public proxy URL. The pool is configured twice, and main.go's 15-minute lifetime silently overrides db.go's 5 minutes.
- **Expected:** delete `ensureSSLMode`, and configure the pool in one place.

### W13 — An empty parent phone is accepted
- **File:** [admin.go:193-237](internal/handlers/admin.go#L193-L237)
- **Rule:** §4 (the spec marks `parent_phone` as required)
- **Current:** every student created without a phone is attached to a single shared parent row with `phone_number = ''`.
- **Expected:** return 400 when `name`, `parent_phone` or `parent_name` is empty.

### W14 — Missing app-compatibility warnings on down-migrations
- **File:** down-migrations for `000001`, `000003`, `000004`, `000005`, `000007`, `000008`, `000009`, `000010`, `000014`
- **Rule:** §3 ("MUST include a `-- ⚠ APP-COMPATIBILITY WARNING`")
- **Current:** each of these drops something current Go code uses:
  - `notifications`: notify.go, mobile.go
  - `avatar_url`: mobile.go
  - `student_leaves` and `attendance_logs.status`: admin.go, hardware.go
  - `banners`: mobile.go
  - `admins`: admin.go
  - `parents` and `parent_id`: every handler
  - `settings`: both settings handlers
  - `get_student_status`: all attendance queries

  Only `000002`, `000013` and `000015` have the warning.
- **Expected:** add the warning block to each of these files.

### W15 — Parents created without a PIN get an unusable plaintext default (seeding hazard)
- **File:** [000009_normalize_parents.up.sql:8](db/migrations/000009_normalize_parents.up.sql#L8)
- **Rule:** none (seeding)
- **Current:** `pin_code DEFAULT '1234'` stores plaintext. Login compares with bcrypt, so any parent seeded through SQL without an explicit bcrypt `pin_code` can never log in (confirmed in Postgres).
- **Expected:** add a migration to drop the default. Seed through the admin API, or insert bcrypt hashes.

---

## NOTICES

| # | File | Issue | Expected |
|---|---|---|---|
| N1 | [main.go:80-83](cmd/api/main.go#L80-L83) | `/health` writes raw `w.Write` text (§4 wording) | Use `respondJSON`, or document it as an exception |
| N2 | [main.go:49](cmd/api/main.go#L49) | Firebase env var read outside [config.go](internal/config/config.go) | Add it to `Config` |
| N3 | [migrate-pins/main.go:9](cmd/migrate-pins/main.go#L9) | Uses `lib/pq`; the app uses pgx, so there are two drivers in go.mod | Switch to `pgx/v5/stdlib` and drop `lib/pq` |
| N4 | [hardware.go:387](internal/handlers/hardware.go#L387) | `attendance_logs.status` is never read; values are `'Present'` vs the default `'present'` | Drop the column or stop writing it |
| N5 | [zkteco_handler.go:51-91](internal/handlers/zkteco_handler.go#L51-L91) | POST branch is dead code (the route is GET-only) | Remove it |
| N6 | [hardware.go:228-352](internal/handlers/hardware.go#L228-L352) | 4 queries per punch; `StudentID` is converted string↔int for no reason ([250](internal/handlers/hardware.go#L250), [267](internal/handlers/hardware.go#L267)) | Merge into fewer CTE queries, like the JSON handler, and use an `int` field |
| N7 | [admin.go:48](internal/handlers/admin.go#L48), [admin.go:104](internal/handlers/admin.go#L104), [mobile.go:76](internal/handlers/mobile.go#L76) | `time.LoadLocation` runs on every request and its error is ignored | Load it once at startup |
| N8 | [notify.go:56](internal/notify/notify.go#L56), [mobile.go:524](internal/handlers/mobile.go#L524) | FCM token is logged in plaintext; the token is stored per student, so only the parent's most recent device gets pushes; dead tokens are never cleared | Redact the token; handle `messaging.IsRegistrationTokenNotRegistered` |
| N9 | [admin.go:482](internal/handlers/admin.go#L482), [admin.go:449](internal/handlers/admin.go#L449) | School name hardcoded; no check-out column; `JOIN parents` drops parentless students, unlike the dashboard and daily report | Store the name in a setting; use a LEFT JOIN |
| N10 | [mobile.go:360](internal/handlers/mobile.go#L360) | `ORDER BY day_of_week` sorts text alphabetically, not by weekday | Order with an explicit weekday `CASE` |
| N11 | [mobile.go:224](internal/handlers/mobile.go#L224), [admin.go:44](internal/handlers/admin.go#L44) | Dead `TotalAbsent < 0` check; `date` and `month` params aren't validated, so bad input gives 500 instead of 400 | Validate with `time.Parse` |
| N12 | [mobile.go:444-450](internal/handlers/mobile.go#L444-L450) | Notifications have no LIMIT or pagination; history is keyed by phone, so a phone change loses it | Add a LIMIT; key by `parent_id` later |
| N13 | [jwt.go:33](internal/auth/jwt.go#L33) | Comment says "short-lived" but the token lasts 7 days | Fix the comment or the TTL |
| N14 | [mobile.go:543](internal/handlers/mobile.go#L543) | The public settings endpoint returns every key | Allow-list the keys it returns |
| N15 | [mobile.go:505](internal/handlers/mobile.go#L505) | Database errors on login are returned as 401, which hides outages | Log them and return 500 |
| N16 | [main.go:139-150](cmd/api/main.go#L139-L150) | No graceful shutdown; the `defer`s never run | Use `signal.NotifyContext` and `srv.Shutdown` |
| N17 | [000003 up:11](db/migrations/000003_create_notifications_table.up.sql#L11) | `CREATE INDEX` lacks `IF NOT EXISTS` while the table has it; `parent_name` has no creating migration (schema drift, already noted in the file) | Make both idempotent the same way |

---

## NICE-TO-HAVE IMPROVEMENTS

The following are non-blocking:
- **W3:** close the one-minute gaps at the edges of the windows.
- **W5–W10:** the ADMS alias, body limits and timeouts, JSON push validation, the JWT assertion, login rate-limiting, and error handling and logging.
- **W12:** remove forced `sslmode=disable` and duplicate pool settings.
- **W14:** app-compatibility warnings (comments only).
- **N1–N17:** all notices.

The ones worth doing first:
1. **W9:** rate-limit parent and admin logins.
2. **W10:** check `rows.Err()`, and log with `slog` before every 500.
3. **W11:** extend e2e coverage to every mobile endpoint, and to PIN preservation and notification counts (the C1–C3 regressions).

Still blocking before production seeding and Flutter testing:
1. **W15 + W13:** don't rely on the `pin_code` default; reject empty phone numbers.
2. **W4(a):** fix DTO nullability (`omitempty` → `*string`) before generating Flutter models.
3. **W1:** set `NOT NULL` on `devices.is_active`, `devices.location_name` and `weekly_schedules.teacher_name`, or wrap them in `COALESCE`.
4. **W2:** pin the database timezone to `Asia/Baghdad` on Railway.
5. **W11:** point the tests at a dedicated test database before running them near production.

---

## CONCLUSION

**Not yet.** All six critical defects (C1–C6) are fixed and verified: e2e passes 50/50, and targeted live checks passed. Before production seeding and Flutter testing, the five blocking items listed under Nice-to-Have Improvements still need fixing: W15/W13, W4(a), W1, W2 and W11.
