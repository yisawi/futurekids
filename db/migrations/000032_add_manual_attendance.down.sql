-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: columns attendance_logs.source and attendance_logs.note (and their checks), every
--   manual attendance row, and the NULL allowance of attendance_logs.device_sn.
-- Running this down-migration against the current main branch will break:
--   internal/handlers/manual_attendance.go — POST and DELETE /api/admin/attendance/manual write
--     and delete manual rows; both answer 500.
--   internal/handlers/admin.go — the daily report and the Excel export read attendance_logs.source
--     and answer 500.
-- Data loss: every manual attendance entry and its note. Those students show absent again on
--   those days; notifications already sent for them stay in the parents' history.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

DELETE FROM attendance_logs WHERE device_sn IS NULL;

ALTER TABLE attendance_logs DROP CONSTRAINT IF EXISTS attendance_logs_note_check;
ALTER TABLE attendance_logs DROP CONSTRAINT IF EXISTS attendance_logs_device_sn_check;
ALTER TABLE attendance_logs DROP CONSTRAINT IF EXISTS attendance_logs_source_check;
ALTER TABLE attendance_logs ALTER COLUMN device_sn SET NOT NULL;
ALTER TABLE attendance_logs DROP COLUMN IF EXISTS note;
ALTER TABLE attendance_logs DROP COLUMN IF EXISTS source;
