package warnings

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"future_kids/internal/auth"
	"future_kids/internal/background"
	"future_kids/internal/cron"
	"future_kids/internal/handlers"

	"github.com/golang-jwt/jwt/v5"
	"github.com/xuri/excelize/v2"
)

const (
	o1Today     = k1Today
	o1Yesterday = "2026-09-23"
	o1Path      = "/api/admin/attendance/manual"
)

// o1Seed adds two parents, their active children created long before the test dates, an inactive
// child, a child created on o1Today, a child without a parent, and a device.
func o1Seed(t *testing.T, db *sql.DB) {
	t.Helper()
	k1Exec(t, db,
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (4101, 'O1 Parent A', '+9647000004101', 'x'), (4102, 'O1 Parent B', '+9647000004102', 'x')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id, grade, section, is_active, created_at) VALUES
			(4101, 'O1 Kid A', 'O1-A', 4101, 'G1', 'A', true, '2026-01-01 08:00'),
			(4102, 'O1 Kid B', 'O1-B', 4102, 'G1', 'A', true, '2026-01-01 08:00'),
			(4103, 'O1 Inactive', 'O1-I', 4101, 'G1', 'A', false, '2026-01-01 08:00'),
			(4104, 'O1 New', 'O1-N', 4101, 'G1', 'A', true, '2026-09-24 07:00'),
			(4105, 'O1 Orphan', 'O1-O', NULL, NULL, NULL, true, '2026-01-01 08:00')`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('O1-DEV', 'Gate', true)`)
}

// o1Body is a create request for O1 Kid A on o1Yesterday, with kv overriding or adding fields.
func o1Body(kv ...any) map[string]any {
	m := map[string]any{"student_id": 4101, "date": o1Yesterday, "type": "in"}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == nil {
			delete(m, kv[i].(string))
			continue
		}
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

type o1Item struct {
	ID          *int64  `json:"id"`
	StudentID   int     `json:"student_id"`
	StudentName string  `json:"student_name"`
	Grade       *string `json:"grade"`
	Section     *string `json:"section"`
	Date        string  `json:"date"`
	Type        string  `json:"type"`
	Time        string  `json:"time"`
	Source      string  `json:"source"`
	Note        *string `json:"note"`
	Notified    bool    `json:"notified"`
	DryRun      bool    `json:"dry_run"`
}

func (i o1Item) String() string {
	id := "-"
	if i.ID != nil {
		id = "set"
	}
	return fmt.Sprintf("id=%s %d %s %s/%s %s %s %s %s note=%s notified=%v dry=%v", id, i.StudentID, i.StudentName, k1Str(i.Grade), k1Str(i.Section), i.Date, i.Type, i.Time, i.Source, k1Str(i.Note), i.Notified, i.DryRun)
}

type o1Result struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Data    o1Item `json:"data"`
}

func o1Decode(t *testing.T, status int, body []byte) o1Result {
	t.Helper()
	var r o1Result
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode %d %s: %v", status, body, err)
	}
	return r
}

// o1App is an in-process app on a seeded database with a fake FCM, an admin token and a parent
// token for each seeded parent.
type o1App struct {
	app     *handlers.AppEnv
	db      *sql.DB
	fcm     *b1FakeFCM
	bg      *background.Group
	admin   string
	parents map[int]string
}

func newO1App(t *testing.T, label string) *o1App {
	t.Helper()
	db, _ := setupThrowawayDB(t, label)
	o1Seed(t, db)
	auth.InitAuth("o1-test-secret")
	a := &o1App{db: db, fcm: newB1FakeFCM(t), bg: &background.Group{}, parents: map[int]string{}}
	a.app = &handlers.AppEnv{DB: db, FCMClient: a.fcm.client(t), Background: a.bg}
	var err error
	if a.admin, err = auth.GenerateAdminToken("admin", 0); err != nil {
		t.Fatal(err)
	}
	for id, phone := range map[int]string{4101: "+9647000004101", 4102: "+9647000004102"} {
		if a.parents[id], err = auth.GenerateParentToken(id, phone, 0); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func (a *o1App) settle(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if pending, err := a.bg.Wait(ctx); err != nil {
		t.Fatalf("background work still running: %d", pending)
	}
}

func (a *o1App) post(t *testing.T, body any) (*httptest.ResponseRecorder, o1Result) {
	t.Helper()
	raw, _ := json.Marshal(body)
	rec := serve(t, a.app.AdminMiddleware(a.app.AdminManualAttendanceHandler), http.MethodPost, o1Path, a.admin, string(raw))
	a.settle(t)
	return rec, o1Decode(t, rec.Code, rec.Body.Bytes())
}

func (a *o1App) punch(t *testing.T, tag, at string) {
	t.Helper()
	rec := httptest.NewRecorder()
	a.app.ADMSHandler(rec, httptest.NewRequest("POST", "/iclock/cdata?SN=O1-DEV&table=ATTLOG", strings.NewReader(tag+"\t"+at+"\t1\t1\n")))
	a.settle(t)
	if rec.Code != 200 || rec.Body.String() != "OK" {
		t.Fatalf("punch %s %s: %d %s", tag, at, rec.Code, rec.Body)
	}
}

func (a *o1App) get(t *testing.T, h http.HandlerFunc, target, token string) map[string]any {
	t.Helper()
	rec := serve(t, h, http.MethodGet, target, token, "")
	var m map[string]any
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &m) != nil {
		t.Fatalf("GET %s: %d %s", target, rec.Code, rec.Body)
	}
	return m
}

func o1Find(list any, id int) map[string]any {
	items, _ := list.([]any)
	for _, it := range items {
		if m, ok := it.(map[string]any); ok && fmt.Sprint(m["student_id"]) == fmt.Sprint(id) {
			return m
		}
	}
	return nil
}

func o1Pick(m map[string]any, keys ...string) string {
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
	}
	return strings.Join(parts, " ")
}

// TestO1Validation verifies the create request rules through the real API (today is o1Today):
// every malformed field, a date other than today or yesterday, a time outside its window, an
// unknown, inactive or not yet registered student, and an oversized body; none writes a row.
func TestO1Validation(t *testing.T) {
	srv, db, admin := l1Server(t, "o1valid", true)
	o1Seed(t, db)
	before := k1Count(t, db, `SELECT COUNT(*) FROM attendance_logs`)
	note201 := strings.Repeat("ب", 201)
	for _, tc := range []struct {
		name   string
		body   any
		status int
		msg    string
	}{
		{"malformed JSON", "{", 400, "Invalid request body"},
		{"empty object", map[string]any{}, 400, "student_id is required"},
		{"student_id a string", o1Body("student_id", "4101"), 400, "student_id must be a positive student id"},
		{"student_id zero", o1Body("student_id", 0), 400, "student_id must be a positive student id"},
		{"student_id negative", o1Body("student_id", -4), 400, "student_id must be a positive student id"},
		{"student_id fractional", o1Body("student_id", 4101.5), 400, "student_id must be a positive student id"},
		{"type missing", o1Body("type", nil), 400, "type is required"},
		{"type upper case", o1Body("type", "IN"), 400, "type must be in or out"},
		{"type a number", o1Body("type", 1), 400, "type must be in or out"},
		{"date not a date", o1Body("date", "2026-02-30"), 400, "date must be formatted as YYYY-MM-DD"},
		{"date a number", o1Body("date", 20260923), 400, "date must be formatted as YYYY-MM-DD"},
		{"date two days ago", o1Body("date", "2026-09-22"), 400, "date must be today (2026-09-24) or yesterday (2026-09-23)"},
		{"date tomorrow", o1Body("date", "2026-09-25"), 400, "date must be today (2026-09-24) or yesterday (2026-09-23)"},
		{"date year 1999", o1Body("date", "1999-09-23"), 400, "date must have a year from 2000 to 2100"},
		{"time one digit hour", o1Body("time", "8:00"), 400, "time must be formatted as HH:MM or HH:MM:SS"},
		{"time 24:00", o1Body("time", "24:00"), 400, "time must be formatted as HH:MM or HH:MM:SS"},
		{"time 60 seconds", o1Body("time", "08:00:60"), 400, "time must be formatted as HH:MM or HH:MM:SS"},
		{"time a number", o1Body("time", 800), 400, "time must be formatted as HH:MM or HH:MM:SS"},
		{"check-in 06:29:59", o1Body("time", "06:29:59"), 400, "time must be in the check-in window 06:30:00 to 09:30:59"},
		{"check-in 09:31:00", o1Body("time", "09:31:00"), 400, "time must be in the check-in window 06:30:00 to 09:30:59"},
		{"check-in 09:31", o1Body("time", "09:31"), 400, "time must be in the check-in window 06:30:00 to 09:30:59"},
		{"check-in at noon", o1Body("time", "12:00"), 400, "time must be in the check-in window 06:30:00 to 09:30:59"},
		{"check-out 11:29:59", o1Body("type", "out", "time", "11:29:59"), 400, "time must be in the check-out window 11:30:00 to 13:30:59"},
		{"check-out 13:31:00", o1Body("type", "out", "time", "13:31:00"), 400, "time must be in the check-out window 11:30:00 to 13:30:59"},
		{"check-out in the morning", o1Body("type", "out", "time", "08:00"), 400, "time must be in the check-out window 11:30:00 to 13:30:59"},
		{"notify a string", o1Body("notify", "yes"), 400, "notify must be true or false"},
		{"dry_run a number", o1Body("dry_run", 1), 400, "dry_run must be true or false"},
		{"note a number", o1Body("note", 5), 400, "note must be a string"},
		{"note 201 Arabic characters", o1Body("note", note201), 400, "note must be at most 200 characters"},
		{"note with NUL", o1Body("note", "a\x00b"), 400, "note must be text"},
		{"note invalid UTF-8", `{"student_id":4101,"date":"2026-09-23","type":"in","note":"bad ` + "\xff" + `"}`, 400, "note must be text"},
		{"student added on o1Today", o1Body("student_id", 4104), 400, "The student was not registered yet on 2026-09-23"},
		{"unknown student", o1Body("student_id", 999999), 404, "Student not found"},
		{"inactive student", o1Body("student_id", 4103), 404, "Student not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := a14Do(t, srv, "POST", o1Path, l1Header(admin), tc.body)
			if res := o1Decode(t, r.status, r.body); r.status != tc.status || res.Status != "error" || res.Message != tc.msg {
				t.Errorf("%d %s, want %d %q", r.status, r.body, tc.status, tc.msg)
			}
		})
	}
	r := a14Do(t, srv, "POST", o1Path, l1Header(admin), `{"pad":"`+strings.Repeat("a", 70<<10)+`"}`)
	if r.status != 413 {
		t.Errorf("oversized body: %d %s", r.status, r.body)
	}
	if after := k1Count(t, db, `SELECT COUNT(*) FROM attendance_logs`); after != before {
		t.Errorf("rejected requests wrote %d rows", after-before)
	}

	t.Run("the window bounds are exactly the status function's", func(t *testing.T) {
		k1Exec(t, db, `INSERT INTO students (id, full_name, rfid_tag, created_at) SELECT 4200 + g, 'O1 Bound ' || g, 'O1-W' || g, '2026-01-01' FROM generate_series(1, 8) g`)
		for i, tc := range []struct {
			typ, at  string
			inWindow bool
		}{
			{"in", "06:29:59", false}, {"in", "06:30:00", true}, {"in", "09:30:59", true}, {"in", "09:31:00", false},
			{"out", "11:29:59", false}, {"out", "11:30:00", true}, {"out", "13:30:59", true}, {"out", "13:31:00", false},
		} {
			student := 4201 + i
			r := a14Do(t, srv, "POST", o1Path, l1Header(admin), o1Body("student_id", student, "type", tc.typ, "time", tc.at, "dry_run", true))
			k1Exec(t, db, fmt.Sprintf(`INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (%d, 'O1-DEV', '%s %s')`, student, o1Yesterday, tc.at))
			var first, last sql.NullString
			db.QueryRow(`SELECT first_check, last_check FROM get_student_status($1, $2::date)`, student, o1Yesterday).Scan(&first, &last)
			classified := first.Valid || last.Valid
			if (r.status == 200) != tc.inWindow || classified != tc.inWindow || (tc.typ == "in" && last.Valid) || (tc.typ == "out" && first.Valid) {
				t.Errorf("%s %s: manual %d, status function first=%v last=%v; want both in window = %v", tc.typ, tc.at, r.status, first, last, tc.inWindow)
			}
			rep := a14Do(t, srv, "GET", "/api/admin/attendance?date="+o1Yesterday, l1Header(admin), nil)
			var report struct {
				Data []map[string]any `json:"data"`
			}
			json.Unmarshal(rep.body, &report)
			for _, it := range report.Data {
				if fmt.Sprint(it["student_id"]) == fmt.Sprint(student) {
					key := "check_in_source"
					if tc.typ == "out" {
						key = "check_out_source"
					}
					if got := it[key]; (got == "device") != tc.inWindow {
						t.Errorf("%s %s: report %s = %v", tc.typ, tc.at, key, got)
					}
				}
			}
		}
	})
}

// TestO1Create verifies a manual entry through the real API: the default times, the response
// (student name, grade and section for the confirmation), a student without a parent, the window
// bounds accepted by a dry run, the 409 for a punch of the same kind from any source, and that a
// dry run writes nothing.
func TestO1Create(t *testing.T) {
	srv, db, admin := l1Server(t, "o1create", true)
	o1Seed(t, db)
	post := func(t *testing.T, body any, status int) o1Result {
		t.Helper()
		r := a14Do(t, srv, "POST", o1Path, l1Header(admin), body)
		if r.status != status {
			t.Fatalf("%v: %d %s, want %d", body, r.status, r.body, status)
		}
		return o1Decode(t, r.status, r.body)
	}

	t.Run("the window bounds are accepted", func(t *testing.T) {
		for _, b := range []map[string]any{
			o1Body("time", "06:30:00", "dry_run", true), o1Body("time", "06:30", "dry_run", true), o1Body("time", "09:30:59", "dry_run", true),
			o1Body("type", "out", "time", "11:30:00", "dry_run", true), o1Body("type", "out", "time", "13:30:59", "dry_run", true), o1Body("type", "out", "time", "13:30", "dry_run", true),
		} {
			post(t, b, 200)
		}
	})

	t.Run("a dry run checks everything and writes nothing", func(t *testing.T) {
		rows, notes := k1Count(t, db, `SELECT COUNT(*) FROM attendance_logs`), k1Count(t, db, `SELECT COUNT(*) FROM notifications`)
		res := post(t, o1Body("dry_run", true, "note", "O1 preview"), 200)
		if got, want := res.Message+" "+res.Data.String(), "Preview only id=- 4101 O1 Kid A G1/A 2026-09-23 in 08:00:00 manual note=O1 preview notified=true dry=true"; got != want {
			t.Errorf("preview\n got %s\nwant %s", got, want)
		}
		if k1Count(t, db, `SELECT COUNT(*) FROM attendance_logs`) != rows || k1Count(t, db, `SELECT COUNT(*) FROM notifications`) != notes {
			t.Errorf("a dry run wrote rows")
		}
	})

	t.Run("defaults: check-in 08:00, check-out 13:00, notify true", func(t *testing.T) {
		res := post(t, o1Body("note", "  البصمة لم تعمل  "), 200)
		if got, want := res.Message+" "+res.Data.String(), "Manual attendance recorded id=set 4101 O1 Kid A G1/A 2026-09-23 in 08:00:00 manual note=البصمة لم تعمل notified=true dry=false"; got != want {
			t.Errorf("check-in\n got %s\nwant %s", got, want)
		}
		var row string
		db.QueryRow(`SELECT student_id || ' ' || check_time || ' ' || source || ' ' || COALESCE(device_sn, '<null>') || ' ' || note FROM attendance_logs WHERE id = $1`, *res.Data.ID).Scan(&row)
		if row != "4101 2026-09-23 08:00:00 manual <null> البصمة لم تعمل" {
			t.Errorf("stored row %q", row)
		}
		res = post(t, o1Body("type", "out", "notify", false), 200)
		if got, want := res.Data.String(), "id=set 4101 O1 Kid A G1/A 2026-09-23 out 13:00:00 manual note=<nil> notified=false dry=false"; got != want {
			t.Errorf("check-out\n got %s\nwant %s", got, want)
		}
	})

	t.Run("a student without a parent is recorded and nobody is notified", func(t *testing.T) {
		res := post(t, o1Body("student_id", 4105, "time", "07:05"), 200)
		if got, want := res.Data.String(), "id=set 4105 O1 Orphan <nil>/<nil> 2026-09-23 in 07:05:00 manual note=<nil> notified=false dry=false"; got != want {
			t.Errorf("got %s\nwant %s", got, want)
		}
	})

	t.Run("a punch of the same kind from any source is a 409", func(t *testing.T) {
		for _, tc := range []struct {
			body any
			msg  string
		}{
			{o1Body("time", "07:00"), "Student already has a check-in at 08:00 AM on 2026-09-23 (manual)"},
			{o1Body("time", "09:30:59", "dry_run", true), "Student already has a check-in at 08:00 AM on 2026-09-23 (manual)"},
			{o1Body("type", "out", "time", "12:00"), "Student already has a check-out at 01:00 PM on 2026-09-23 (manual)"},
		} {
			if res := post(t, tc.body, 409); res.Message != tc.msg {
				t.Errorf("%v: %q, want %q", tc.body, res.Message, tc.msg)
			}
		}
		k1Exec(t, db, `INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (4102, 'O1-DEV', '2026-09-23 07:10:00'), (4102, 'O1-DEV', '2026-09-23 10:00:00')`)
		if res := post(t, o1Body("student_id", 4102), 409); res.Message != "Student already has a check-in at 07:10 AM on 2026-09-23 (device)" {
			t.Errorf("after a device check-in: %q", res.Message)
		}
		post(t, o1Body("student_id", 4102, "type", "out", "time", "11:45"), 200)
	})

}

// TestO1Today verifies the rules that depend on the time of day, with the clock pinned: a time
// later than now is refused for today but not for yesterday.
func TestO1Today(t *testing.T) {
	a := newO1App(t, "o1today")
	pinToday(t, o1Today+" 08:30")
	for _, tc := range []struct {
		body   any
		status int
		msg    string
	}{
		{map[string]any{"student_id": 4101, "type": "in", "time": "08:31"}, 400, "time must not be later than now (08:30:00) for today"},
		{map[string]any{"student_id": 4101, "type": "out"}, 400, "time must not be later than now (08:30:00) for today"},
		{map[string]any{"student_id": 4101, "type": "in", "time": "08:30:00"}, 200, "Manual attendance recorded"},
		{map[string]any{"student_id": 4102, "type": "in"}, 200, "Manual attendance recorded"},
		{map[string]any{"student_id": 4101, "date": o1Yesterday, "type": "out"}, 200, "Manual attendance recorded"},
		{map[string]any{"student_id": 4104, "type": "in", "time": "07:30"}, 200, "Manual attendance recorded"},
	} {
		if rec, res := a.post(t, tc.body); rec.Code != tc.status || res.Message != tc.msg {
			t.Errorf("%v: %d %s, want %d %q", tc.body, rec.Code, rec.Body, tc.status, tc.msg)
		}
	}
}

// TestO1DayTypes verifies that a weekend, a holiday or a pause is refused, named in the message.
func TestO1DayTypes(t *testing.T) {
	a := newO1App(t, "o1days")
	pinToday(t, "2026-09-27 10:00")
	if rec, res := a.post(t, map[string]any{"student_id": 4101, "date": "2026-09-26", "type": "in"}); rec.Code != 400 || res.Message != "2026-09-26 is not a school day (weekend)" {
		t.Errorf("Saturday: %d %s", rec.Code, rec.Body)
	}
	k1Exec(t, a.db, `INSERT INTO school_closures (kind, start_date, end_date, title) VALUES ('holiday', '2026-09-27', '2026-09-27', 'O1 Holiday')`)
	if rec, res := a.post(t, map[string]any{"student_id": 4101, "type": "in"}); rec.Code != 400 || res.Message != "2026-09-27 is not a school day (holiday)" {
		t.Errorf("holiday: %d %s", rec.Code, rec.Body)
	}
	k1Exec(t, a.db, `DELETE FROM school_closures`, `INSERT INTO school_closures (kind, start_date, title) VALUES ('pause', '2026-09-20', 'O1 Pause')`)
	if rec, res := a.post(t, map[string]any{"student_id": 4101, "type": "in"}); rec.Code != 400 || res.Message != "2026-09-27 is not a school day (pause)" {
		t.Errorf("pause: %d %s", rec.Code, rec.Body)
	}
	if n := k1Count(t, a.db, `SELECT COUNT(*) FROM attendance_logs`); n != 0 {
		t.Errorf("%d rows written", n)
	}
}

// TestO1Effects verifies that a manual punch counts exactly like a device punch at the same time
// everywhere: the admin daily report (but for the markers), the dashboard, the parent's today,
// summary and month, the Excel export, get_student_status, the noon absence job and the 09:31
// rule; 08:00 and 09:00 behave like device punches at those times.
func TestO1Effects(t *testing.T) {
	t.Run("at 13:00: reports, dashboard, parent views, Excel, status function, noon job", func(t *testing.T) {
		a := newO1App(t, "o1effects")
		pinToday(t, o1Today+" 13:00")
		for _, b := range []map[string]any{{"student_id": 4101, "type": "in", "time": "07:20"}, {"student_id": 4101, "type": "out", "time": "12:40"}} {
			if rec, _ := a.post(t, b); rec.Code != 200 {
				t.Fatalf("manual: %d %s", rec.Code, rec.Body)
			}
		}
		a.punch(t, "O1-B", o1Today+" 07:20:00")
		a.punch(t, "O1-B", o1Today+" 12:40:00")

		daily := a.get(t, a.app.AdminMiddleware(a.app.AdminDailyAttendanceHandler), "/api/admin/attendance", a.admin)
		manual, device := o1Find(daily["data"], 4101), o1Find(daily["data"], 4102)
		if m, d := o1Pick(manual, "status", "check_in_time", "check_out_time"), o1Pick(device, "status", "check_in_time", "check_out_time"); m != d || m != "status=Present check_in_time=07:20 AM check_out_time=12:40 PM" {
			t.Errorf("admin daily: manual %s, device %s", m, d)
		}
		if m, d := o1Pick(manual, "check_in_source", "check_out_source"), o1Pick(device, "check_in_source", "check_out_source", "manual_punches"); m != "check_in_source=manual check_out_source=manual" || d != "check_in_source=device check_out_source=device manual_punches=[]" {
			t.Errorf("markers: manual %s, device %s", m, d)
		}
		if punches, _ := manual["manual_punches"].([]any); len(punches) != 2 || o1Pick(punches[0].(map[string]any), "type", "time", "note") != "type=in time=07:20:00 note=<nil>" || o1Pick(punches[1].(map[string]any), "type", "time") != "type=out time=12:40:00" {
			t.Errorf("manual_punches %v", manual["manual_punches"])
		}

		dash := a.get(t, a.app.AdminMiddleware(a.app.AdminDashboardHandler), "/api/admin/dashboard", a.admin)
		if got := o1Pick(dash["data"].(map[string]any), "total_students", "present_today", "excused_today", "absent_today"); got != "total_students=4 present_today=2 excused_today=0 absent_today=2" {
			t.Errorf("dashboard %s", got)
		}

		keys := []string{"status", "check_in_time", "check_out_time"}
		todayA := o1Find(a.get(t, a.app.AuthMiddleware(a.app.MobileTodayAttendanceHandler), "/api/mobile/attendance/today", a.parents[4101])["data"], 4101)
		todayB := o1Find(a.get(t, a.app.AuthMiddleware(a.app.MobileTodayAttendanceHandler), "/api/mobile/attendance/today", a.parents[4102])["data"], 4102)
		if o1Pick(todayA, keys...) != o1Pick(todayB, keys...) || len(todayA) != len(todayB) {
			t.Errorf("parent today: manual %v, device %v", todayA, todayB)
		}
		sumKeys := []string{"total_present", "total_excused", "total_absent"}
		sumA := o1Find(a.get(t, a.app.AuthMiddleware(a.app.MobileAttendanceSummaryHandler), "/api/mobile/attendance/summary?month=2026-09", a.parents[4101])["data"], 4101)
		sumB := o1Find(a.get(t, a.app.AuthMiddleware(a.app.MobileAttendanceSummaryHandler), "/api/mobile/attendance/summary?month=2026-09", a.parents[4102])["data"], 4102)
		if o1Pick(sumA, sumKeys...) != o1Pick(sumB, sumKeys...) {
			t.Errorf("summary: manual %v, device %v", sumA, sumB)
		}
		monthA := o1Find(a.get(t, a.app.AuthMiddleware(a.app.MobileMonthlyAttendanceHandler), "/api/mobile/attendance/monthly?month=2026-09", a.parents[4101])["data"], 4101)
		monthB := o1Find(a.get(t, a.app.AuthMiddleware(a.app.MobileMonthlyAttendanceHandler), "/api/mobile/attendance/monthly?month=2026-09", a.parents[4102])["data"], 4102)
		recA, _ := json.Marshal(monthA["records"])
		recB, _ := json.Marshal(monthB["records"])
		if string(recA) != string(recB) || !strings.Contains(string(recA), `"check_in_time":"07:20 AM"`) {
			t.Errorf("monthly: manual %s, device %s", recA, recB)
		}

		var stA, stB string
		a.db.QueryRow(`SELECT row(st.*)::text FROM get_student_status(4101, $1::date) st`, o1Today).Scan(&stA)
		a.db.QueryRow(`SELECT row(st.*)::text FROM get_student_status(4102, $1::date) st`, o1Today).Scan(&stB)
		if stA != stB {
			t.Errorf("get_student_status: manual %s, device %s", stA, stB)
		}

		rec := serve(t, a.app.AdminMiddleware(a.app.AdminExportExcelHandler), http.MethodGet, "/api/admin/export/excel", a.admin, "")
		f, err := excelize.OpenReader(strings.NewReader(rec.Body.String()))
		if err != nil {
			t.Fatal(err)
		}
		rows, _ := f.GetRows("Sheet1")
		byName := map[string][]string{}
		for _, r := range rows[5:] {
			byName[r[1]] = r
		}
		if m, d := byName["O1 Kid A"], byName["O1 Kid B"]; len(m) != 10 || len(d) != 9 || strings.Join(m[6:9], "|") != strings.Join(d[6:9], "|") || m[9] != "الدخول والخروج" {
			t.Errorf("Excel: manual %v, device %v", m, d)
		}

		cron.ProcessDailyAbsences(a.db, a.app.FCMClient, a.bg)
		a.settle(t)
		var absent []string
		rs, _ := a.db.Query(`SELECT body FROM notifications WHERE title = 'إشعار غياب' ORDER BY body`)
		for rs.Next() {
			var b string
			rs.Scan(&b)
			absent = append(absent, b)
		}
		rs.Close()
		if strings.Join(absent, "|") != "الطالب O1 New غائب اليوم" {
			t.Errorf("noon job sent %v; want only O1 New (a manual check-in counts as present)", absent)
		}
	})

	t.Run("before 09:31: the parent month view treats a manual 09:00 like a device 09:00", func(t *testing.T) {
		a := newO1App(t, "o1rule")
		pinToday(t, o1Today+" 09:15")
		if rec, _ := a.post(t, map[string]any{"student_id": 4101, "type": "in", "time": "09:00"}); rec.Code != 200 {
			t.Fatalf("manual: %d %s", rec.Code, rec.Body)
		}
		a.punch(t, "O1-B", o1Today+" 09:00:00")
		latest := func(parent, student int) (string, int) {
			m := o1Find(a.get(t, a.app.AuthMiddleware(a.app.MobileMonthlyAttendanceHandler), "/api/mobile/attendance/monthly", a.parents[parent])["data"], student)
			recs, _ := m["records"].([]any)
			if len(recs) == 0 {
				return "", 0
			}
			raw, _ := json.Marshal(recs[0])
			return string(raw), len(recs)
		}
		manual, nManual := latest(4101, 4101)
		device, nDevice := latest(4102, 4102)
		if manual != device || nManual != nDevice || !strings.Contains(manual, `"check_in_time":"09:00 AM"`) || !strings.Contains(manual, `"date":"`+o1Today+`"`) || !strings.Contains(manual, `"status":"Present"`) {
			t.Errorf("today's record: manual %s (%d), device %s (%d)", manual, nManual, device, nDevice)
		}
		if newKid, n := latest(4101, 4104); n != 0 {
			t.Errorf("the absent child's today is listed before 09:31: %s", newKid)
		}
		a.post(t, map[string]any{"student_id": 4104, "type": "in"})
		daily := a.get(t, a.app.AdminMiddleware(a.app.AdminDailyAttendanceHandler), "/api/admin/attendance", a.admin)
		if got := o1Pick(o1Find(daily["data"], 4104), "status", "check_in_time", "check_in_source"); got != "status=Present check_in_time=08:00 AM check_in_source=manual" {
			t.Errorf("default 08:00: %s", got)
		}
	})
}

// TestO1Notification verifies the parent's notification for a manual punch: the text of a
// device punch at the recorded time, stored with the punch and pushed only to that parent's
// devices; notify false sends nothing; no full token or note reaches the logs.
func TestO1Notification(t *testing.T) {
	capture := &w10Capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })
	a := newO1App(t, "o1notify")
	pinToday(t, o1Today+" 13:00")
	tokA1, tokA2, tokB := b1Token("o1-a1"), b1Token("o1-a2"), b1Token("o1-b")
	k1Exec(t, a.db, fmt.Sprintf(`INSERT INTO device_tokens (parent_id, token) VALUES (4101, '%s'), (4101, '%s'), (4102, '%s')`, tokA1, tokA2, tokB))
	history := func(parent int) []string {
		var out []string
		rs, _ := a.db.Query(`SELECT title || '|' || body FROM notifications WHERE parent_id = $1 ORDER BY id`, parent)
		defer rs.Close()
		for rs.Next() {
			var s string
			rs.Scan(&s)
			out = append(out, s)
		}
		return out
	}

	_, res := a.post(t, map[string]any{"student_id": 4101, "type": "in", "time": "07:50", "note": "O1-SECRET-NOTE"})
	sent := a.fcm.take()
	sort.Strings(sent)
	if want := []string{tokA1, tokA2}; !res.Data.Notified || strings.Join(sent, ",") != strings.Join(want, ",") {
		t.Errorf("check-in push went to %v (notified %v), want only parent A's two phones", sent, res.Data.Notified)
	}
	_, res = a.post(t, map[string]any{"student_id": 4101, "type": "out", "time": "12:10:30"})
	a.fcm.take()
	if got := history(4101); strings.Join(got, "\n") != "إشعار دخول|تم تسجيل دخول الطالب O1 Kid A الساعة 07:50\nإشعار خروج|تم تسجيل خروج الطالب O1 Kid A الساعة 12:10" {
		t.Errorf("parent A history %v", got)
	}

	a.punch(t, "O1-B", o1Today+" 07:50:00")
	if got := history(4102); len(got) != 1 || got[0] != "إشعار دخول|تم تسجيل دخول الطالب O1 Kid B الساعة 07:50" {
		t.Errorf("device punch history %v: the manual text must equal it but for the name", got)
	}
	if sent := a.fcm.take(); len(sent) != 1 || sent[0] != tokB {
		t.Errorf("device push went to %v", sent)
	}

	_, res = a.post(t, map[string]any{"student_id": 4102, "type": "out", "notify": false})
	if sent := a.fcm.take(); res.Data.Notified || len(sent) != 0 || len(history(4102)) != 1 {
		t.Errorf("notify false: notified %v, pushes %v, history %v", res.Data.Notified, sent, history(4102))
	}
	_, res = a.post(t, map[string]any{"student_id": 4105, "type": "in"})
	if res.Data.Notified || len(a.fcm.take()) != 0 {
		t.Errorf("a student without a parent was notified")
	}

	for _, l := range capture.snapshot() {
		line := l.Msg + fmt.Sprint(l.Attrs)
		for _, tok := range []string{tokA1, tokA2, tokB} {
			if strings.Contains(line, tok) {
				t.Errorf("full token in log: %s", line)
			}
		}
		if strings.Contains(line, "O1-SECRET") {
			t.Errorf("note in log: %s", line)
		}
	}
}

// TestO1Delete verifies the undo through the real API: only a manual entry from today or
// yesterday is deleted, everything else stays untouched, the parent's notification stays, a
// device punch is never deleted, and unknown or malformed ids are refused.
func TestO1Delete(t *testing.T) {
	srv, db, admin := l1Server(t, "o1delete", true)
	o1Seed(t, db)
	create := func(body map[string]any) int64 {
		r := a14Do(t, srv, "POST", o1Path, l1Header(admin), body)
		res := o1Decode(t, r.status, r.body)
		if r.status != 200 || res.Data.ID == nil {
			t.Fatalf("create: %d %s", r.status, r.body)
		}
		return *res.Data.ID
	}
	in := create(o1Body())
	create(o1Body("type", "out"))
	var device, older, future int64
	db.QueryRow(`INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (4102, 'O1-DEV', '2026-09-23 07:10:00') RETURNING id`).Scan(&device)
	db.QueryRow(`INSERT INTO attendance_logs (student_id, check_time, source) VALUES (4102, '2026-09-22 08:00:00', 'manual') RETURNING id`).Scan(&older)
	db.QueryRow(`INSERT INTO attendance_logs (student_id, check_time, source) VALUES (4102, '2026-09-25 08:00:00', 'manual') RETURNING id`).Scan(&future)
	k1Exec(t, db, `INSERT INTO student_leaves (student_id, leave_date, notes) VALUES (4105, '2026-09-23', 'O1 leave')`)
	snapshot := func(except int64) string {
		var s string
		db.QueryRow(`SELECT
			(SELECT string_agg(id || ' ' || student_id || ' ' || check_time || ' ' || source || ' ' || COALESCE(device_sn, '-') || ' ' || COALESCE(note, '-'), ',' ORDER BY id) FROM attendance_logs WHERE id <> $1) || ' / ' ||
			(SELECT string_agg(id || ' ' || parent_id || ' ' || title || ' ' || body, ',' ORDER BY id) FROM notifications) || ' / ' ||
			(SELECT string_agg(student_id || ' ' || leave_date, ',') FROM student_leaves)`, except).Scan(&s)
		return s
	}
	del := func(query string) a14Response {
		return a14Do(t, srv, "DELETE", o1Path+query, l1Header(admin), nil)
	}
	for _, tc := range []struct {
		query  string
		status int
		msg    string
	}{
		{"", 400, "id must be a positive attendance entry id"},
		{"?id=abc", 400, "id must be a positive attendance entry id"},
		{"?id=0", 400, "id must be a positive attendance entry id"},
		{"?id=-3", 400, "id must be a positive attendance entry id"},
		{"?id=1.5", 400, "id must be a positive attendance entry id"},
		{"?id=999999999", 404, "Attendance entry not found"},
		{fmt.Sprintf("?id=%d", device), 409, "Only manual entries can be deleted; this punch came from a device"},
		{fmt.Sprintf("?id=%d", older), 409, "Only manual entries from today (2026-09-24) or yesterday (2026-09-23) can be deleted"},
		{fmt.Sprintf("?id=%d", future), 409, "Only manual entries from today (2026-09-24) or yesterday (2026-09-23) can be deleted"},
	} {
		before := snapshot(0)
		r := del(tc.query)
		if res := o1Decode(t, r.status, r.body); r.status != tc.status || res.Message != tc.msg {
			t.Errorf("DELETE %s: %d %s, want %d %q", tc.query, r.status, r.body, tc.status, tc.msg)
		}
		if snapshot(0) != before {
			t.Errorf("DELETE %s changed the database", tc.query)
		}
	}

	before := snapshot(in)
	r := del(fmt.Sprintf("?id=%d", in))
	var res struct {
		Message string                              `json:"message"`
		Data    handlers.ManualAttendanceDeleteItem `json:"data"`
	}
	json.Unmarshal(r.body, &res)
	if got := fmt.Sprintf("%d %s %d %s %s/%s %s %s %s", r.status, res.Message, res.Data.StudentID, res.Data.StudentName, k1Str(res.Data.Grade), k1Str(res.Data.Section), res.Data.Date, res.Data.Type, res.Data.Time); got != "200 Manual attendance deleted 4101 O1 Kid A G1/A 2026-09-23 in 08:00:00" || res.Data.ID != in {
		t.Errorf("delete: %s (%s)", got, r.body)
	}
	if after := snapshot(in); after != before {
		t.Errorf("deleting one entry changed other rows:\nbefore %s\nafter  %s", before, after)
	}
	if n := k1Count(t, db, `SELECT COUNT(*) FROM attendance_logs WHERE id = $1`, in); n != 0 {
		t.Errorf("the entry is still stored")
	}
	if n := k1Count(t, db, `SELECT COUNT(*) FROM notifications WHERE parent_id = 4101 AND title = 'إشعار دخول'`); n != 1 {
		t.Errorf("the parent's notification is gone after the undo: %d", n)
	}
	if r := del(fmt.Sprintf("?id=%d", in)); r.status != 404 {
		t.Errorf("deleting twice: %d", r.status)
	}
	create(o1Body("time", "09:00"))
}

// TestO1AtomicityAndConcurrency verifies that a failing notification insert leaves nothing (no
// row, no push), and that 20 identical concurrent requests give one 200 and nineteen 409.
func TestO1AtomicityAndConcurrency(t *testing.T) {
	t.Run("a failing notification insert writes nothing", func(t *testing.T) {
		a := newO1App(t, "o1atomic")
		pinToday(t, o1Today+" 13:00")
		k1Exec(t, a.db, fmt.Sprintf(`INSERT INTO device_tokens (parent_id, token) VALUES (4101, '%s')`, b1Token("o1-atomic")))
		k1Exec(t, a.db, `ALTER TABLE notifications RENAME TO notifications_offline`)
		rec, _ := a.post(t, map[string]any{"student_id": 4101, "type": "in"})
		k1Exec(t, a.db, `ALTER TABLE notifications_offline RENAME TO notifications`)
		if rec.Code != 500 || k1Count(t, a.db, `SELECT COUNT(*) FROM attendance_logs`) != 0 || len(a.fcm.take()) != 0 {
			t.Errorf("%d %s; rows %d", rec.Code, rec.Body, k1Count(t, a.db, `SELECT COUNT(*) FROM attendance_logs`))
		}
		if rec, _ := a.post(t, map[string]any{"student_id": 4101, "type": "in"}); rec.Code != 200 {
			t.Errorf("after the repair: %d %s", rec.Code, rec.Body)
		}
	})

	srv, db, admin := l1Server(t, "o1race", true)
	o1Seed(t, db)
	race := func(t *testing.T, student int, body func(i int) map[string]any) {
		t.Helper()
		codes := make(chan int, 20)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				codes <- a14Do(t, srv, "POST", o1Path, l1Header(admin), body(i)).status
			}()
		}
		close(start)
		wg.Wait()
		close(codes)
		count := map[int]int{}
		for c := range codes {
			count[c]++
		}
		if count[200] != 1 || count[409] != 19 {
			t.Errorf("status counts %v", count)
		}
		if rows, notes := k1Count(t, db, `SELECT COUNT(*) FROM attendance_logs WHERE student_id = $1`, student), k1Count(t, db, `SELECT COUNT(*) FROM notifications WHERE parent_id = $1`, student); rows != 1 || notes != 1 {
			t.Errorf("%d rows, %d notifications; want 1 and 1", rows, notes)
		}
	}
	t.Run("20 identical concurrent requests: one 200, nineteen 409", func(t *testing.T) {
		race(t, 4101, func(int) map[string]any { return o1Body() })
	})
	t.Run("20 concurrent check-ins at different times in one window: one 200, nineteen 409", func(t *testing.T) {
		race(t, 4102, func(i int) map[string]any { return o1Body("student_id", 4102, "time", fmt.Sprintf("07:%02d", i)) })
	})
}

// TestO1AuthAndActivityLog verifies that both routes need an admin token (missing, malformed,
// expired, a parent's, or one from before a password rotation are refused and write nothing),
// and that every create, dry run, refusal and delete is in the activity log with its target and
// type, date, notify and dry_run, never the note.
func TestO1AuthAndActivityLog(t *testing.T) {
	srv, db, admin := l1Server(t, "o1auth", true)
	o1Seed(t, db)
	count := k1Count(t, db, `SELECT COUNT(*) FROM admin_activity_log`)
	send := func(method, query string, body any) a14Response {
		r := a14Do(t, srv, method, o1Path+query, l1Header(admin), body)
		count++
		l1WaitCount(t, db, count)
		return r
	}
	created := send("POST", "", o1Body("note", "O1-SECRET-NOTE"))
	id := *o1Decode(t, created.status, created.body).Data.ID
	send("POST", "", o1Body("type", "out", "dry_run", true, "notify", false))
	send("POST", "", o1Body("time", "07:00"))
	send("POST", "", o1Body("student_id", 999999))
	var device int64
	db.QueryRow(`INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (4102, 'O1-DEV', '2026-09-23 07:10:00') RETURNING id`).Scan(&device)
	send("DELETE", fmt.Sprintf("?id=%d", device), nil)
	send("DELETE", fmt.Sprintf("?id=%d", id), nil)
	send("DELETE", "?id=999999999", nil)
	var got []string
	for _, r := range l1Rows(t, db, `action LIKE 'attendance.%'`) {
		got = append(got, r.String())
	}
	want := []string{
		"admin attendance.manual_create POST /api/admin/attendance/manual 200 ip=203.0.113.7 target=student:4101 detail=type=in date=2026-09-23 notify=true",
		"admin attendance.manual_create POST /api/admin/attendance/manual 200 ip=203.0.113.7 target=student:4101 detail=type=out date=2026-09-23 notify=false dry_run",
		"admin attendance.manual_create POST /api/admin/attendance/manual 409 ip=203.0.113.7 target=student:4101 detail=type=in date=2026-09-23 notify=true",
		"admin attendance.manual_create POST /api/admin/attendance/manual 404 ip=203.0.113.7 target=student:999999 detail=type=in date=2026-09-23 notify=true",
		fmt.Sprintf("admin attendance.manual_delete DELETE /api/admin/attendance/manual 409 ip=203.0.113.7 target=student:4102 detail=id=%d type=in date=2026-09-23", device),
		fmt.Sprintf("admin attendance.manual_delete DELETE /api/admin/attendance/manual 200 ip=203.0.113.7 target=student:4101 detail=id=%d type=in date=2026-09-23", id),
		"admin attendance.manual_delete DELETE /api/admin/attendance/manual 404 ip=203.0.113.7 target=-:- detail=id=999999999",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("activity log\n got %s\nwant %s", strings.Join(got, "\n     "), strings.Join(want, "\n     "))
	}
	if n := k1Count(t, db, `SELECT COUNT(*) FROM admin_activity_log a WHERE row_to_json(a)::text LIKE '%O1-SECRET%'`); n != 0 || strings.Contains(srv.out.String(), "O1-SECRET") {
		t.Errorf("the note reached the activity log (%d rows) or the server log", n)
	}

	earlier, later := time.Now().Add(-time.Hour).Unix(), time.Now().Add(time.Hour).Unix()
	expired := e1Sign(t, jwt.MapClaims{"username": "admin", "role": "admin", "sv": 0, "exp": earlier})
	parent := e1Sign(t, jwt.MapClaims{"parent_id": 4101, "phone": "+9647000004101", "role": "parent", "sv": 0, "exp": later})
	rows := k1Count(t, db, `SELECT COUNT(*) FROM attendance_logs`)
	check := func(name, token string, want int) {
		for _, m := range []struct {
			method, query string
			body          any
		}{{"POST", "", o1Body("student_id", 4102, "type", "out")}, {"DELETE", fmt.Sprintf("?id=%d", device), nil}} {
			if r := a14Do(t, srv, m.method, o1Path+m.query, l1Header(token), m.body); r.status != want {
				t.Errorf("%s %s: %d %s, want %d", name, m.method, r.status, r.body, want)
			}
		}
	}
	check("no token", "", 401)
	check("malformed token", "not-a-jwt", 401)
	check("expired token", expired, 401)
	check("parent token", parent, 403)
	script, err := os.ReadFile(filepath.Join("..", "..", "db", "scripts", "rotate_admin_password.sql"))
	if err != nil {
		t.Fatal(err)
	}
	k1Exec(t, db, strings.ReplaceAll(string(script), "NEW_PASSWORD_HERE", "o1-rotated-password"))
	check("token from before a password rotation", admin, 401)
	if n := k1Count(t, db, `SELECT COUNT(*) FROM attendance_logs`); n != rows {
		t.Errorf("refused requests wrote %d rows", n-rows)
	}
}

// TestO1Excel verifies the manual-entry column J of the daily export: the title and closure rows
// are merged through J, the header style reaches J5, and every cell of A to I holds exactly what
// it held before the column was added, on a school day, a weekend and a holiday.
func TestO1Excel(t *testing.T) {
	a := newO1App(t, "o1excel")
	pinToday(t, o1Today+" 13:00")
	for _, b := range []map[string]any{
		{"student_id": 4101, "type": "in", "time": "07:20"},
		{"student_id": 4102, "type": "out", "time": "12:40"},
		{"student_id": 4105, "type": "in", "time": "07:30"},
		{"student_id": 4105, "type": "out", "time": "12:00"},
	} {
		if rec, _ := a.post(t, b); rec.Code != 200 {
			t.Fatalf("manual: %d %s", rec.Code, rec.Body)
		}
	}
	a.punch(t, "O1-A", o1Today+" 12:40:00")
	a.punch(t, "O1-B", o1Today+" 07:20:00")
	k1Exec(t, a.db, `INSERT INTO settings (setting_key, setting_value) VALUES ('school_name', 'مدرسة اختبار') ON CONFLICT (setting_key) DO UPDATE SET setting_value = EXCLUDED.setting_value`,
		`INSERT INTO school_closures (kind, start_date, end_date, title) VALUES ('holiday', '2026-09-22', '2026-09-22', 'عطلة اختبار')`)
	export := func(t *testing.T, date string) *excelize.File {
		t.Helper()
		rec := serve(t, a.app.AdminMiddleware(a.app.AdminExportExcelHandler), http.MethodGet, "/api/admin/export/excel?date="+date, a.admin, "")
		f, err := excelize.OpenReader(strings.NewReader(rec.Body.String()))
		if rec.Code != 200 || err != nil {
			t.Fatalf("export %s: %d %v", date, rec.Code, err)
		}
		return f
	}
	merges := func(f *excelize.File) string {
		ms, _ := f.GetMergeCells("Sheet1")
		var out []string
		for _, m := range ms {
			out = append(out, m.GetStartAxis()+":"+m.GetEndAxis())
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}
	cells := func(f *excelize.File, cols string, firstRow, lastRow int) []string {
		var out []string
		for r := firstRow; r <= lastRow; r++ {
			for _, c := range cols {
				v, _ := f.GetCellValue("Sheet1", fmt.Sprintf("%c%d", c, r))
				out = append(out, fmt.Sprintf("%c%d=%s", c, r, v))
			}
		}
		return out
	}
	headerAI := []string{"رقم الطالب", "اسم الطالب", "الصف", "الشعبة", "ولي الأمر", "رقم الهاتف", "الحالة", "وقت الدخول", "وقت الخروج"}
	title := func(date string) []string {
		return []string{"A1=وزارة التربية والتعليم", "A2=مدرسة اختبار", "A3=تقرير الحضور والغياب اليومي الشامل - تاريخ: " + date}
	}
	style := func(f *excelize.File, cell string) int {
		s, _ := f.GetCellStyle("Sheet1", cell)
		return s
	}

	t.Run("a school day", func(t *testing.T) {
		f := export(t, o1Today)
		if got := merges(f); got != "A1:J1 A2:J2 A3:J3" {
			t.Errorf("merges %s", got)
		}
		var got []string
		for _, c := range []string{"A1", "A2", "A3"} {
			v, _ := f.GetCellValue("Sheet1", c)
			got = append(got, c+"="+v)
		}
		got = append(got, cells(f, "ABCDEFGHI", 5, 9)...)
		var expect []string
		expect = append(expect, title(o1Today)...)
		for i, h := range headerAI {
			expect = append(expect, fmt.Sprintf("%c5=%s", 'A'+i, h))
		}
		rowsWant := [][]string{
			{"4101", "O1 Kid A", "G1", "A", "O1 Parent A", "+9647000004101", "حاضر", "07:20 AM", "12:40 PM"},
			{"4102", "O1 Kid B", "G1", "A", "O1 Parent B", "+9647000004102", "حاضر", "07:20 AM", "12:40 PM"},
			{"4105", "O1 Orphan", "غير محدد", "-", "-", "-", "حاضر", "07:30 AM", "12:00 PM"},
			{"4104", "O1 New", "G1", "A", "O1 Parent A", "+9647000004101", "غائب", "", ""},
		}
		for r, row := range rowsWant {
			for c, v := range row {
				expect = append(expect, fmt.Sprintf("%c%d=%s", 'A'+c, 6+r, v))
			}
		}
		if strings.Join(got, "\n") != strings.Join(expect, "\n") {
			t.Errorf("A-I cells\n got %v\nwant %v", got, expect)
		}
		for _, c := range []string{"K1", "K3", "A4", "J4", "K5", "K6", "A10", "J10"} {
			if v, _ := f.GetCellValue("Sheet1", c); v != "" {
				t.Errorf("%s = %q, want empty", c, v)
			}
		}
		if got := cells(f, "J", 5, 9); strings.Join(got, " ") != "J5=إدخال يدوي J6=الدخول J7=الخروج J8=الدخول والخروج J9=" {
			t.Errorf("column J %v", got)
		}
		if style(f, "J5") != style(f, "A5") || style(f, "J1") != style(f, "A1") || style(f, "J3") != style(f, "A3") {
			t.Errorf("J does not share the title and header styles")
		}
	})

	for _, tc := range []struct{ name, date, label string }{
		{"a weekend", "2026-09-26", "عطلة نهاية الأسبوع"},
		{"a holiday", "2026-09-22", "عطلة اختبار"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := export(t, tc.date)
			if got := merges(f); got != "A1:J1 A2:J2 A3:J3 A4:J4" {
				t.Errorf("merges %s", got)
			}
			got := cells(f, "ABCDEFGHI", 4, 6)
			var want []string
			for c := 'A'; c <= 'I'; c++ {
				want = append(want, fmt.Sprintf("%c4=%s", c, tc.label))
			}
			for i, h := range headerAI {
				want = append(want, fmt.Sprintf("%c5=%s", 'A'+i, h))
			}
			for c := 'A'; c <= 'I'; c++ {
				want = append(want, fmt.Sprintf("%c6=", c))
			}
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("rows 4-6\n got %v\nwant %v", got, want)
			}
			if v, _ := f.GetCellValue("Sheet1", "J5"); v != "إدخال يدوي" || style(f, "J4") != style(f, "A4") {
				t.Errorf("J5 %q, J4 style %d vs A4 %d", v, style(f, "J4"), style(f, "A4"))
			}
		})
	}
}

// TestO1DevicePunchesUnchanged verifies that device punches are stored as before, marked as
// device rows with their serial number and no note, through ADMS and the JSON push.
func TestO1DevicePunchesUnchanged(t *testing.T) {
	a := newO1App(t, "o1device")
	pinToday(t, o1Today+" 13:00")
	a.punch(t, "O1-A", o1Today+" 07:15:00")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/attendance/push/json?SN=O1-DEV", strings.NewReader(`{"device_sn":"O1-DEV","rfid_tag":"O1-B","push_time":"`+o1Today+` 07:16:00"}`))
	a.app.DeviceAuthMiddleware(a.app.HardwareAttendancePushHandler)(rec, req)
	a.settle(t)
	if rec.Code != 200 {
		t.Fatalf("JSON push: %d %s", rec.Code, rec.Body)
	}
	var rows string
	a.db.QueryRow(`SELECT string_agg(student_id || ' ' || device_sn || ' ' || source || ' ' || COALESCE(note, '<null>'), ',' ORDER BY id) FROM attendance_logs`).Scan(&rows)
	if rows != "4101 O1-DEV device <null>,4102 O1-DEV device <null>" {
		t.Errorf("device rows %q", rows)
	}
}

// TestO1Migration verifies migration 000032: up, down and up give the exact schemas golang-migrate
// builds for 31 and 32, re-running either file changes nothing, the down deletes manual rows and
// keeps device rows, the checks keep a device row's serial number required (also against a NULL
// insert, as 000016 did) and a manual row's empty, and golang-migrate reaches 32 from every
// version 20 to 31 marking existing punches as device punches.
func TestO1Migration(t *testing.T) {
	golden := map[string]string{}
	for _, v := range []string{"31", "32"} {
		db, run := m1Fresh(t, "o1g"+v)
		m1Run(t, run, "goto", v)
		golden[v] = m1Schema(t, db)
	}
	up := a14Migration(t, "000032_add_manual_attendance.up.sql")
	down := a14Migration(t, "000032_add_manual_attendance.down.sql")

	t.Run("up, down, up; re-runs; data; checks", func(t *testing.T) {
		db, run := m1Fresh(t, "o1steps")
		m1Run(t, run, "goto", "32")
		check := func(t *testing.T, v string) {
			t.Helper()
			m1Version(t, run, v)
			if got := m1Schema(t, db); got != golden[v] {
				t.Errorf("schema at %s differs from the one built up to %s", v, v)
			}
		}
		check(t, "32")
		k1Exec(t, db, up)
		check(t, "32")
		k1Exec(t, db,
			`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('O1-MIG', 'Gate', true)`,
			`INSERT INTO students (id, full_name, rfid_tag) VALUES (4301, 'O1 Mig', 'O1-M')`,
			`INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (4301, 'O1-MIG', '2026-09-23 07:10:00')`,
			`INSERT INTO attendance_logs (student_id, check_time, source, note) VALUES (4301, '2026-09-23 12:00:00', 'manual', 'n')`)
		for _, bad := range []string{
			`INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (4301, NULL, '2026-09-24 07:00:00')`,
			`INSERT INTO attendance_logs (student_id, check_time) VALUES (4301, '2026-09-24 07:01:00')`,
			`INSERT INTO attendance_logs (student_id, device_sn, check_time, source) VALUES (4301, 'O1-MIG', '2026-09-24 07:02:00', 'manual')`,
			`INSERT INTO attendance_logs (student_id, device_sn, check_time, note) VALUES (4301, 'O1-MIG', '2026-09-24 07:03:00', 'device note')`,
			`INSERT INTO attendance_logs (student_id, device_sn, check_time, source) VALUES (4301, 'O1-MIG', '2026-09-24 07:04:00', 'other')`,
			`INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (4301, 'NO-SUCH-DEVICE', '2026-09-24 07:05:00')`,
			`INSERT INTO attendance_logs (student_id, check_time, source, note) VALUES (4301, '2026-09-24 07:06:00', 'manual', repeat('n', 201))`,
		} {
			if _, err := db.Exec(bad); err == nil {
				t.Errorf("accepted at 32: %s", bad)
			}
		}
		m1Run(t, run, "down", "1")
		check(t, "31")
		var rows string
		db.QueryRow(`SELECT string_agg(student_id || ' ' || device_sn || ' ' || check_time, ',') FROM attendance_logs`).Scan(&rows)
		if rows != "4301 O1-MIG 2026-09-23 07:10:00" {
			t.Errorf("after the down: %q, want the device row only", rows)
		}
		if _, err := db.Exec(`INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (4301, NULL, '2026-09-24 07:00:00')`); err == nil {
			t.Errorf("a NULL device_sn is accepted at 31")
		}
		k1Exec(t, db, down)
		check(t, "31")
		m1Run(t, run, "up", "1")
		check(t, "32")
		if n := k1Count(t, db, `SELECT COUNT(*) FROM attendance_logs WHERE source = 'device' AND device_sn = 'O1-MIG' AND note IS NULL`); n != 1 {
			t.Errorf("the device row after up: %d", n)
		}
	})

	for from := 20; from <= 31; from++ {
		t.Run(fmt.Sprintf("golang-migrate from version %d to 32", from), func(t *testing.T) {
			db, run := m1Fresh(t, fmt.Sprintf("o1v%d", from))
			m1Run(t, run, "goto", fmt.Sprint(from))
			k1Exec(t, db,
				`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('O1-V', 'Gate', true)`,
				`INSERT INTO students (id, full_name, rfid_tag) VALUES (4401, 'O1 V', 'O1-V')`,
				`INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (4401, 'O1-V', '2026-09-23 07:10:00')`)
			m1Run(t, run, "goto", "32")
			m1Version(t, run, "32")
			if n := k1Count(t, db, `SELECT COUNT(*) FROM attendance_logs WHERE source = 'device' AND device_sn = 'O1-V' AND note IS NULL`); n != 1 {
				t.Errorf("existing punch after 32: %d device rows", n)
			}
		})
	}
}
