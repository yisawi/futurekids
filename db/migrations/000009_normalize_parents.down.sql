-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: table parents and column students.parent_id (parent data is copied back into students).
-- Running this down-migration against the current main branch will break:
--   internal/handlers/admin.go — AdminStudentsHandler upserts and joins parents;
--     AdminDashboardHandler counts parents; AdminExportExcelHandler joins parents.
--   internal/handlers/mobile.go — MobileLoginHandler reads parents; every parent endpoint
--     filters students by parent_id; MobileNotificationsHandler joins parents.
--   internal/handlers/hardware.go — notifyPunch joins parents.
--   internal/handlers/middleware.go — AuthMiddleware reads parents.session_version on every parent request.
--   internal/cron/absent_job.go — ProcessDailyAbsences joins parents.
--   cmd/migrate-pins/main.go — reads and updates parents.
--   internal/notify/announcement.go — announcement recipients are parents.
--   internal/handlers/announcements.go — looks up a parent by phone and shows the parent in the
--     sent log.
-- Data loss: parents with no student and parents.created_at are lost; the rest is copied back to
--   students.parent_phone, parent_pin and parent_name.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

BEGIN;

-- 1. استعادة الأعمدة القديمة في جدول الطلاب
ALTER TABLE students ADD COLUMN IF NOT EXISTS parent_phone VARCHAR(20);
ALTER TABLE students ADD COLUMN IF NOT EXISTS parent_pin VARCHAR(255);
-- parent_name never had a tracked up-migration (pre-existing schema drift);
-- VARCHAR(255) matches parents.full_name, its closest post-normalization equivalent.
ALTER TABLE students ADD COLUMN IF NOT EXISTS parent_name VARCHAR(255);

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
