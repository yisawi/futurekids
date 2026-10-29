-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: table weekly_schedules and columns students.grade and students.section.
-- Running this down-migration against the current main branch will break:
--   internal/handlers/mobile.go — MobileScheduleHandler joins weekly_schedules on grade/section;
--     MobileStudentsHandler reads grade and section.
--   internal/handlers/admin.go — AdminStudentsHandler reads (GET) and writes (POST/PUT) grade
--     and section; AdminExportExcelHandler exports them.
--   internal/handlers/schedule_excel.go — AdminScheduleClassesHandler, AdminScheduleExportHandler
--     and AdminScheduleImportHandler read and write weekly_schedules and read students.grade and
--     students.section.
--   internal/notify/broadcast.go — broadcasts to a class match students.grade and
--     students.section.
--   internal/handlers/manual_attendance.go — the manual attendance responses show the
--     student's grade and section.
--   internal/handlers/teachers.go — GET /api/admin/teachers/specializations reads weekly_schedules.
--   internal/handlers/tuition.go — a student's tuition fee is the fee of students.grade; the
--     tuition list shows and filters grade and section.
-- Data loss: the whole weekly timetable and every student's grade and section.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

DROP TABLE IF EXISTS weekly_schedules;
ALTER TABLE students DROP COLUMN IF EXISTS grade;
ALTER TABLE students DROP COLUMN IF EXISTS section;
