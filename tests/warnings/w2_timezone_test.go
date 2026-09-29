package warnings

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"future_kids/internal/auth"
	"future_kids/internal/config"
	"future_kids/internal/handlers"
)

const w2ParentPhone = "+9647700000302"

// TestW2TimezoneConsistency verifies that "today" and notification timestamps
// follow Asia/Baghdad regardless of the Postgres session timezone (audit warning W2).
//
// Handler checks run on sessions pinned to UTC-12 and UTC+14. At any hour, at
// least one of them has a CURRENT_DATE different from Baghdad's date (UTC-12
// before 15:00 Baghdad time, UTC+14 from 13:00), so a handler that still relied
// on CURRENT_DATE fails deterministically whenever the test runs.
func TestW2TimezoneConsistency(t *testing.T) {
	baseDB, dsn := setupThrowawayDB(t, "w2")
	baghdad, err := time.LoadLocation("Asia/Baghdad")
	if err != nil {
		t.Fatalf("load Asia/Baghdad: %v", err)
	}

	// Simulate Railway, whose Postgres default timezone is UTC.
	if _, err := baseDB.Exec("ALTER DATABASE " + dbName(t, dsn) + " SET timezone = 'UTC'"); err != nil {
		t.Fatalf("set database timezone: %v", err)
	}

	seed := []string{
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1, 'W2 Parent', '` + w2ParentPhone + `', 'unused')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES (1, 'W2 Student', 'W2-RFID-1', 1)`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('W2-DEVICE', 'Gate', true)`,
		// Device wall-clock punches (Baghdad local time), as ZKTeco sends them.
		`INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES
			(1, 'W2-DEVICE', '2026-09-24 01:30:00'),
			(1, 'W2-DEVICE', '2026-09-24 09:30:00'),
			(1, 'W2-DEVICE', '2026-09-24 13:30:00'),
			(1, 'W2-DEVICE', '2026-09-24 23:59:00'),
			(1, 'W2-DEVICE', '2026-09-25 00:01:00')`,
		`INSERT INTO notifications (parent_phone, title, body, created_at) VALUES ('` + w2ParentPhone + `', 'W2 title', 'W2 body', '2026-09-24 01:30:00')`,
	}
	for _, q := range seed {
		if _, err := baseDB.Exec(q); err != nil {
			t.Fatalf("seed failed: %v\nquery: %s", err, q)
		}
	}

	utcDB := openDB(t, dsn)
	t.Setenv("DATABASE_URL", dsn)
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	pinnedDB := openDB(t, cfg.DBUrl)

	auth.InitAuth("w2-test-secret")
	parentToken, err := auth.GenerateParentToken(1, w2ParentPhone)
	if err != nil {
		t.Fatalf("GenerateParentToken: %v", err)
	}

	t.Run("config pins session timezone", func(t *testing.T) {
		var utcZone, pinnedZone, pinnedToday string
		if err := utcDB.QueryRow("SHOW timezone").Scan(&utcZone); err != nil {
			t.Fatalf("SHOW timezone (unpinned): %v", err)
		}
		if utcZone != "UTC" {
			t.Fatalf("CURRENT_DATE behavior: simulated Railway session should be UTC, got %q", utcZone)
		}
		if err := pinnedDB.QueryRow("SELECT current_setting('TimeZone'), CURRENT_DATE::text").Scan(&pinnedZone, &pinnedToday); err != nil {
			t.Fatalf("query pinned session: %v", err)
		}
		if pinnedZone != "Asia/Baghdad" {
			t.Errorf("CURRENT_DATE behavior: config DSN session timezone = %q, want Asia/Baghdad", pinnedZone)
		}
		if want := time.Now().In(baghdad).Format("2006-01-02"); pinnedToday != want {
			t.Errorf("CURRENT_DATE behavior: pinned CURRENT_DATE = %s, want Baghdad today %s", pinnedToday, want)
		}

		t.Setenv("DATABASE_URL", withSessionZone(t, dsn, "UTC"))
		explicitCfg, err := config.LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if u, _ := url.Parse(explicitCfg.DBUrl); u == nil || u.Query().Get("timezone") != "UTC" {
			t.Errorf("explicit DSN timezone must be kept, got %q", explicitCfg.DBUrl)
		}
	})

	t.Run("baghdad calendar day boundaries", func(t *testing.T) {
		countDay := func(day string) int {
			var n int
			err := utcDB.QueryRow(
				`SELECT COUNT(*) FROM attendance_logs WHERE student_id = 1 AND check_time >= $1::date AND check_time < $1::date + 1`,
				day,
			).Scan(&n)
			if err != nil {
				t.Fatalf("count punches for %s: %v", day, err)
			}
			return n
		}

		punch0130 := time.Date(2026, 9, 24, 1, 30, 0, 0, baghdad)
		goDay := punch0130.Format("2006-01-02")
		var utcDay string
		if err := utcDB.QueryRow(`SELECT ($1::timestamptz AT TIME ZONE 'UTC')::date::text`, punch0130).Scan(&utcDay); err != nil {
			t.Fatalf("compute UTC date: %v", err)
		}
		if utcDay != "2026-09-23" {
			t.Errorf("CURRENT_DATE behavior: at 01:30 Baghdad a UTC session's date should be 2026-09-23, got %s", utcDay)
		}
		if n := countDay(utcDay); n != 0 {
			t.Errorf("CURRENT_DATE behavior: UTC date %s should match none of the 2026-09-24 punches, got %d", utcDay, n)
		}
		if n := countDay(goDay); n != 4 {
			t.Errorf("Baghdad-today behavior: day %s should hold 01:30, 09:30, 13:30 and 23:59 punches, got %d", goDay, n)
		}

		before := time.Date(2026, 9, 24, 23, 59, 0, 0, baghdad).Format("2006-01-02")
		after := time.Date(2026, 9, 25, 0, 1, 0, 0, baghdad).Format("2006-01-02")
		if before == after {
			t.Errorf("Baghdad-today behavior: 23:59 and 00:01 must fall on different days, both %s", before)
		}
		if n := countDay(after); n != 1 {
			t.Errorf("Baghdad-today behavior: day %s should hold only the 00:01 punch, got %d", after, n)
		}
	})

	expectedDays := schoolDaysUpToToday(time.Now().In(baghdad))

	for _, zone := range []string{"Etc/GMT+12", "Pacific/Kiritimati"} {
		app := &handlers.AppEnv{DB: openDB(t, withSessionZone(t, dsn, zone))}

		t.Run("monthly uses Baghdad today/"+zone, func(t *testing.T) {
			rec := serve(t, handlers.AuthMiddleware(app.MobileMonthlyAttendanceHandler), http.MethodGet, "/api/mobile/attendance/monthly", parentToken, "")
			data := decodeData(t, rec, http.StatusOK)
			var got []string
			if len(data) == 1 {
				records, _ := data[0]["records"].([]any)
				for _, r := range records {
					got = append(got, r.(map[string]any)["date"].(string))
				}
			}
			if !reflect.DeepEqual(got, expectedDays) {
				t.Errorf("Baghdad-today behavior: monthly dates under session %s\n got: %v\nwant: %v", zone, got, expectedDays)
			}
		})

		t.Run("summary uses Baghdad today/"+zone, func(t *testing.T) {
			rec := serve(t, handlers.AuthMiddleware(app.MobileAttendanceSummaryHandler), http.MethodGet, "/api/mobile/attendance/summary", parentToken, "")
			data := decodeData(t, rec, http.StatusOK)
			total := 0
			if len(data) == 1 {
				for _, k := range []string{"total_present", "total_excused", "total_absent"} {
					total += int(data[0][k].(float64))
				}
			}
			if total != len(expectedDays) {
				t.Errorf("Baghdad-today behavior: summary counted %d school days under session %s, want %d", total, zone, len(expectedDays))
			}
		})

		t.Run("notification created_at in Baghdad time/"+zone, func(t *testing.T) {
			rec := serve(t, handlers.AuthMiddleware(app.MobileNotificationsHandler), http.MethodGet, "/api/mobile/notifications", parentToken, "")
			data := decodeData(t, rec, http.StatusOK)
			if len(data) != 1 {
				t.Fatalf("expected 1 notification, got %d", len(data))
			}
			if got, want := data[0]["created_at"], "2026-09-24T01:30:00+03:00"; got != want {
				t.Errorf("JSON formatting: created_at = %v, want %s", got, want)
			}
		})
	}

	if t.Failed() {
		t.Log("FAIL: Timezone consistency not verified (see subtest errors above)")
	} else {
		t.Log("PASS: Timezone consistency verified")
	}
}

// schoolDaysUpToToday lists this month's dates from today back to the 1st,
// newest first, excluding Friday and Saturday — the monthly endpoint's contract.
func schoolDaysUpToToday(today time.Time) []string {
	var days []string
	for d := today; d.Month() == today.Month(); d = d.AddDate(0, 0, -1) {
		if d.Weekday() != time.Friday && d.Weekday() != time.Saturday {
			days = append(days, d.Format("2006-01-02"))
		}
	}
	return days
}

func withSessionZone(t *testing.T, dsn, zone string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	q := u.Query()
	q.Set("timezone", zone)
	u.RawQuery = q.Encode()
	return u.String()
}

func dbName(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	return strings.TrimPrefix(u.Path, "/")
}
