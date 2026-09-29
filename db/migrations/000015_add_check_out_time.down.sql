-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: the 3-column get_student_status (status, first_check TEXT, last_check TEXT); it is
--   replaced by the 2-column version (status, first_check TIMESTAMP).
-- Running this down-migration against the current main branch will break:
--   internal/handlers/admin.go — AdminDailyAttendanceHandler and AdminExportExcelHandler read
--     first_check/last_check as text.
--   internal/handlers/mobile.go — MobileTodayAttendanceHandler and MobileMonthlyAttendanceHandler
--     read first_check/last_check as text.
--   internal/handlers/hardware.go — notifyPunch compares first_check/last_check.
-- Data loss: none (function only); check-out times disappear from every attendance response.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

DROP FUNCTION IF EXISTS get_student_status(INT, DATE);

CREATE OR REPLACE FUNCTION get_student_status(p_student_id INT, p_date DATE)
RETURNS TABLE (
    status TEXT,
    first_check TIMESTAMP
) 
LANGUAGE sql 
STABLE
AS $$
    SELECT 
        CASE 
            WHEN al.id IS NOT NULL THEN 'Present'::TEXT
            WHEN sl.id IS NOT NULL THEN 'Excused'::TEXT
            ELSE 'Absent'::TEXT
        END AS status,
        al.check_time AS first_check
    FROM (SELECT 1) _dummy
    LEFT JOIN (
        SELECT id, check_time
        FROM attendance_logs 
        WHERE student_id = p_student_id 
        AND check_time >= p_date AND check_time < p_date + INTERVAL '1 day'
        ORDER BY check_time ASC LIMIT 1
    ) al ON true
    LEFT JOIN (
        SELECT id 
        FROM student_leaves 
        WHERE student_id = p_student_id 
        AND leave_date = p_date
        LIMIT 1
    ) sl ON true;
$$;
