-- APP-COMPATIBILITY: none — it only re-adds, empty and nullable, notifications.parent_phone (with idx_notifications_phone and 000023's trigger notifications_fill_parent_id), notifications.parent_phone_original and students.fcm_token, which the current Go code does not use.
-- Data loss: none by the rollback itself, but it cannot restore what the up-migration dropped:
--   every notification's phone number and original phone number and every students.fcm_token
--   come back NULL; only a backup has them. 000030's down-migration then fills parent_phone with
--   each parent's current number, and 000029's down-migration restores no notification formats.

ALTER TABLE notifications ADD COLUMN IF NOT EXISTS parent_phone VARCHAR(20);
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS parent_phone_original VARCHAR(20);
CREATE INDEX IF NOT EXISTS idx_notifications_phone ON notifications(parent_phone);
ALTER TABLE students ADD COLUMN IF NOT EXISTS fcm_token TEXT;

CREATE OR REPLACE FUNCTION notifications_fill_parent_id() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.parent_id IS NULL AND NEW.parent_phone IS NOT NULL THEN
        SELECT id INTO NEW.parent_id FROM parents WHERE phone_number = NEW.parent_phone;
    END IF;
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS notifications_fill_parent_id ON notifications;
CREATE TRIGGER notifications_fill_parent_id BEFORE INSERT ON notifications
FOR EACH ROW EXECUTE FUNCTION notifications_fill_parent_id();
