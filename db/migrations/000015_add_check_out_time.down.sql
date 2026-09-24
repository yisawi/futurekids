-- ⚠ APP-COMPATIBILITY WARNING
-- Running this down-migration against the current main branch will break:
--   internal/handlers/admin.go — AdminDailyAttendanceHandler and
--   AdminExportExcelHandler call get_student_status() and expect a 3-column
--   result (status TEXT, first_check TEXT, last_check TEXT). This down-migration
--   reverts the function to a 2-column signature (status TEXT, first_check TIMESTAMP),
--   which will cause a runtime scan error on every attendance query.
--   internal/handlers/mobile.go — MobileTodayAttendanceHandler and
--   MobileMonthlyAttendanceHandler have the same 3-column dependency.
-- Do not run this migrate-down without first reverting or updating those
-- files to match the pre-migration schema.

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
