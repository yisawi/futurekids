# Future Kids — Architectural Constitution (RULES)

This document establishes the strict architectural guidelines for the Future Kids Go backend. These rules are non-negotiable and designed to prevent architectural drift, technical debt, and framework bloat during the MVP phase.

## 1. Core Architecture
- **Go Monolith:** The system is a strict Go monolithic application.
- **Standard Library Routing:** We use the Go 1.22+ standard library `net/http` enhanced routing (`ServeMux`). **Do not introduce** any external web frameworks like Gin, Fiber, or Echo.
- **Simplicity:** The architecture relies on straightforward handler functions, minimal middleware (Auth/Admin/Device Logging), and standard library concurrency.

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

## 4. API & JSON Contracts
- **OpenAPI 3.0 Strictness:** The API must strictly adhere to the `future_kids_api.yaml` specification. Every endpoint, method, request payload, and response format must perfectly match this document.
- **Centralized HTTP Responses:** All handlers MUST use the shared helpers in `internal/handlers/response.go`:
  - `respondJSON(w, status, data)` for successful responses.
  - `respondError(w, status, message)` for errors, guaranteeing a unified `{"status": "error", "message": "..."}` shape.
  - **No raw JSON literals** or manual `w.Write()` error handling.
- **Unified DTOs:** The `DailyAttendanceDTO` struct in `internal/handlers/types.go` is the absolute SSOT for serializing attendance data. It is shared across both Admin and Mobile handlers to prevent DRY violations.

## 5. Business & Hardware Logic
- **ZKTeco ADMS Integration:** Hardware endpoints (`/iclock/cdata`) must ALWAYS respond with HTTP 200 plain-text `OK`. Any other status code (or JSON error) causes physical ZKTeco devices to enter an infinite retry loop.
- **Strict Time-Window Attendance Logic:** The backend aggressively filters hardware punches to infer check-in/out and prevent hardware spam:
  - **Morning Check-In Window:** `06:30 AM` to `09:30 AM`. (Captures the EARLIEST punch).
  - **Afternoon Check-Out Window:** `11:30 AM` to `01:30 PM`. (Captures the EARLIEST punch).
  - **Dead Zones:** Punches between `09:31 AM - 11:29 AM` and after `01:31 PM` are completely ignored (Joke/Spam punches).
  - **Idempotency:** Duplicate punches within a valid window must be gracefully ignored; they must not overwrite the first valid punch.

## 6. Testing Mandate
- **End-to-End Verification:** The `tests/e2e_test.sh` script is the ultimate validator of the system's integrity.
- **Strict Requirement:** The `e2e_test.sh` script **MUST pass 100% locally** before any git commit or deployment to Railway. 
- It guarantees that the entire data flow—from the raw hardware ADMS push, through the SQL time-window function, up to the validated `DailyAttendanceDTO` JSON responses—is working flawlessly.
