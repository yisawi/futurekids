-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: tables attendance_logs, devices and students (the base schema).
-- Running this down-migration against the current main branch will break:
--   internal/handlers/hardware.go — ADMSHandler/persistATTLOG and HardwareAttendancePushHandler
--     write attendance_logs and read devices and students.
--   internal/handlers/middleware.go — DeviceAuthMiddleware reads devices.
--   internal/handlers/admin.go — AdminStudentsHandler, AdminDashboardHandler,
--     AdminDailyAttendanceHandler, AdminExportExcelHandler, AdminDevicesHandler.
--   internal/handlers/mobile.go — every parent endpoint reads students.
--   internal/cron/absent_job.go — ProcessDailyAbsences reads students.
--   internal/notify/notify.go — SendPushNotification clears unregistered students.fcm_token values.
-- Data loss: every student, device and attendance punch is deleted permanently. Only run this
--   when tearing down the whole database.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

DROP TABLE IF EXISTS attendance_logs;
DROP TABLE IF EXISTS devices;
DROP TABLE IF EXISTS students;
