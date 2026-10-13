-- Manual attendance: a punch the admin enters for a student whose fingerprint failed is a normal
-- attendance_logs row with source 'manual', no device_sn and an optional note, so every report
-- treats it exactly like a device punch. Device rows keep their serial number (and its foreign
-- key): device_sn is NULL exactly when source is 'manual'. Old application code keeps running on
-- this schema: its inserts get source 'device' and nothing it reads changes.

ALTER TABLE attendance_logs ADD COLUMN IF NOT EXISTS source VARCHAR(10) NOT NULL DEFAULT 'device';
ALTER TABLE attendance_logs ADD COLUMN IF NOT EXISTS note VARCHAR(200);
ALTER TABLE attendance_logs ALTER COLUMN device_sn DROP NOT NULL;

ALTER TABLE attendance_logs DROP CONSTRAINT IF EXISTS attendance_logs_source_check;
ALTER TABLE attendance_logs ADD CONSTRAINT attendance_logs_source_check CHECK (source IN ('device', 'manual'));
ALTER TABLE attendance_logs DROP CONSTRAINT IF EXISTS attendance_logs_device_sn_check;
ALTER TABLE attendance_logs ADD CONSTRAINT attendance_logs_device_sn_check CHECK ((source = 'device') = (device_sn IS NOT NULL));
ALTER TABLE attendance_logs DROP CONSTRAINT IF EXISTS attendance_logs_note_check;
ALTER TABLE attendance_logs ADD CONSTRAINT attendance_logs_note_check CHECK (source = 'manual' OR note IS NULL);
