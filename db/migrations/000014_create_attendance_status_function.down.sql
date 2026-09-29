-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: function get_student_status.
-- Running this down-migration against the current main branch will break:
--   internal/handlers/admin.go — AdminDashboardHandler, AdminDailyAttendanceHandler,
--     AdminExportExcelHandler.
--   internal/handlers/mobile.go — MobileTodayAttendanceHandler, MobileAttendanceSummaryHandler,
--     MobileMonthlyAttendanceHandler.
--   internal/handlers/hardware.go — notifyPunch.
--   internal/cron/absent_job.go — ProcessDailyAbsences.
-- Data loss: none (only the function is dropped).
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

DROP FUNCTION IF EXISTS get_student_status(INT, DATE) CASCADE;
