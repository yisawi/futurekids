-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: table banner_images.
-- Running this down-migration against the current main branch will break:
--   internal/handlers/banners.go — the admin banner routes and both banner image routes read
--     and write it.
--   internal/handlers/mobile.go — GetActiveBannersHandler joins it to build image_url.
-- Data loss: every picture uploaded from the admin dashboard. The banners themselves stay, but
--   their image_url is empty, so this rollback deactivates every banner that had an uploaded
--   picture (is_active = false) to keep the parent app from showing blank banners.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

DO $$
BEGIN
    IF to_regclass('banner_images') IS NOT NULL THEN
        UPDATE banners SET is_active = false WHERE id IN (SELECT banner_id FROM banner_images);
    END IF;
END $$;

DROP TABLE IF EXISTS banner_images;
