BEGIN;

-- 1. استعادة الأعمدة القديمة في جدول الطلاب
ALTER TABLE students ADD COLUMN parent_phone VARCHAR(20);
ALTER TABLE students ADD COLUMN parent_pin VARCHAR(255);
-- parent_name never had a tracked up-migration (pre-existing schema drift);
-- VARCHAR(255) matches parents.full_name, its closest post-normalization equivalent.
ALTER TABLE students ADD COLUMN parent_name VARCHAR(255);

-- 2. إرجاع بيانات الآباء إلى جدول الطلاب
UPDATE students s
SET parent_phone = p.phone_number,
    parent_pin = p.pin_code,
    parent_name = p.full_name
FROM parents p
WHERE s.parent_id = p.id;

-- 3. فك الربط وحذف جدول الآباء
ALTER TABLE students DROP COLUMN IF EXISTS parent_id;
DROP TABLE IF EXISTS parents;

COMMIT;