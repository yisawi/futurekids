-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: column students.is_active (soft delete).
-- Running this down-migration against the current main branch will break:
--   internal/handlers/admin.go — AdminStudentsHandler (GET filter, DELETE soft-delete),
--     AdminDashboardHandler, AdminDailyAttendanceHandler, AdminExportExcelHandler.
--   internal/handlers/mobile.go — MobileTodayAttendanceHandler, MobileAttendanceSummaryHandler,
--     MobileMonthlyAttendanceHandler, MobileScheduleHandler, MobileStudentsHandler.
--   internal/cron/absent_job.go — ProcessDailyAbsences.
-- Data loss: which students were deleted; soft-deleted students become indistinguishable from active ones.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

ALTER TABLE students DROP COLUMN is_active;
