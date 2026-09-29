-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: table admins.
-- Running this down-migration against the current main branch will break:
--   internal/handlers/admin.go — AdminLoginHandler reads admins; every admin login fails.
--   db/scripts/rotate_admin_password.sql — updates admins.
-- Data loss: every admin account and the rotated password hash. Re-applying 000008 re-seeds the public
--   admin/admin123 password, so rotate it again afterwards (db/scripts/README.md).
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

DROP TABLE IF EXISTS admins;
