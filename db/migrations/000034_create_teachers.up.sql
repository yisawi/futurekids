-- Teachers, managed by the admin, and their fingerprint punches. A teacher's rfid_tag is the user ID
-- on the ZKTeco device, in the staff range 1 to 99 (students use 100 and up), and a device punch whose
-- PIN matches no student but a teacher is stored in teacher_attendance_logs. Teacher punches are never
-- notified and never count in student attendance. status is descriptive only (there is no delete).
-- Old application code keeps running on this schema: it only adds tables, which nothing old reads.

CREATE TABLE IF NOT EXISTS teachers (
    id SERIAL PRIMARY KEY,
    first_name TEXT NOT NULL CHECK (char_length(first_name) BETWEEN 1 AND 100),
    father_name TEXT NOT NULL CHECK (char_length(father_name) BETWEEN 1 AND 100),
    grandfather_name TEXT NOT NULL CHECK (char_length(grandfather_name) BETWEEN 1 AND 100),
    phone VARCHAR(20) NOT NULL CONSTRAINT teachers_phone_key UNIQUE CHECK (phone ~ '^\+9647[0-9]{9}$'),
    gender TEXT NOT NULL CHECK (gender IN ('male', 'female')),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'on_leave', 'transferred', 'retired')),
    specialization TEXT CHECK (char_length(specialization) BETWEEN 1 AND 100),
    notes TEXT CHECK (char_length(notes) BETWEEN 1 AND 500),
    rfid_tag VARCHAR(2) CONSTRAINT teachers_rfid_tag_key UNIQUE CHECK (rfid_tag ~ '^[1-9][0-9]?$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS teacher_attendance_logs (
    id BIGSERIAL PRIMARY KEY,
    teacher_id INT NOT NULL REFERENCES teachers(id),
    device_sn VARCHAR(50) NOT NULL REFERENCES devices(serial_number),
    check_time TIMESTAMP NOT NULL,
    sync_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT unique_teacher_punch UNIQUE (teacher_id, check_time)
);

CREATE INDEX IF NOT EXISTS idx_teacher_attendance_check_time ON teacher_attendance_logs (check_time);
