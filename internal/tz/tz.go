// Package tz holds the school's timezone, loaded once for the whole process.
package tz

import "time"

// Baghdad is Asia/Baghdad, loaded once at startup. Iraq has used a fixed UTC+3 with no
// daylight saving since 2008, so when the system has no tz database the fixed-offset
// fallback gives identical results; FromTZDatabase reports which one is in use.
var Baghdad, FromTZDatabase = load()

func load() (*time.Location, bool) {
	loc, err := time.LoadLocation("Asia/Baghdad")
	if err != nil {
		return time.FixedZone("Asia/Baghdad", 3*60*60), false
	}
	return loc, true
}

// Now returns the current time in Baghdad.
func Now() time.Time { return time.Now().In(Baghdad) }

// Today returns today's date in Baghdad as YYYY-MM-DD.
func Today() string { return Now().Format("2006-01-02") }
