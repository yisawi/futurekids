-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: table settings.
-- Running this down-migration against the current main branch will break:
--   internal/handlers/admin.go — AdminSettingsHandler reads and writes settings.
--   internal/handlers/mobile.go — MobileSettingsHandler reads settings.
-- Data loss: every setting (for example whatsapp_number). Re-applying 000010 re-seeds only the placeholder.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

BEGIN;
DROP TABLE IF EXISTS settings;
COMMIT;
