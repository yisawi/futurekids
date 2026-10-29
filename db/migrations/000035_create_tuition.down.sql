-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: tables tuition_payments, tuition_accounts and tuition_fees, and the academic_year setting.
-- Running this down-migration against the current main branch will break:
--   internal/handlers/tuition.go — every /api/admin/tuition route and GET /api/mobile/tuition answer
--     500, and so does PUT /api/admin/settings for the academic_year key (the setting is missing).
--   internal/handlers/admin.go — AdminSettingsHandler hands the academic_year key to the year switch,
--     which writes tuition_accounts.
-- Data loss: every grade fee, discount and payment (voided ones included), and the academic year.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

DROP TABLE IF EXISTS tuition_payments;
DROP TABLE IF EXISTS tuition_accounts;
DROP TABLE IF EXISTS tuition_fees;
DELETE FROM settings WHERE setting_key = 'academic_year';
