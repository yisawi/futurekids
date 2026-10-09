package warnings

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"future_kids/internal/auth"
	"future_kids/internal/handlers"
)

const (
	c1Phone         = "+9647000000951"
	c1MonthlyPath   = "/api/mobile/attendance/monthly?month=2026-03"
	c1DailyPath     = "/api/admin/attendance?date=2026-03-01"
	c1GoldenMonthly = `{"data":[{"student_id":9101,"full_name":"C1 Child One","records":[{"date":"2026-03-31","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-30","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-29","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-26","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-25","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-24","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-23","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-22","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-19","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-18","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-17","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-16","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-15","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-12","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-11","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-10","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-09","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-08","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-05","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-04","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-03","status":"Excused","check_time":null,"check_out_time":null},{"date":"2026-03-02","status":"Present","check_time":null,"check_out_time":"12:30 PM"},{"date":"2026-03-01","status":"Present","check_time":"07:15 AM","check_out_time":"12:30 PM"}]},{"student_id":9102,"full_name":"C1 Child Two","records":[{"date":"2026-03-31","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-30","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-29","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-26","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-25","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-24","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-23","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-22","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-19","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-18","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-17","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-16","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-15","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-12","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-11","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-10","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-09","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-08","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-05","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-04","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-03","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-02","status":"Absent","check_time":null,"check_out_time":null},{"date":"2026-03-01","status":"Absent","check_time":null,"check_out_time":null}]}],"month":"2026-03","status":"success"}
`
	c1GoldenDaily = `{"data":[{"student_id":9101,"full_name":"C1 Child One","status":"Present","check_in_time":"07:15 AM","check_out_time":"12:30 PM"},{"student_id":9102,"full_name":"C1 Child Two","status":"Absent","check_in_time":null,"check_out_time":null}],"date":"2026-03-01","status":"success"}
`
)

// c1GoldenMonthly and c1GoldenDaily are the exact response bodies the code produced for c1Setup
// before the rename (monthly records then named the check-in time "check_time").

// c1Setup seeds one parent with two children in March 2026 (a past month, so responses are
// fixed): child one punches in and out on 1 March, only out on 2 March, has a leave on 3 March;
// child two never punches.
func c1Setup(t *testing.T) (*handlers.AppEnv, string, string) {
	t.Helper()
	db, _ := setupThrowawayDB(t, "c1")
	for _, q := range []string{
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('C1-DEV', 'Gate', true)`,
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (9101, 'C1 Parent', '` + c1Phone + `', 'x')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id, grade, section, created_at) VALUES (9101, 'C1 Child One', 'C1-1', 9101, 'G1', 'A', '2026-01-01 08:00'), (9102, 'C1 Child Two', 'C1-2', 9101, 'G1', 'A', '2026-01-01 08:00')`,
		`INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (9101, 'C1-DEV', '2026-03-01 07:15:00'), (9101, 'C1-DEV', '2026-03-01 12:30:00'), (9101, 'C1-DEV', '2026-03-02 12:30:00')`,
		`INSERT INTO student_leaves (student_id, leave_date, notes) VALUES (9101, '2026-03-03', 'C1 leave')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	auth.InitAuth("c1-test-secret")
	parent, err := auth.GenerateParentToken(9101, c1Phone, 0)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := auth.GenerateAdminToken("admin", 0)
	if err != nil {
		t.Fatal(err)
	}
	return &handlers.AppEnv{DB: db}, parent, admin
}

// TestC1MonthlyCheckInTime verifies Task C1: monthly attendance records name the check-in time
// check_in_time, like the daily records, and nothing else in the monthly response changed byte
// for byte; the admin daily report and the parent's today records are unchanged.
func TestC1MonthlyCheckInTime(t *testing.T) {
	app, parent, admin := c1Setup(t)
	monthly := serve(t, app.AuthMiddleware(app.MobileMonthlyAttendanceHandler), http.MethodGet, c1MonthlyPath, parent, "")
	body := monthly.Body.String()

	t.Run("the monthly response differs from before only in the key name", func(t *testing.T) {
		want := strings.ReplaceAll(c1GoldenMonthly, `"check_time":`, `"check_in_time":`)
		if monthly.Code != http.StatusOK || body != want {
			t.Errorf("HTTP %d; body differs from the previous serialisation with check_time renamed:\ngot  %s\nwant %s", monthly.Code, body, want)
		}
		if strings.Contains(body, `"check_time"`) {
			t.Errorf("the monthly response still contains a check_time key")
		}
		if got, want := strings.Count(body, `"check_in_time":`), strings.Count(c1GoldenMonthly, `"check_time":`); got != want || got == 0 {
			t.Errorf("%d check_in_time keys, want %d (one per record)", got, want)
		}
	})

	t.Run("check_in_time holds the same values as before", func(t *testing.T) {
		var resp struct {
			Data []struct {
				StudentID int                          `json:"student_id"`
				Records   []map[string]json.RawMessage `json:"records"`
			} `json:"data"`
		}
		if err := json.Unmarshal(monthly.Body.Bytes(), &resp); err != nil || len(resp.Data) == 0 {
			t.Fatalf("decode: %v", err)
		}
		byDate := map[string]map[string]json.RawMessage{}
		for _, r := range resp.Data[0].Records {
			var date string
			json.Unmarshal(r["date"], &date)
			byDate[date] = r
		}
		for date, want := range map[string][2]string{
			"2026-03-01": {`"07:15 AM"`, `"12:30 PM"`},
			"2026-03-02": {`null`, `"12:30 PM"`},
			"2026-03-03": {`null`, `null`},
			"2026-03-04": {`null`, `null`},
		} {
			r := byDate[date]
			in, hasIn := r["check_in_time"]
			if _, old := r["check_time"]; old || !hasIn || string(in) != want[0] || string(r["check_out_time"]) != want[1] {
				t.Errorf("%s: %v; want check_in_time %s and check_out_time %s, and no check_time", date, r, want[0], want[1])
			}
		}
	})

	t.Run("the admin daily report is unchanged", func(t *testing.T) {
		daily := serve(t, app.AdminMiddleware(app.AdminDailyAttendanceHandler), http.MethodGet, c1DailyPath, admin, "")
		if daily.Code != http.StatusOK || daily.Body.String() != c1GoldenDaily {
			t.Errorf("HTTP %d; body changed:\ngot  %s\nwant %s", daily.Code, daily.Body.String(), c1GoldenDaily)
		}
	})

	t.Run("the parent's today records keep their keys", func(t *testing.T) {
		today := serve(t, app.AuthMiddleware(app.MobileTodayAttendanceHandler), http.MethodGet, "/api/mobile/attendance/today", parent, "")
		var resp struct {
			Data []map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(today.Body.Bytes(), &resp); err != nil || today.Code != http.StatusOK || len(resp.Data) != 2 {
			t.Fatalf("today: HTTP %d, %d records, %v", today.Code, len(resp.Data), err)
		}
		for _, r := range resp.Data {
			var keys []string
			for k := range r {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if strings.Join(keys, ",") != "check_in_time,check_out_time,full_name,status,student_id" {
				t.Errorf("today record keys %v changed", keys)
			}
		}
	})
}
