-- Drops what only code from before 000022 (device tokens) and 000023 (notifications keyed by
-- parent) used: notifications.parent_phone with its index and 000023's trigger and function,
-- 000029's notifications.parent_phone_original, and students.fcm_token. No code from 000030 on
-- uses them. The down-migration cannot restore their values: back up first.

DROP TRIGGER IF EXISTS notifications_fill_parent_id ON notifications;
DROP FUNCTION IF EXISTS notifications_fill_parent_id();
DROP INDEX IF EXISTS idx_notifications_phone;
ALTER TABLE notifications DROP COLUMN IF EXISTS parent_phone_original;
ALTER TABLE notifications DROP COLUMN IF EXISTS parent_phone;
ALTER TABLE students DROP COLUMN IF EXISTS fcm_token;
