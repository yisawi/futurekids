-- attendance_logs.status (added in 000005) was written only by the JSON punch push ('Present',
-- while the column default is 'present') and never read: attendance status is always computed
-- by get_student_status. Deploy the app version that no longer writes it BEFORE running this,
-- or older app code will fail on JSON punch pushes.
ALTER TABLE attendance_logs DROP COLUMN IF EXISTS status;
