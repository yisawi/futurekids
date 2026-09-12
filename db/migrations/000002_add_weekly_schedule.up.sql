-- إضافة الصف والشعبة لجدول الطلاب الحالي
ALTER TABLE students ADD COLUMN IF NOT EXISTS grade VARCHAR(50);
ALTER TABLE students ADD COLUMN IF NOT EXISTS section VARCHAR(50);

-- إنشاء جدول الحصص الأسبوعية
CREATE TABLE IF NOT EXISTS weekly_schedules (
    id SERIAL PRIMARY KEY,
    grade VARCHAR(50) NOT NULL,
    section VARCHAR(50) NOT NULL,
    day_of_week VARCHAR(20) NOT NULL, -- الأحد، الإثنين، الثلاثاء...
    period_number INT NOT NULL,       -- 1, 2, 3...
    subject_name VARCHAR(100) NOT NULL,
    teacher_name VARCHAR(100),
    UNIQUE(grade, section, day_of_week, period_number)
);