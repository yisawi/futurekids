-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: table announcements and column notifications.announcement_id (with its index).
-- Running this down-migration against the current main branch will break:
--   internal/notify/announcement.go — CreateAnnouncement, RecentDuplicate and PushAnnouncement
--     read and write announcements and notifications.announcement_id.
--   internal/handlers/announcements.go — POST and GET /api/admin/announcements (the send and the
--     sent log with its read counts).
-- Data loss: the whole sent log (every announcement's title, body, audience and recipient count).
--   The notifications parents received are kept, unread or read as they were, but lose their
--   link to the announcement, so read counts can no longer be computed.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

DROP INDEX IF EXISTS idx_notifications_announcement_id;
ALTER TABLE notifications DROP COLUMN IF EXISTS announcement_id;
DROP TABLE IF EXISTS announcements;
