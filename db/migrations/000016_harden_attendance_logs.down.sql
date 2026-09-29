-- APP-COMPATIBILITY: none — it only relaxes NOT NULL on attendance_logs.student_id and device_sn; the Go code always supplies both.
-- Data loss: none (integrity checks become weaker).

ALTER TABLE attendance_logs ALTER COLUMN student_id DROP NOT NULL;
ALTER TABLE attendance_logs ALTER COLUMN device_sn DROP NOT NULL;
