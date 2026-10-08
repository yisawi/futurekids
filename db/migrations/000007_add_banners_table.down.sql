-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: table banners.
-- Running this down-migration against the current main branch will break:
--   internal/handlers/mobile.go — GetActiveBannersHandler reads banners.
--   internal/handlers/banners.go — the admin banner routes and both banner image routes.
-- Data loss: every banner.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

DROP TABLE IF EXISTS banners;
