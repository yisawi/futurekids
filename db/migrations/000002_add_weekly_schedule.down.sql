-- ⚠ APP-COMPATIBILITY WARNING
-- Running this down-migration against the current main branch will break:
--   internal/handlers/admin.go — AdminStudentsHandler (POST branch) writes grade and
--   section on every student create; (PUT branch) updates grade and section on every
--   student update. Dropping these columns will cause both SQL statements to fail.
-- Do not run this migrate-down without first reverting or updating those
-- files to match the pre-migration schema.

DROP TABLE IF EXISTS weekly_schedules;
ALTER TABLE students DROP COLUMN IF EXISTS grade;
ALTER TABLE students DROP COLUMN IF EXISTS section;
