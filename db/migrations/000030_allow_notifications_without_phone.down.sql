-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: the NULL allowance of notifications.parent_phone (NOT NULL again).
-- Running this down-migration against the current main branch will break:
--   internal/notify/notify.go — SaveNotificationHistory inserts without parent_phone, so punch
--     (internal/handlers/hardware.go) and absence (internal/cron/absent_job.go) notifications
--     are missing from the parents' history.
--   internal/notify/broadcast.go — CreateBroadcast inserts without parent_phone, so sending a
--     broadcast (internal/handlers/broadcasts.go) and registering a closure while parents exist
--     (internal/handlers/holidays.go) answer 500.
-- Data loss: none. Rows stored without a phone number get their parent's current number (not
--   the number at the time they were sent); rows without a parent get an empty string.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

UPDATE notifications n
SET parent_phone = p.phone_number
FROM parents p
WHERE n.parent_phone IS NULL AND n.parent_id = p.id;

UPDATE notifications SET parent_phone = '' WHERE parent_phone IS NULL;

ALTER TABLE notifications ALTER COLUMN parent_phone SET NOT NULL;
