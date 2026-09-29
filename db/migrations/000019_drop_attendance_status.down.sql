-- APP-COMPATIBILITY: none — it re-adds attendance_logs.status, which the current Go code neither reads nor writes.
-- Data loss: none (the restored column is filled with its default, 'present').

ALTER TABLE attendance_logs ADD COLUMN IF NOT EXISTS status VARCHAR(20) DEFAULT 'present';
