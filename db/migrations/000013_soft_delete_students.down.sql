-- ⚠ APP-COMPATIBILITY WARNING
-- Running this down-migration against the current main branch will break:
--   internal/handlers/admin.go — AdminStudentsHandler (GET branch) filters on
--   is_active; AdminDailyAttendanceHandler filters on is_active.
--   internal/handlers/mobile.go — MobileStudentsHandler filters on is_active.
--   internal/cron/absent_job.go — ProcessDailyAbsences filters on is_active.
-- Do not run this migrate-down without first reverting or updating those
-- files to match the pre-migration schema.

ALTER TABLE students DROP COLUMN is_active;
