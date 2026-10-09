package warnings

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"future_kids/internal/auth"
	"future_kids/internal/handlers"
	"future_kids/internal/tz"
)

// TestH1TodayBeforeCheckInWindowEnd verifies Task H1 (today): before 09:31 Asia/Baghdad the
// parent monthly records and summary leave out today's record when it is Absent (a Present or
// Excused today stays); from 09:31:00 it counts as before. The today endpoint, the dashboard
// and the admin report for today are unchanged.
func TestH1TodayBeforeCheckInWindowEnd(t *testing.T) {
	db, _ := setupThrowawayDB(t, "h1today")
	realToday := tz.Today()
	for _, q := range []string{
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1, 'H1 Parent', '+9647000001601', 'x')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id, created_at) VALUES
			(1, 'H1 Present', 'H1T-1', 1, NULL), (2, 'H1 Excused', 'H1T-2', 1, NULL), (3, 'H1 Absent', 'H1T-3', 1, NULL)`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('H1T-DEV', 'Gate', true)`,
		`INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (1, 'H1T-DEV', '2026-03-18 07:05')`,
		`INSERT INTO student_leaves (student_id, leave_date) VALUES (2, '2026-03-18')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	auth.InitAuth("h1-test-secret")
	parent, _ := auth.GenerateParentToken(1, "+9647000001601", 0)
	admin, _ := auth.GenerateAdminToken("admin", 0)
	app := &handlers.AppEnv{DB: db}
	restore := handlers.Clock
	t.Cleanup(func() { handlers.Clock = restore })
	at := func(clock string) {
		ts, err := time.ParseInLocation("2006-01-02 15:04:05", clock, tz.Baghdad)
		if err != nil {
			t.Fatal(err)
		}
		handlers.Clock = func() time.Time { return ts }
	}
	marchDaysTo18 := len(h1MarchSchoolDays(1)) - len(h1MarchSchoolDays(19))

	views := func(t *testing.T, month string) (map[int]string, map[int]string) {
		t.Helper()
		rec := serve(t, app.AuthMiddleware(app.MobileMonthlyAttendanceHandler), http.MethodGet, "/api/mobile/attendance/monthly?month="+month, parent, "")
		var m h1Monthly
		json.Unmarshal(rec.Body.Bytes(), &m)
		latest := map[int]string{}
		for _, s := range m.Data {
			latest[s.StudentID] = fmt.Sprintf("%d %s %s", len(s.Records), s.Records[0].Date, s.Records[0].Status)
		}
		rec = serve(t, app.AuthMiddleware(app.MobileAttendanceSummaryHandler), http.MethodGet, "/api/mobile/attendance/summary?month="+month, parent, "")
		var sm h1Summary
		json.Unmarshal(rec.Body.Bytes(), &sm)
		counts := map[int]string{}
		for _, s := range sm.Data {
			counts[s.StudentID] = fmt.Sprintf("%d/%d/%d", s.TotalPresent, s.TotalExcused, s.TotalAbsent)
		}
		return latest, counts
	}

	n := marchDaysTo18
	before := map[int]string{1: fmt.Sprintf("%d 2026-03-18 Present", n), 2: fmt.Sprintf("%d 2026-03-18 Excused", n), 3: fmt.Sprintf("%d 2026-03-17 Absent", n-1)}
	beforeCounts := map[int]string{1: fmt.Sprintf("1/0/%d", n-1), 2: fmt.Sprintf("0/1/%d", n-1), 3: fmt.Sprintf("0/0/%d", n-1)}
	after := map[int]string{1: before[1], 2: before[2], 3: fmt.Sprintf("%d 2026-03-18 Absent", n)}
	afterCounts := map[int]string{1: beforeCounts[1], 2: beforeCounts[2], 3: fmt.Sprintf("0/0/%d", n)}
	for _, tc := range []struct {
		clock          string
		latest, counts map[int]string
	}{
		{"2026-03-18 00:00:00", before, beforeCounts},
		{"2026-03-18 07:15:00", before, beforeCounts},
		{"2026-03-18 09:30:59", before, beforeCounts},
		{"2026-03-18 09:31:00", after, afterCounts},
		{"2026-03-18 14:00:00", after, afterCounts},
		{"2026-03-18 23:59:59", after, afterCounts},
	} {
		t.Run("today at "+tc.clock[11:], func(t *testing.T) {
			at(tc.clock)
			latest, counts := views(t, "2026-03")
			if fmt.Sprint(latest) != fmt.Sprint(tc.latest) {
				t.Errorf("monthly (records, newest record)\n got: %v\nwant: %v", latest, tc.latest)
			}
			if fmt.Sprint(counts) != fmt.Sprint(tc.counts) {
				t.Errorf("summary present/excused/absent\n got: %v\nwant: %v", counts, tc.counts)
			}
		})
	}

	t.Run("a month without today is unaffected before 09:31", func(t *testing.T) {
		at("2026-03-18 07:15:00")
		latest, counts := views(t, "2026-02")
		feb := 20
		want := fmt.Sprintf("%d 2026-02-26 Absent", feb)
		for id := 1; id <= 3; id++ {
			if latest[id] != want || counts[id] != fmt.Sprintf("0/0/%d", feb) {
				t.Errorf("student %d in February: %q %q, want %q 0/0/%d", id, latest[id], counts[id], want, feb)
			}
		}
	})

	t.Run("today endpoint, dashboard and today's admin report still say Absent before 09:31", func(t *testing.T) {
		at(realToday + " 07:15:00")
		rec := serve(t, app.AuthMiddleware(app.MobileTodayAttendanceHandler), http.MethodGet, "/api/mobile/attendance/today", parent, "")
		if !strings.Contains(rec.Body.String(), `"full_name":"H1 Absent","status":"Absent"`) {
			t.Errorf("parent today: %s", rec.Body.String())
		}
		rec = serve(t, app.AdminMiddleware(app.AdminDailyAttendanceHandler), http.MethodGet, "/api/admin/attendance?date="+realToday, admin, "")
		if strings.Count(rec.Body.String(), `"status":"Absent"`) != 3 {
			t.Errorf("admin report for today: %s", rec.Body.String())
		}
		rec = serve(t, app.AdminMiddleware(app.AdminDashboardHandler), http.MethodGet, "/api/admin/dashboard", admin, "")
		if !strings.Contains(rec.Body.String(), `"absent_today":3`) {
			t.Errorf("dashboard: %s", rec.Body.String())
		}
	})
}
