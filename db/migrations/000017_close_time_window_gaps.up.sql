-- Closes the boundary gaps left by 000015 (RULES.md §5):
--   * Check-in  window 06:30:00 – 09:30:59.999999 (was <= 09:30:00, dropping 09:30:01–09:30:59)
--   * Check-out window 11:30:00 – 13:30:59.999999 (was <= 13:30:00, dropping 13:30:01–13:30:59)
--   * A day with only a valid check-out punch is 'Present' (the student was at school but
--     missed the morning punch), not 'Absent' with a populated last_check.
-- The return signature is unchanged, so no Go code is affected.

DROP FUNCTION IF EXISTS get_student_status(INT, DATE);

CREATE FUNCTION get_student_status(p_student_id INT, p_date DATE)
RETURNS TABLE (
    status TEXT,
    first_check TEXT,
    last_check TEXT
)
LANGUAGE sql
STABLE
AS $$
    SELECT
        CASE
            WHEN check_in.id IS NOT NULL OR check_out.id IS NOT NULL THEN 'Present'::TEXT
            WHEN sl.id IS NOT NULL THEN 'Excused'::TEXT
            ELSE 'Absent'::TEXT
        END AS status,
        TO_CHAR(check_in.check_time, 'HH12:MI AM') AS first_check,
        TO_CHAR(check_out.check_time, 'HH12:MI AM') AS last_check
    FROM (SELECT 1) _dummy
    LEFT JOIN (
        SELECT id, check_time
        FROM attendance_logs
        WHERE student_id = p_student_id
        AND check_time >= p_date + INTERVAL '6 hours 30 minutes'
        AND check_time <  p_date + INTERVAL '9 hours 31 minutes'
        ORDER BY check_time ASC LIMIT 1
    ) check_in ON true
    LEFT JOIN (
        SELECT id, check_time
        FROM attendance_logs
        WHERE student_id = p_student_id
        AND check_time >= p_date + INTERVAL '11 hours 30 minutes'
        AND check_time <  p_date + INTERVAL '13 hours 31 minutes'
        ORDER BY check_time ASC LIMIT 1
    ) check_out ON true
    LEFT JOIN (
        SELECT id
        FROM student_leaves
        WHERE student_id = p_student_id
        AND leave_date = p_date
        LIMIT 1
    ) sl ON true;
$$;
