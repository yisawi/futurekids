# Future Kids Backend - Architectural Scan & Analysis Report

**Date Generated**: September 14, 2026
**Target**: Future Kids Backend (Go + PostgreSQL)

---

## 1. Database Schema
The database architecture has been significantly improved through recent normalization efforts. The current schema state (based on migrations `000001` through `000009`) includes:

### Core Tables
*   **`parents`**: Newly extracted table for guardian data (`id`, `full_name`, `phone_number` [UNIQUE], `pin_code`).
*   **`students`**: Contains student details (`id`, `full_name`, `rfid_tag` [UNIQUE], `parent_id` [FK to parents], `grade`, `section`, `fcm_token`, `avatar_url`).
    *   *Recent Refactoring*: Columns `parent_phone`, `parent_pin`, and `parent_name` were dropped. The relationship is now strictly maintained via `parent_id`.
*   **`devices`**: Tracks biometric/RFID attendance hardware (`serial_number` [PK], `location_name`, `is_active`, `last_sync`).
*   **`attendance_logs`**: Records student punches (`id`, `student_id`, `device_sn`, `check_time`, `status`). Enforces `UNIQUE(student_id, check_time)` to prevent duplicate punches.
*   **`student_leaves`**: Manages excused absences (`id`, `student_id`, `leave_date`, `notes`). Enforces `UNIQUE(student_id, leave_date)`.
*   **`weekly_schedules`**: Stores class schedules by `grade` and `section`.
*   **`notifications`**: Stores parent notifications. Includes an index on `parent_phone` (`idx_notifications_phone`) for fast retrieval.
*   **`banners`**: Manages UI banners (`title`, `image_url`, `action_link`, `is_active`).
*   **`admins`**: Stores admin credentials (`username`, `password_hash`).

---

## 2. API Endpoints
The API is divided into three main domains, each protected by specific middleware.

### Admin Domain
**Middleware**: `AdminMiddleware` (Validates JWT, enforces `role == "admin"`)
*   `POST /api/admin/login` *(Unprotected)*
*   `GET /api/admin/dashboard`
*   `GET, POST, PUT, DELETE /api/admin/students`
*   `POST /api/admin/leaves`
*   `GET /api/admin/attendance`
*   `GET /api/admin/export/excel`

### Mobile (Parent) Domain
**Middleware**: `AuthMiddleware` (Validates JWT, enforces `role == "parent"`, injects `parent_id` into Context)
*   `POST /api/mobile/login` (Alias: `/api/v1/auth/login`) *(Unprotected)*
*   `GET /api/mobile/attendance/today`
*   `GET /api/mobile/attendance/monthly`
*   `GET /api/mobile/attendance/summary`
*   `GET /api/mobile/students`
*   `GET /api/mobile/schedule`
*   `GET /api/mobile/notifications`
*   `GET /api/mobile/banners`

### Hardware Domain
**Middleware**: `DeviceAuthMiddleware` (Checks `?SN=` against active `devices`), `HardwareLoggerMiddleware` (Logs metrics).
*   `POST /api/attendance/push` (Accepts ADMS raw text payload)
*   `POST /api/attendance/push/json` (Accepts structured JSON payload)

---

## 3. Security & Data Isolation
The mobile application employs a strict, context-based data isolation strategy to ensure parents can only view their own children's data.

1.  **JWT Issuance**: Upon login (`MobileLoginHandler`), a JWT is issued containing the `parent_id` (from the `parents` table) and `role: "parent"`.
2.  **Context Injection**: The `AuthMiddleware` intercepts incoming requests, parses the JWT, and extracts the `parent_id`. It injects this ID into the `http.Request` Context using a typed key (`ParentIDKey`).
3.  **Data Isolation (No URL Params)**: Handlers (e.g., `MobileAttendanceSummaryHandler`) do *not* rely on `student_id` query parameters. Instead, they retrieve the `parent_id` from the Context and use it directly in SQL joins (`WHERE s.parent_id = $1`). This structural barrier makes it impossible for a parent to manipulate URL parameters to access unauthorized records.

---

## 4. Concurrency & Performance
Heavy optimizations have been implemented to handle the "Morning Rush," where hundreds of students clock in simultaneously.

### Hardware Push (`POST /api/attendance/push/json`)
*   **Atomic Queries**: Uses a Common Table Expression (CTE) to lookup the student by `rfid_tag` and insert the `attendance_logs` record in a single SQL execution. This eliminates "check-then-act" race conditions entirely in Go.
*   **Idempotency via `ON CONFLICT`**: The query uses `ON CONFLICT (student_id, check_time) DO NOTHING`. If a device spams the server due to network lag, the database safely ignores duplicates while the Go server returns a fast `200 OK` ("Ignored or Duplicate") to prevent the device from hanging.
*   **Memory Protection**: Uses `http.MaxBytesReader` to cap payload sizes at 1MB, preventing memory exhaustion (DDoS protection).

### Connection Pooling (`main.go`)
To prevent the Postgres database from being overwhelmed by simultaneous hardware connections:
*   `SetMaxOpenConns(25)`: Caps the maximum number of active database connections.
*   `SetMaxIdleConns(25)`: Keeps warm connections in the pool for fast reuse.
*   `SetConnMaxLifetime(15 * time.Minute)`: Reaps stale connections to maintain pool health.

---

## 5. Background Jobs (Absence Engine)
The system uses the `robfig/cron/v3` package to process automated absences and notifications.

*   **Timezone Precision**: The engine is strictly bound to `Asia/Baghdad`. It executes daily at exactly 12:00 PM (`0 12 * * *`).
*   **Dynamic Exclusion Logic**: The SQL query identifies absent students by excluding both present *and* excused students simultaneously:
    ```sql
    SELECT id, full_name, parent_phone, fcm_token 
    FROM students 
    WHERE id NOT IN (SELECT student_id FROM attendance_logs WHERE DATE(check_time) = $1) 
    AND id NOT IN (SELECT student_id FROM student_leaves WHERE leave_date = $1)
    ```
*   This approach avoids complex loops and negative counts, safely fetching only those who truly missed the day without a valid excuse, preparing them for FCM push notifications.
