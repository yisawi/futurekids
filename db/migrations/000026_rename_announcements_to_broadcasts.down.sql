-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: the names table broadcasts, sequence broadcasts_id_seq, column
--   notifications.broadcast_id and index idx_notifications_broadcast_id (and the broadcasts_*
--   constraints); they are renamed back to their announcement names.
-- Running this down-migration against the current main branch will break:
--   internal/notify/broadcast.go — CreateBroadcast, RecentDuplicate and PushBroadcast read and
--     write broadcasts and notifications.broadcast_id.
--   internal/handlers/broadcasts.go — POST and GET /api/admin/broadcasts (the send and the sent
--     log with its read counts).
-- Data loss: none. Every row, value, link and sequence position is kept; only names change.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

DO $$
BEGIN
    IF to_regclass('public.broadcasts') IS NOT NULL AND to_regclass('public.announcements') IS NULL THEN
        ALTER TABLE broadcasts RENAME TO announcements;
    END IF;
    IF to_regclass('public.broadcasts_id_seq') IS NOT NULL AND to_regclass('public.announcements_id_seq') IS NULL THEN
        ALTER SEQUENCE broadcasts_id_seq RENAME TO announcements_id_seq;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.announcements') AND conname = 'broadcasts_pkey') THEN
        ALTER TABLE announcements RENAME CONSTRAINT broadcasts_pkey TO announcements_pkey;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.announcements') AND conname = 'broadcasts_audience_fields') THEN
        ALTER TABLE announcements RENAME CONSTRAINT broadcasts_audience_fields TO announcements_audience_fields;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.announcements') AND conname = 'broadcasts_audience_grade_check') THEN
        ALTER TABLE announcements RENAME CONSTRAINT broadcasts_audience_grade_check TO announcements_audience_grade_check;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.announcements') AND conname = 'broadcasts_audience_parent_id_fkey') THEN
        ALTER TABLE announcements RENAME CONSTRAINT broadcasts_audience_parent_id_fkey TO announcements_audience_parent_id_fkey;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.announcements') AND conname = 'broadcasts_audience_section_check') THEN
        ALTER TABLE announcements RENAME CONSTRAINT broadcasts_audience_section_check TO announcements_audience_section_check;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.announcements') AND conname = 'broadcasts_audience_type_check') THEN
        ALTER TABLE announcements RENAME CONSTRAINT broadcasts_audience_type_check TO announcements_audience_type_check;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.announcements') AND conname = 'broadcasts_body_check') THEN
        ALTER TABLE announcements RENAME CONSTRAINT broadcasts_body_check TO announcements_body_check;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.announcements') AND conname = 'broadcasts_recipient_count_check') THEN
        ALTER TABLE announcements RENAME CONSTRAINT broadcasts_recipient_count_check TO announcements_recipient_count_check;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('public.announcements') AND conname = 'broadcasts_title_check') THEN
        ALTER TABLE announcements RENAME CONSTRAINT broadcasts_title_check TO announcements_title_check;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'notifications' AND column_name = 'broadcast_id') THEN
        ALTER TABLE notifications RENAME COLUMN broadcast_id TO announcement_id;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'notifications'::regclass AND conname = 'notifications_broadcast_id_fkey') THEN
        ALTER TABLE notifications RENAME CONSTRAINT notifications_broadcast_id_fkey TO notifications_announcement_id_fkey;
    END IF;
END $$;

ALTER INDEX IF EXISTS idx_notifications_broadcast_id RENAME TO idx_notifications_announcement_id;
