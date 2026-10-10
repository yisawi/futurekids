-- Renames the admin-to-parent notification feature from "announcement" to "broadcast", so it is not
-- confused with the banners the school calls announcements. A pure rename: no row, value or
-- foreign key changes, and every id keeps counting from the same sequence. Each step only runs
-- when the old name exists and the new one does not, so a partly renamed database finishes cleanly.

DO $$
BEGIN
    IF to_regclass('public.announcements') IS NOT NULL AND to_regclass('public.broadcasts') IS NULL THEN
        ALTER TABLE announcements RENAME TO broadcasts;
    END IF;
    IF to_regclass('public.announcements_id_seq') IS NOT NULL AND to_regclass('public.broadcasts_id_seq') IS NULL THEN
        ALTER SEQUENCE announcements_id_seq RENAME TO broadcasts_id_seq;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.broadcasts') AND conname = 'announcements_pkey') THEN
        ALTER TABLE broadcasts RENAME CONSTRAINT announcements_pkey TO broadcasts_pkey;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.broadcasts') AND conname = 'announcements_audience_fields') THEN
        ALTER TABLE broadcasts RENAME CONSTRAINT announcements_audience_fields TO broadcasts_audience_fields;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.broadcasts') AND conname = 'announcements_audience_grade_check') THEN
        ALTER TABLE broadcasts RENAME CONSTRAINT announcements_audience_grade_check TO broadcasts_audience_grade_check;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.broadcasts') AND conname = 'announcements_audience_parent_id_fkey') THEN
        ALTER TABLE broadcasts RENAME CONSTRAINT announcements_audience_parent_id_fkey TO broadcasts_audience_parent_id_fkey;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.broadcasts') AND conname = 'announcements_audience_section_check') THEN
        ALTER TABLE broadcasts RENAME CONSTRAINT announcements_audience_section_check TO broadcasts_audience_section_check;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.broadcasts') AND conname = 'announcements_audience_type_check') THEN
        ALTER TABLE broadcasts RENAME CONSTRAINT announcements_audience_type_check TO broadcasts_audience_type_check;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.broadcasts') AND conname = 'announcements_body_check') THEN
        ALTER TABLE broadcasts RENAME CONSTRAINT announcements_body_check TO broadcasts_body_check;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.broadcasts') AND conname = 'announcements_recipient_count_check') THEN
        ALTER TABLE broadcasts RENAME CONSTRAINT announcements_recipient_count_check TO broadcasts_recipient_count_check;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.broadcasts') AND conname = 'announcements_title_check') THEN
        ALTER TABLE broadcasts RENAME CONSTRAINT announcements_title_check TO broadcasts_title_check;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'notifications' AND column_name = 'announcement_id') THEN
        ALTER TABLE notifications RENAME COLUMN announcement_id TO broadcast_id;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'notifications'::regclass AND conname = 'notifications_announcement_id_fkey') THEN
        ALTER TABLE notifications RENAME CONSTRAINT notifications_announcement_id_fkey TO notifications_broadcast_id_fkey;
    END IF;
END $$;

ALTER INDEX IF EXISTS idx_notifications_announcement_id RENAME TO idx_notifications_broadcast_id;
