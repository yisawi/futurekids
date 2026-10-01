-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: columns parents.session_version and admins.session_version.
-- Running this down-migration against the current main branch will break:
--   internal/handlers/middleware.go — AuthMiddleware and AdminMiddleware read session_version
--     on every authenticated request, so every parent and admin request fails.
--   internal/handlers/mobile.go — MobileLoginHandler reads session_version to issue tokens.
--   internal/handlers/admin.go — AdminLoginHandler and the student upsert (PIN change) use it.
--   internal/auth/jwt.go — issues and reads the "sv" claim that is compared with it.
-- Data loss: the session versions; tokens issued before a PIN or password change would be
--   accepted again by code that does not check them.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.
ALTER TABLE parents DROP COLUMN IF EXISTS session_version;
ALTER TABLE admins DROP COLUMN IF EXISTS session_version;
