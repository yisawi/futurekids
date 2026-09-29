-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: table student_leaves and column attendance_logs.status (re-added, unused, by 000019 down).
-- Running this down-migration against the current main branch will break:
--   internal/handlers/admin.go — AdminCreateLeaveHandler inserts into student_leaves.
--   get_student_status (000014+) reads student_leaves, so every attendance query also fails:
--     internal/handlers/admin.go (dashboard, daily attendance, Excel export),
--     internal/handlers/mobile.go (today, summary, monthly), internal/cron/absent_job.go.
-- Data loss: every leave record (excused days become Absent) and the attendance_logs.status values.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

ALTER TABLE attendance_logs DROP COLUMN IF EXISTS status;
DROP TABLE IF EXISTS student_leaves;
