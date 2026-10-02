-- Notification history belongs to a parent (parent_id), not to a phone number, so it survives
-- a change of the parent's number and is deleted with the parent (RULES.md §4).
-- Old application code keeps running on this schema: parent_id is nullable, parent_phone is
-- unchanged and still written, and until a later migration drops parent_phone the trigger
-- below fills parent_id from parent_phone for any row inserted without it (rows the old code
-- writes between this migration and the deploy), so no row is left unlinked.
-- Backfill: rows whose parent_phone matches a parent get that parent's id. Rows matching no
-- parent keep parent_id NULL: no parent could see them before and none can now; nothing is
-- deleted. Their number is reported as a NOTICE. Re-running the file changes nothing more.

ALTER TABLE notifications ADD COLUMN IF NOT EXISTS parent_id INT REFERENCES parents(id) ON DELETE CASCADE;

UPDATE notifications n
SET parent_id = p.id
FROM parents p
WHERE n.parent_id IS NULL AND n.parent_phone = p.phone_number;

CREATE INDEX IF NOT EXISTS idx_notifications_parent_id_id ON notifications (parent_id, id);
CREATE INDEX IF NOT EXISTS idx_notifications_parent_unread ON notifications (parent_id) WHERE is_read IS NOT TRUE;

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

DO $$
DECLARE
    unmatched BIGINT;
BEGIN
    SELECT COUNT(*) INTO unmatched FROM notifications WHERE parent_id IS NULL;
    RAISE NOTICE 'notifications matching no parent: % (kept with parent_id NULL; visible to no parent, as before)', unmatched;
END $$;
