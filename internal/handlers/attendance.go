package handlers

import (
	"time"

	"future_kids/internal/tz"
)

// Clock returns the current Asia/Baghdad time used by the parent month views; tests replace it.
var Clock = tz.Now

// CheckInWindowEnd is when the check-in window closes, after midnight, exclusive. The SQL copy
// is the status function's check-in window in db/migrations/000017_close_time_window_gaps.up.sql
// (check_time < p_date + INTERVAL '9 hours 31 minutes'). Before it, the parent month views leave
// out today's record when it is Absent, since the child may simply not have arrived yet.
const CheckInWindowEnd = 9*time.Hour + 31*time.Minute

// studentExistedOnSQL is true when student s already existed on the date given as %s. A student
// is counted from the date of students.created_at, inclusive; the column holds Asia/Baghdad wall
// time (the session time zone), so its date is taken as is. NULL means every day counts.
const studentExistedOnSQL = "(s.created_at IS NULL OR s.created_at::date <= %s)"

func beforeCheckInWindowEnd(now time.Time) bool {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return now.Before(midnight.Add(CheckInWindowEnd))
}
