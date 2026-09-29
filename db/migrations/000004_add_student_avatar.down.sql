-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: column students.avatar_url.
-- Running this down-migration against the current main branch will break:
--   internal/handlers/mobile.go — MobileStudentsHandler reads avatar_url.
-- Data loss: every student's avatar URL.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

ALTER TABLE students DROP COLUMN IF EXISTS avatar_url;
