-- APP-COMPATIBILITY: none — the get_student_status signature is unchanged, so no Go code breaks. Behavior regresses to
--   000015: punches at 09:30:01-09:30:59 and 13:30:01-13:30:59 are ignored and check-out-only days
--   report Absent (audit warning W3).
-- Data loss: none (function only).

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
            WHEN check_in.id IS NOT NULL THEN 'Present'::TEXT
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
        AND check_time <= p_date + INTERVAL '9 hours 30 minutes'
        ORDER BY check_time ASC LIMIT 1
    ) check_in ON true
    LEFT JOIN (
        SELECT id, check_time
        FROM attendance_logs
        WHERE student_id = p_student_id
        AND check_time >= p_date + INTERVAL '11 hours 30 minutes'
        AND check_time <= p_date + INTERVAL '13 hours 30 minutes'
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
