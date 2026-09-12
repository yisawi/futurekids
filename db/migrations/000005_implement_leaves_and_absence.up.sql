-- 1. إنشاء جدول الإجازات (مجاز)
CREATE TABLE IF NOT EXISTS student_leaves (
    id SERIAL PRIMARY KEY,
    student_id INT NOT NULL REFERENCES students(id) ON DELETE CASCADE,
    leave_date DATE NOT NULL,
    notes TEXT, -- (اختياري: عذر طبي، إجازة زمنية) لا تظهر لولي الأمر
    UNIQUE(student_id, leave_date)
);

-- 2. تعديل جدول الحضور لدعم الحالات
ALTER TABLE attendance_logs ADD COLUMN IF NOT EXISTS status VARCHAR(20) DEFAULT 'present';