-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: table notifications.
-- Running this down-migration against the current main branch will break:
--   internal/notify/notify.go — SaveNotificationHistory inserts into notifications.
--   internal/handlers/mobile.go — MobileNotificationsHandler reads notifications.
--   internal/handlers/hardware.go — notifyPunch saves check-in/out history via notify.
--   internal/cron/absent_job.go — ProcessDailyAbsences saves absence history via notify.
-- Data loss: the entire notification history shown to parents.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

DROP INDEX IF EXISTS idx_notifications_phone;
DROP TABLE IF EXISTS notifications;
