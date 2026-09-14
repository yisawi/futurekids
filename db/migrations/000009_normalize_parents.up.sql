BEGIN;

-- 1. إنشاء جدول أولياء الأمور
CREATE TABLE parents (
    id SERIAL PRIMARY KEY,
    full_name VARCHAR(255) NOT NULL,
    phone_number VARCHAR(20) UNIQUE NOT NULL,
    pin_code VARCHAR(255) NOT NULL DEFAULT '1234',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- 2. إضافة عمود الربط لجدول الطلاب
ALTER TABLE students ADD COLUMN parent_id INT REFERENCES parents(id) ON DELETE RESTRICT;

-- 3. نقل البيانات الحالية (Data Migration) للحفاظ على بيانات الآباء المسجلين مسبقاً
INSERT INTO parents (full_name, phone_number, pin_code)
SELECT DISTINCT 'غير مدخل', parent_phone, COALESCE(parent_pin, '1234')
FROM students 
WHERE parent_phone IS NOT NULL 
ON CONFLICT (phone_number) DO NOTHING;

UPDATE students s
SET parent_id = p.id
FROM parents p
WHERE s.parent_phone = p.phone_number;

-- 4. حذف الأعمدة القديمة لتنظيف التشوه المعماري
ALTER TABLE students DROP COLUMN IF EXISTS parent_phone;
ALTER TABLE students DROP COLUMN IF EXISTS parent_pin;
ALTER TABLE students DROP COLUMN IF EXISTS parent_name;

COMMIT;