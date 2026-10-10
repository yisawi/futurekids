-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: column notifications.parent_id (with its indexes) and the trigger and function
--   notifications_fill_parent_id.
-- Running this down-migration against the current main branch will break:
--   internal/notify/notify.go — SaveNotificationHistory writes notifications.parent_id.
--   internal/handlers/mobile.go — MobileNotificationsHandler lists, counts and marks as read
--     by notifications.parent_id; every notification endpoint fails.
--   internal/handlers/hardware.go and internal/cron/absent_job.go — pass the parent id to
--     SaveNotificationHistory.
--   internal/notify/broadcast.go — CreateBroadcast writes notifications.parent_id and
--     PushBroadcast joins on it.
-- Data loss: which parent each notification belongs to. Old code matches history by
--   parent_phone again, so a parent whose phone number changed after the up-migration no
--   longer sees the history from before the change. Read state (is_read) is kept.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.
DROP TRIGGER IF EXISTS notifications_fill_parent_id ON notifications;
DROP FUNCTION IF EXISTS notifications_fill_parent_id();
DROP INDEX IF EXISTS idx_notifications_parent_unread;
DROP INDEX IF EXISTS idx_notifications_parent_id_id;
ALTER TABLE notifications DROP COLUMN IF EXISTS parent_id;
