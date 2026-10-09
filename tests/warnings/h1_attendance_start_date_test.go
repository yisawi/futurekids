package warnings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"future_kids/internal/auth"
	"future_kids/internal/handlers"

	excelize "github.com/xuri/excelize/v2"
)

// h1MarchSchoolDays lists the school days (Sunday to Thursday) of March 2026 from day `from`.
func h1MarchSchoolDays(from int) []string {
	var days []string
	for d := from; d <= 31; d++ {
		if wd := (d - 1) % 7; wd != 5 && wd != 6 {
			days = append(days, fmt.Sprintf("2026-03-%02d", d))
		}
	}
	return days
}

type h1Monthly struct {
	Data []struct {
		StudentID int `json:"student_id"`
		Records   []struct {
			Date   string `json:"date"`
			Status string `json:"status"`
		} `json:"records"`
	} `json:"data"`
}

type h1Summary struct {
	Data []struct {
		StudentID    int `json:"student_id"`
		TotalPresent int `json:"total_present"`
		TotalExcused int `json:"total_excused"`
		TotalAbsent  int `json:"total_absent"`
	} `json:"data"`
}

// TestH1AttendanceStartDate verifies Task H1 (start date): a student is counted from the
// Asia/Baghdad date of students.created_at, inclusive; earlier days are neither listed nor
// counted (punches and leaves on them are ignored); NULL created_at counts every day; past-date
// admin reports and exports leave out students added later.
func TestH1AttendanceStartDate(t *testing.T) {
	db, _ := setupThrowawayDB(t, "h1start")
	for _, q := range []string{
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1, 'H1 Parent', '+9647000001501', 'x'), (2, 'H1 Later Parent', '+9647000001502', 'x')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id, created_at) VALUES
			(1, 'H1 New', 'H1-1', 1, '2026-03-11 10:00'),
			(2, 'H1 Always', 'H1-2', 1, NULL),
			(3, 'H1 Friday', 'H1-3', 1, '2026-03-13 09:00'),
			(4, 'H1 Later', 'H1-4', 2, '2026-04-05 08:00')`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('H1-DEV', 'Gate', true)`,
		`INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (1, 'H1-DEV', '2026-03-09 07:10'), (1, 'H1-DEV', '2026-03-12 07:20'), (3, 'H1-DEV', '2026-03-12 07:25')`,
		`INSERT INTO student_leaves (student_id, leave_date, notes) VALUES (1, '2026-03-10', 'H1 leave before start')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	auth.InitAuth("h1-test-secret")
	parent, _ := auth.GenerateParentToken(1, "+9647000001501", 0)
	laterParent, _ := auth.GenerateParentToken(2, "+9647000001502", 0)
	admin, _ := auth.GenerateAdminToken("admin", 0)
	app := &handlers.AppEnv{DB: db}

	monthly := func(t *testing.T, token, month string) h1Monthly {
		t.Helper()
		rec := serve(t, app.AuthMiddleware(app.MobileMonthlyAttendanceHandler), http.MethodGet, "/api/mobile/attendance/monthly?month="+month, token, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("monthly %s: %d %s", month, rec.Code, rec.Body.String())
		}
		var m h1Monthly
		json.Unmarshal(rec.Body.Bytes(), &m)
		return m
	}
	summary := func(t *testing.T, token, month string) h1Summary {
		t.Helper()
		rec := serve(t, app.AuthMiddleware(app.MobileAttendanceSummaryHandler), http.MethodGet, "/api/mobile/attendance/summary?month="+month, token, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("summary %s: %d %s", month, rec.Code, rec.Body.String())
		}
		var s h1Summary
		json.Unmarshal(rec.Body.Bytes(), &s)
		return s
	}
	report := func(t *testing.T, date string) map[int]string {
		t.Helper()
		rec := serve(t, app.AdminMiddleware(app.AdminDailyAttendanceHandler), http.MethodGet, "/api/admin/attendance?date="+date, admin, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("report %s: %d %s", date, rec.Code, rec.Body.String())
		}
		var r struct {
			Data []struct {
				StudentID int    `json:"student_id"`
				Status    string `json:"status"`
			} `json:"data"`
		}
		json.Unmarshal(rec.Body.Bytes(), &r)
		out := map[int]string{}
		for _, e := range r.Data {
			out[e.StudentID] = e.Status
		}
		return out
	}
	export := func(t *testing.T, date string) []string {
		t.Helper()
		rec := serve(t, app.AdminMiddleware(app.AdminExportExcelHandler), http.MethodGet, "/api/admin/export/excel?date="+date, admin, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("export %s: %d", date, rec.Code)
		}
		f, err := excelize.OpenReader(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		rows, _ := f.GetRows("Sheet1")
		var ids []string
		for i, r := range rows {
			if i >= 5 && len(r) > 0 {
				ids = append(ids, r[0])
			}
		}
		sort.Strings(ids)
		return ids
	}
	byStudent := func(m h1Monthly) map[int][]string {
		out := map[int][]string{}
		for _, s := range m.Data {
			for _, r := range s.Records {
				out[s.StudentID] = append(out[s.StudentID], r.Date+" "+r.Status)
			}
		}
		return out
	}
	expect := func(days []string, status map[string]string) []string {
		var out []string
		for i := len(days) - 1; i >= 0; i-- {
			s := "Absent"
			if v, ok := status[days[i]]; ok {
				s = v
			}
			out = append(out, days[i]+" "+s)
		}
		return out
	}

	t.Run("monthly records start on the creation date", func(t *testing.T) {
		got := byStudent(monthly(t, parent, "2026-03"))
		for _, tc := range []struct {
			name string
			id   int
			want []string
		}{
			{"created on 2026-03-11 (punch on the 9th and leave on the 10th ignored)", 1, expect(h1MarchSchoolDays(11), map[string]string{"2026-03-12": "Present"})},
			{"NULL created_at counts every school day", 2, expect(h1MarchSchoolDays(1), nil)},
			{"created on Friday 2026-03-13 starts on Sunday the 15th (punch on the 12th ignored)", 3, expect(h1MarchSchoolDays(15), nil)},
		} {
			if strings.Join(got[tc.id], "|") != strings.Join(tc.want, "|") {
				t.Errorf("%s\n got: %v\nwant: %v", tc.name, got[tc.id], tc.want)
			}
		}
		if _, listed := got[4]; listed {
			t.Errorf("another parent's child is listed")
		}
	})

	t.Run("the summary counts only days from the creation date", func(t *testing.T) {
		got := map[int]string{}
		for _, s := range summary(t, parent, "2026-03").Data {
			got[s.StudentID] = fmt.Sprintf("%d/%d/%d", s.TotalPresent, s.TotalExcused, s.TotalAbsent)
		}
		want := map[int]string{
			1: fmt.Sprintf("1/0/%d", len(h1MarchSchoolDays(11))-1),
			2: fmt.Sprintf("0/0/%d", len(h1MarchSchoolDays(1))),
			3: fmt.Sprintf("0/0/%d", len(h1MarchSchoolDays(15))),
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("present/excused/absent\n got: %v\nwant: %v", got, want)
		}
	})

	t.Run("a month entirely before the creation date leaves the child out", func(t *testing.T) {
		if m := monthly(t, laterParent, "2026-03"); len(m.Data) != 0 {
			t.Errorf("monthly for a month before the only child existed: %+v, want an empty data array", m.Data)
		}
		if s := summary(t, laterParent, "2026-03"); len(s.Data) != 0 {
			t.Errorf("summary for a month before the only child existed: %+v, want an empty data array", s.Data)
		}
		var ids []int
		for _, s := range monthly(t, parent, "2026-02").Data {
			ids = append(ids, s.StudentID)
		}
		if fmt.Sprint(ids) != "[2]" {
			t.Errorf("February lists students %v, want only the one with NULL created_at", ids)
		}
		var sums []int
		for _, s := range summary(t, parent, "2026-02").Data {
			sums = append(sums, s.StudentID)
		}
		if fmt.Sprint(sums) != "[2]" {
			t.Errorf("February summary lists students %v, want [2]", sums)
		}
		april := byStudent(monthly(t, laterParent, "2026-04"))
		if len(april[4]) == 0 || !strings.HasPrefix(april[4][len(april[4])-1], "2026-04-05 ") {
			t.Errorf("the child created on 2026-04-05 should start that day: %v", april[4])
		}
	})

	t.Run("admin report and Excel export list only students who existed on the date", func(t *testing.T) {
		for _, tc := range []struct {
			date   string
			report map[int]string
		}{
			{"2026-03-10", map[int]string{2: "Absent"}},
			{"2026-03-11", map[int]string{1: "Absent", 2: "Absent"}},
			{"2026-03-12", map[int]string{1: "Present", 2: "Absent"}},
			{"2026-03-13", map[int]string{1: "Absent", 2: "Absent", 3: "Absent"}},
			{"2026-03-15", map[int]string{1: "Absent", 2: "Absent", 3: "Absent"}},
			{"2026-04-05", map[int]string{1: "Absent", 2: "Absent", 3: "Absent", 4: "Absent"}},
		} {
			got := report(t, tc.date)
			if fmt.Sprint(got) != fmt.Sprint(tc.report) {
				t.Errorf("report %s\n got: %v\nwant: %v", tc.date, got, tc.report)
			}
			var want []string
			for id := range tc.report {
				want = append(want, fmt.Sprint(id))
			}
			sort.Strings(want)
			if got := export(t, tc.date); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("export %s lists %v, want %v", tc.date, got, want)
			}
		}
	})

	t.Run("response shapes are unchanged", func(t *testing.T) {
		rec := serve(t, app.AuthMiddleware(app.MobileMonthlyAttendanceHandler), http.MethodGet, "/api/mobile/attendance/monthly?month=2026-03", parent, "")
		var raw map[string]any
		json.Unmarshal(rec.Body.Bytes(), &raw)
		keys := func(m map[string]any) string {
			var k []string
			for key := range m {
				k = append(k, key)
			}
			sort.Strings(k)
			return strings.Join(k, ",")
		}
		first := raw["data"].([]any)[0].(map[string]any)
		record := first["records"].([]any)[0].(map[string]any)
		if keys(raw) != "data,month,status" || keys(first) != "full_name,records,student_id" || keys(record) != "check_in_time,check_out_time,date,status" {
			t.Errorf("monthly keys %s / %s / %s", keys(raw), keys(first), keys(record))
		}
		rec = serve(t, app.AuthMiddleware(app.MobileAttendanceSummaryHandler), http.MethodGet, "/api/mobile/attendance/summary?month=2026-03", parent, "")
		json.Unmarshal(rec.Body.Bytes(), &raw)
		if s := raw["data"].([]any)[0].(map[string]any); keys(raw) != "data,month,status" || keys(s) != "full_name,student_id,total_absent,total_excused,total_present" {
			t.Errorf("summary keys %s / %s", keys(raw), keys(s))
		}
	})
}
