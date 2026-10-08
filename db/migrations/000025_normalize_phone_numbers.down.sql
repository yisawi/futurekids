-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: columns parents.phone_number_original and notifications.parent_phone_original,
--   after writing the original (pre-normalisation) phone numbers back.
-- Running this down-migration against the current main branch will break:
--   internal/handlers/mobile.go — MobileLoginHandler normalises the phone it receives
--     (internal/phone/phone.go), so a parent whose stored number is restored to another format
--     can no longer log in, and MobileNotificationsHandler no longer finds that parent's history.
--   internal/handlers/admin.go — AdminStudentsHandler links students by the normalised number,
--     so creating a student for such a parent creates a second parent instead.
-- Data loss: none; numbers stored or changed after the up-migration keep their canonical form.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

UPDATE parents SET phone_number = phone_number_original WHERE phone_number_original IS NOT NULL;
UPDATE notifications SET parent_phone = parent_phone_original WHERE parent_phone_original IS NOT NULL;

ALTER TABLE parents DROP COLUMN IF EXISTS phone_number_original;
ALTER TABLE notifications DROP COLUMN IF EXISTS parent_phone_original;
