-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: table admin_activity_log.
-- Running this down-migration against the current main branch will break:
--   internal/handlers/activity.go — the activity wrapper on every admin route can no longer
--     record (each request still gets its normal answer; an ERROR is logged), and
--     GET /api/admin/activity-log answers 500.
--   internal/handlers/parent_account.go — parent account deletion writes its entry in the same
--     transaction, so POST /api/mobile/account/delete answers 500.
-- Data loss: the whole activity log (every recorded admin action, login attempt and export).
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

DROP TABLE IF EXISTS admin_activity_log;
