BEGIN;

-- 1. استعادة الأعمدة القديمة في جدول الطلاب
ALTER TABLE students ADD COLUMN parent_phone VARCHAR(20);
ALTER TABLE students ADD COLUMN parent_pin VARCHAR(255);

-- 2. إرجاع بيانات الآباء إلى جدول الطلاب
UPDATE students s
SET parent_phone = p.phone_number,
    parent_pin = p.pin_code
FROM parents p
WHERE s.parent_id = p.id;

-- 3. فك الربط وحذف جدول الآباء
ALTER TABLE students DROP COLUMN IF EXISTS parent_id;
DROP TABLE IF EXISTS parents;

COMMIT;