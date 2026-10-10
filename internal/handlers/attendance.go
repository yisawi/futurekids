package handlers

import (
	"context"
	"database/sql"
	"time"

	"future_kids/internal/tz"
)

// Clock returns the current Asia/Baghdad time that decides "today" everywhere; tests and
// FAKE_TODAY replace it.
var Clock = tz.Now

func today() string { return Clock().Format("2006-01-02") }

// CheckInWindowEnd is when the check-in window closes, after midnight, exclusive. The SQL copy
// is the status function's check-in window in db/migrations/000017_close_time_window_gaps.up.sql
// (check_time < p_date + INTERVAL '9 hours 31 minutes'). Before it, the parent month views leave
// out today's record when it is Absent, since the child may simply not have arrived yet.
const CheckInWindowEnd = 9*time.Hour + 31*time.Minute

// studentExistedOnSQL is true when student s already existed on the date given as %s. A student
// is counted from the date of students.created_at, inclusive; the column holds Asia/Baghdad wall
// time (the session time zone), so its date is taken as is. NULL means every day counts.
const studentExistedOnSQL = "(s.created_at IS NULL OR s.created_at::date <= %s)"

// Day types, in precedence order: Friday and Saturday are the weekend, then a pause, then a
// holiday; any other date is a school day.
const (
	DaySchool  = "school"
	DayWeekend = "weekend"
	DayHoliday = "holiday"
	DayPause   = "pause"
)

// schoolDaySQL is true when the date given as %[1]s is a school day: Sunday to Thursday and not
// covered by any school closure. DayOn is the same rule for one date.
const schoolDaySQL = `(EXTRACT(DOW FROM %[1]s) NOT IN (5, 6) AND NOT EXISTS (
	SELECT 1 FROM school_closures sc
	WHERE sc.start_date <= %[1]s AND (sc.end_date IS NULL OR sc.end_date >= %[1]s)))`

// Day is the type of one date, with the covering closure's title and notes for a holiday or pause.
type Day struct {
	Type  string
	Title *string
	Notes *string
}

// School reports whether the date is a school day.
func (d Day) School() bool { return d.Type == DaySchool }

// DayOn returns the type of date (YYYY-MM-DD) under the schoolDaySQL rule.
func DayOn(ctx context.Context, q rowQueryer, date string) (Day, error) {
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		return Day{}, err
	}
	if wd := d.Weekday(); wd == time.Friday || wd == time.Saturday {
		return Day{Type: DayWeekend}, nil
	}
	day := Day{}
	err = q.QueryRowContext(ctx, `
		SELECT kind, title, notes FROM school_closures
		WHERE start_date <= $1::date AND (end_date IS NULL OR end_date >= $1::date)
		ORDER BY kind = 'pause' DESC, start_date
		LIMIT 1`, date).Scan(&day.Type, &day.Title, &day.Notes)
	if err == sql.ErrNoRows {
		return Day{Type: DaySchool}, nil
	}
	if err != nil {
		return Day{}, err
	}
	return day, nil
}

// withDay adds the day_type, day_title and day_notes fields of day to a response.
func withDay(resp map[string]any, day Day) map[string]any {
	resp["day_type"], resp["day_title"], resp["day_notes"] = day.Type, day.Title, day.Notes
	return resp
}

func beforeCheckInWindowEnd(now time.Time) bool {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return now.Before(midnight.Add(CheckInWindowEnd))
}
