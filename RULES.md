# Future Kids — Architectural Constitution (RULES)

This document establishes the strict architectural guidelines for the Future Kids Go backend. These rules are non-negotiable and designed to prevent architectural drift, technical debt, and framework bloat during the MVP phase.

## 1. Core Architecture
- **Go Monolith:** The system is a strict Go monolithic application.
- **Standard Library Routing:** We use the Go 1.22+ standard library `net/http` enhanced routing (`ServeMux`). **Do not introduce** any external web frameworks like Gin, Fiber, or Echo.
- **Simplicity:** The architecture relies on straightforward handler functions, minimal middleware (Auth/Admin/Device Logging), and standard library concurrency.

- Package rule: do not create a new package for under ~100 lines or a single consumer.
Put it in an existing package (handlers, server, config) unless two or more packages need it.

- Read-only review: list every package under internal/ with its line count and number of importers.
Recommend which ones should be merged into an existing package. Do not change anything.

## 2. Strict Constraints & Anti-Patterns (CRITICAL)
- **No Infrastructure Bloat:** The system MUST remain strictly a **Go + PostgreSQL** stack.
- **Prohibited Technologies:** 
  - **No Redis** or external caching layers.
  - **No RabbitMQ**, Kafka, or other message brokers.
  - Do not introduce new infrastructure dependencies. Background jobs (like Daily Absences) are handled via in-memory cron (`github.com/robfig/cron`) and Go routines.

## 3. Database Standards
- **Relational SSOT:** PostgreSQL is the single source of truth.
- **Raw SQL Migrations:** All database schema changes must be written as raw SQL files in the `db/migrations/` directory using the `golang-migrate/migrate` tool conventions (up/down files).
- **NO Heavy ORMs:** The use of GORM, Ent, or other heavy ORMs is strictly prohibited. Database interactions must use the standard `database/sql` package with raw SQL queries.
- **Database-Level Logic:** Where appropriate, complex business logic (e.g., attendance status calculation) is encapsulated in robust PostgreSQL functions (like `get_student_status`) to guarantee consistency across all endpoints.
- **App-Compatibility Warnings on Down-Migrations:** Any down-migration that drops a column, table, or changes a function signature that the current Go codebase actively reads or writes MUST include a `-- ⚠ APP-COMPATIBILITY WARNING` comment block at the top of the file, naming the specific Go files/handlers that depend on it. This does not make the migration reversible at the application layer — it only ensures nobody runs `migrate down` past that point without knowing it will break the running server. A down-migration that affects no current Go code starts instead with `-- APP-COMPATIBILITY: none — <reason>`. Both forms state the data a rollback loses; see `db/migrations/README.md`.

## 4. API & JSON Contracts
- **OpenAPI 3.0 Strictness:** The API must strictly adhere to the `future_kids_api.yaml` specification. Every endpoint, method, request payload, and response format must perfectly match this document.
- **Centralized HTTP Responses:** All handlers MUST use the shared helpers in `internal/handlers/response.go`:
  - `respondJSON(w, status, data)` for successful responses.
  - `respondError(w, status, message)` for errors, guaranteeing a unified `{"status": "error", "message": "..."}` shape.
  - **No raw JSON literals** or manual `w.Write()` error handling.
- **Routes and error envelope:** Register every non-hardware route with its method (`"GET /api/..."`) and serve the mux through `handlers.RouteErrors`, so an unknown path gets a JSON 404 and a wrong method a JSON 405 with `Allow`, decided before authentication. Hardware routes and `/health` keep their registrations and plain-text answers (§5).
- **Phone numbers:** A phone number is an Iraqi mobile number stored, compared and returned only in the canonical form `+9647XXXXXXXXX`. Every input passes through `internal/phone.Normalize` (accepting `07…`, `+964…`, `00964…`, `964…`, spaces, dashes, dots, parentheses, direction marks and Arabic-Indic or Persian digits); invalid numbers get 400 from admin endpoints and the unknown-number 401 from login. SQL that converts stored numbers must match it (see `db/migrations/000028_normalize_phone_numbers.up.sql`).
- **Push recipients:** Push notifications go to the device tokens in `device_tokens`, which belong to a parent (at most 10 per parent, least recently seen evicted). Every push about a child goes to all of the child's parent's devices, and a broadcast goes to every device of each recipient parent (`notify.PushBroadcast`, after the commit, in batches of at most 500). Tokens are registered through `PUT /api/mobile/device-token` (or login's `fcm_token`, through the same code path) and removed with `DELETE /api/mobile/device-token`, when FCM reports them unregistered, when the parent's PIN changes, and with the parent. `students.fcm_token` is legacy: new code must not read or write it.
- **Notification history:** A notification belongs to the parent it was sent to (`notifications.parent_id`) and is listed, counted and marked read by that id, never by phone number. Every insert goes through `notify.SaveNotificationHistory`, or, for a broadcast the admin sends, through `notify.CreateBroadcast`, which writes all recipients in one statement inside the caller's transaction and links each row to it (`broadcast_id`); both also write `parent_phone` until a later migration drops that column. `is_read` is the parent's read state, set only by `PUT /api/mobile/notifications/read?id=` and `PUT /api/mobile/notifications/read-all`; the list reports the total in `unread_count`.
- **Terminology:** "banner" is the image shown on the parent home screen (dashboard label إعلان); "broadcast" is a notification an admin sends to parents (dashboard label إشعار). The word "announcement" is not used in code, routes or docs.
- **Unified DTOs:** The `DailyAttendanceDTO` struct in `internal/handlers/types.go` is the absolute SSOT for serializing attendance data. It is shared across both Admin and Mobile handlers to prevent DRY violations.
- **Flutter task notes:** Every task that changes the API contract or its documented behaviour updates `future_kids_api.yaml`, regenerates `flutter_api.yaml` and updates `docs/FLUTTER_HANDOFF.md`, and also adds `flutter/<task-id>/README.md` and `flutter/<task-id>/openapi-fragment.yaml` (the task id in lowercase). That folder is local only and git-ignored (`/flutter/`); no test, lint or build may read it. A revision of an existing task updates that task's folder in place instead of creating a new one.

## 5. Business & Hardware Logic
- **ZKTeco ADMS Integration:** Hardware endpoints (`/iclock/cdata`) must ALWAYS respond with HTTP 200 plain-text `OK`. Any other status code (or JSON error) causes physical ZKTeco devices to enter an infinite retry loop.
  - **Single exception — transient database failure:** if the database is unavailable (connection loss, timeout, shutdown, deadlock) while storing an `ATTLOG` batch, the batch is rolled back in full and the device receives HTTP `503`, so it keeps the punches and resends them once the database recovers. Bad data never gets `503`: unknown PINs, malformed lines, records the database rejects, oversized bodies, and unknown or disabled devices are always ACKed with `200 OK` (and logged), because resending them can never succeed.
- **Strict Time-Window Attendance Logic:** The backend aggressively filters hardware punches to infer check-in/out and prevent hardware spam:
  - **Morning Check-In Window:** `06:30 AM` to `09:30 AM`. (Captures the EARLIEST punch).
  - **Afternoon Check-Out Window:** `11:30 AM` to `01:30 PM`. (Captures the EARLIEST punch).
  - **Dead Zones:** Punches between `09:31 AM - 11:29 AM` and after `01:31 PM` are completely ignored (Joke/Spam punches).
  - **Idempotency:** Duplicate punches within a valid window must be gracefully ignored; they must not overwrite the first valid punch.
- **Attendance start date:** A student is counted from the Asia/Baghdad date of `students.created_at`, inclusive (NULL counts every day). Every query that lists or counts a student on a past date applies `studentExistedOnSQL` (`internal/handlers/attendance.go`); punches and leaves before that date are ignored.
- **School days:** A date is a school day when it is Sunday to Thursday and no school closure (`school_closures`: a holiday or a pause) covers it; the day type is, in this precedence, `weekend` (Friday, Saturday), `pause`, `holiday`, otherwise `school`. Closed days are not school days: they are not counted or listed in attendance, the noon absence job does nothing, and punches on them are stored without notifications. The rule exists once: `handlers.DayOn` for one date and `schoolDaySQL` for set queries (`internal/handlers/attendance.go`); "today" is `handlers.Clock`.
- **Today before the check-in window closes:** Until `handlers.CheckInWindowEnd` (09:31 Asia/Baghdad, the same boundary as the check-in window in `get_student_status`), the parent monthly records and summary leave out today's record when it is `Absent`. The today endpoint, the admin daily report, the dashboard and the noon absence job are unchanged; clients show "not arrived yet".

## 6. Testing Mandate
- **End-to-End Verification:** The `tests/e2e_test.sh` script is the ultimate validator of the system's integrity.
- **Strict Requirement:** The `e2e_test.sh` script **MUST pass 100% locally** before any git commit or deployment to Railway. 
- It guarantees that the entire data flow—from the raw hardware ADMS push, through the SQL time-window function, up to the validated `DailyAttendanceDTO` JSON responses—is working flawlessly.

