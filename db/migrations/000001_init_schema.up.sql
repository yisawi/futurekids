CREATE TABLE students (
    id SERIAL PRIMARY KEY,
    full_name VARCHAR(100) NOT NULL,
    rfid_tag VARCHAR(50) UNIQUE NOT NULL,
    parent_phone VARCHAR(20),
    fcm_token TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE devices (
    serial_number VARCHAR(50) PRIMARY KEY,
    location_name VARCHAR(50),
    is_active BOOLEAN DEFAULT TRUE,
    last_sync TIMESTAMP
);

CREATE TABLE attendance_logs (
    id BIGSERIAL PRIMARY KEY,
    student_id INT REFERENCES students(id) ON DELETE CASCADE,
    device_sn VARCHAR(50) REFERENCES devices(serial_number),
    check_time TIMESTAMP NOT NULL,
    sync_time TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    log_type SMALLINT,
    CONSTRAINT unique_student_punch UNIQUE(student_id, check_time)
);