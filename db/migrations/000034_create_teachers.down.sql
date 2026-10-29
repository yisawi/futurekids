-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: tables teachers and teacher_attendance_logs.
-- Running this down-migration against the current main branch will break:
--   internal/handlers/teachers.go — every /api/admin/teachers route answers 500.
--   internal/handlers/hardware.go — the ADMS handler looks up device PINs that match no student in
--     teachers; that query fails, so every batch containing such a PIN is rolled back and answered
--     503, and the device keeps resending it.
-- Data loss: every teacher and every teacher punch.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

DROP TABLE IF EXISTS teacher_attendance_logs;
DROP TABLE IF EXISTS teachers;
