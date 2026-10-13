package warnings

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"future_kids/internal/auth"
	"future_kids/internal/background"
	"future_kids/internal/clientip"
	"future_kids/internal/handlers"
	"future_kids/internal/testdb"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// l1IP is the client address the L1 requests claim through the trusted local proxy.
const l1IP = "203.0.113.7"

type l1Row struct {
	ID         int64
	Actor      string
	Action     string
	Method     string
	Path       string
	Status     int
	IP         sql.NullString
	TargetType sql.NullString
	TargetID   sql.NullString
	Detail     sql.NullString
}

func (r l1Row) String() string {
	return fmt.Sprintf("%s %s %s %s %d ip=%s target=%s:%s detail=%s", r.Actor, r.Action, r.Method, r.Path, r.Status,
		l1Null(r.IP), l1Null(r.TargetType), l1Null(r.TargetID), l1Null(r.Detail))
}

func l1Null(s sql.NullString) string {
	if !s.Valid {
		return "-"
	}
	return s.String
}

// l1Server starts the real API (FAKE_TODAY=k1Today) and returns it, its database and an admin
// token. With trusted, 127.0.0.1 is a trusted proxy, so X-Real-IP names the client.
func l1Server(t *testing.T, label string, trusted bool) (*g3Server, *sql.DB, string) {
	t.Helper()
	db, dsn := setupThrowawayDB(t, label)
	hash, _ := bcrypt.GenerateFromPassword([]byte(a14AdminPassword), bcrypt.DefaultCost)
	k1Exec(t, db, fmt.Sprintf(`UPDATE admins SET password_hash = '%s' WHERE username = 'admin'`, hash))
	env := []string{"ABSENCE_CRON_SCHEDULE=0 0 1 1 *", "FAKE_TODAY=" + k1Today}
	if trusted {
		env = append(env, "TRUSTED_PROXY_CIDRS=127.0.0.1/32")
	}
	srv := g3Start(t, dsn, g3FreePort(t), true, env...)
	r := a14Do(t, srv, "POST", "/api/admin/login", map[string]string{"X-Real-IP": "198.51.100.200"}, map[string]string{"username": "admin", "password": a14AdminPassword})
	if r.status != 200 {
		t.Fatalf("admin login: %d %s", r.status, r.body)
	}
	l1WaitCount(t, db, 1)
	return srv, db, fmt.Sprint(r.json(t)["data"].(map[string]any)["token"])
}

func l1Header(token string) map[string]string {
	h := map[string]string{"X-Real-IP": l1IP}
	if token != "" {
		h["Authorization"] = "Bearer " + token
	}
	return h
}

func l1Rows(t *testing.T, db *sql.DB, where string, args ...any) []l1Row {
	t.Helper()
	rows, err := db.Query(`SELECT id, actor, action, method, path, status, ip, target_type, target_id, detail FROM admin_activity_log WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []l1Row
	for rows.Next() {
		var r l1Row
		if err := rows.Scan(&r.ID, &r.Actor, &r.Action, &r.Method, &r.Path, &r.Status, &r.IP, &r.TargetType, &r.TargetID, &r.Detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func l1Last(t *testing.T, db *sql.DB) l1Row {
	t.Helper()
	rows := l1Rows(t, db, `id = (SELECT max(id) FROM admin_activity_log)`)
	if len(rows) != 1 {
		t.Fatalf("the activity log is empty")
	}
	return rows[0]
}

// l1WaitCount polls the log until it holds n rows, failing after 10 seconds.
func l1WaitCount(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := k1Count(t, db, `SELECT COUNT(*) FROM admin_activity_log`)
		if got == n {
			return
		}
		if got > n || time.Now().After(deadline) {
			t.Fatalf("activity log has %d rows, want %d", got, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// l1Marker sends one recorded admin write and waits for its row, so every row of an earlier
// request is visible.
func l1Marker(t *testing.T, srv *g3Server, db *sql.DB, token, key string) {
	t.Helper()
	if r := a14Do(t, srv, "PUT", "/api/admin/settings", l1Header(token), map[string]string{"key": key, "value": "x"}); r.status != 200 {
		t.Fatalf("marker: %d %s", r.status, r.body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for k1Count(t, db, `SELECT COUNT(*) FROM admin_activity_log WHERE target_id = $1`, key) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("marker %s was not recorded", key)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestL1WritesRecorded verifies every admin write route records exactly one row, after the
// handler answered, with its action, status, client IP, target and detail, also when the request
// was rejected or failed, and the two Excel exports are recorded too.
func TestL1WritesRecorded(t *testing.T) {
	srv, db, admin := l1Server(t, "l1writes", true)
	j1Seed(t, db)
	count := k1Count(t, db, `SELECT COUNT(*) FROM admin_activity_log`)
	send := func(t *testing.T, method, path string, body any, status int) (a14Response, l1Row) {
		t.Helper()
		var r a14Response
		if form, ok := body.([]f1Part); ok {
			ct, raw := f1Form(form...)
			r = f1Do(t, srv, method, path, l1Header(admin), ct, raw)
		} else {
			r = a14Do(t, srv, method, path, l1Header(admin), body)
		}
		if r.status != status {
			t.Fatalf("%s %s: %d %s, want %d", method, path, r.status, r.body, status)
		}
		count++
		l1WaitCount(t, db, count)
		return r, l1Last(t, db)
	}
	expect := func(t *testing.T, row l1Row, want string) {
		t.Helper()
		if got := row.String(); got != want {
			t.Errorf("row\n got %s\nwant %s", got, want)
		}
	}
	var studentID int

	t.Run("students", func(t *testing.T) {
		r, row := send(t, "POST", "/api/admin/students", map[string]any{"name": "L1 Kid", "parent_name": "L1 Parent", "parent_phone": "07000031001", "parent_pin": "Zq8#vR2m!Tx9pW4z", "rfid_tag": "L1-1", "grade": "G1", "section": "A"}, 200)
		studentID = int(r.json(t)["data"].(map[string]any)["id"].(float64))
		expect(t, row, fmt.Sprintf("admin student.create POST /api/admin/students 200 ip=%s target=student:%d detail=-", l1IP, studentID))
		_, row = send(t, "PUT", "/api/admin/students", map[string]any{"id": studentID, "name": "L1 Kid", "parent_name": "L1 Parent", "parent_phone": "07000031001", "rfid_tag": "L1-1"}, 200)
		expect(t, row, fmt.Sprintf("admin student.update PUT /api/admin/students 200 ip=%s target=student:%d detail=-", l1IP, studentID))
		_, row = send(t, "PUT", "/api/admin/students", map[string]any{"id": 999999, "name": "X", "parent_name": "L1 Parent", "parent_phone": "07000031001", "rfid_tag": "L1-9"}, 404)
		expect(t, row, fmt.Sprintf("admin student.update PUT /api/admin/students 404 ip=%s target=student:999999 detail=-", l1IP))
		_, row = send(t, "POST", "/api/admin/students", map[string]any{"name": "L1 Twin", "parent_name": "L1 Parent", "parent_phone": "07000031001", "rfid_tag": "L1-1"}, 409)
		expect(t, row, fmt.Sprintf("admin student.create POST /api/admin/students 409 ip=%s target=-:- detail=-", l1IP))
		_, row = send(t, "POST", "/api/admin/students", map[string]any{"parent_name": "x"}, 400)
		expect(t, row, fmt.Sprintf("admin student.create POST /api/admin/students 400 ip=%s target=-:- detail=-", l1IP))
	})

	t.Run("devices", func(t *testing.T) {
		_, row := send(t, "POST", "/api/admin/devices", map[string]any{"serial_number": "L1-DEV-1", "location_name": "Gate"}, 200)
		expect(t, row, "admin device.create POST /api/admin/devices 200 ip="+l1IP+" target=device:L1-DEV-1 detail=-")
		_, row = send(t, "POST", "/api/admin/devices", map[string]any{"serial_number": "L1-DEV-1"}, 409)
		expect(t, row, "admin device.create POST /api/admin/devices 409 ip="+l1IP+" target=device:L1-DEV-1 detail=-")
		_, row = send(t, "PUT", "/api/admin/devices", map[string]any{"serial_number": "L1-DEV-1", "is_active": true}, 200)
		expect(t, row, "admin device.update PUT /api/admin/devices 200 ip="+l1IP+" target=device:L1-DEV-1 detail=-")
		_, row = send(t, "DELETE", "/api/admin/devices?sn=L1-DEV-1", nil, 200)
		expect(t, row, "admin device.disable DELETE /api/admin/devices 200 ip="+l1IP+" target=device:L1-DEV-1 detail=-")
	})

	t.Run("leaves", func(t *testing.T) {
		_, row := send(t, "POST", "/api/admin/leaves", map[string]any{"student_id": studentID, "leave_date": k1Today}, 200)
		expect(t, row, fmt.Sprintf("admin leave.create POST /api/admin/leaves 200 ip=%s target=student:%d detail=date=%s", l1IP, studentID, k1Today))
		_, row = send(t, "POST", "/api/admin/leaves", map[string]any{"student_id": 999999, "leave_date": k1Today}, 404)
		expect(t, row, fmt.Sprintf("admin leave.create POST /api/admin/leaves 404 ip=%s target=student:999999 detail=date=%s", l1IP, k1Today))
		_, row = send(t, "DELETE", fmt.Sprintf("/api/admin/leaves?student_id=%d&date=%s", studentID, k1Today), nil, 200)
		expect(t, row, fmt.Sprintf("admin leave.delete DELETE /api/admin/leaves 200 ip=%s target=student:%d detail=date=%s", l1IP, studentID, k1Today))
	})

	t.Run("settings: the key, never the value; a forced 500 is failed", func(t *testing.T) {
		_, row := send(t, "PUT", "/api/admin/settings", map[string]string{"key": "school_name", "value": "Fake School"}, 200)
		expect(t, row, "admin settings.update PUT /api/admin/settings 200 ip="+l1IP+" target=setting:school_name detail=-")
		k1Exec(t, db, `ALTER TABLE settings ADD CONSTRAINT l1_refuse CHECK (setting_value <> 'Poison')`)
		defer db.Exec(`ALTER TABLE settings DROP CONSTRAINT l1_refuse`)
		_, row = send(t, "PUT", "/api/admin/settings", map[string]string{"key": "school_name", "value": "Poison"}, 500)
		expect(t, row, "admin settings.update PUT /api/admin/settings 500 ip="+l1IP+" target=setting:school_name detail=-")
		if handlers.ActivityResult(row.Status) != "failed" {
			t.Errorf("a 500 is %q", handlers.ActivityResult(row.Status))
		}
	})

	t.Run("two identical writes give two rows", func(t *testing.T) {
		before := k1Count(t, db, `SELECT COUNT(*) FROM admin_activity_log WHERE target_id = 'whatsapp_number'`)
		send(t, "PUT", "/api/admin/settings", map[string]string{"key": "whatsapp_number", "value": "+9647000000999"}, 200)
		send(t, "PUT", "/api/admin/settings", map[string]string{"key": "whatsapp_number", "value": "+9647000000999"}, 200)
		if n := k1Count(t, db, `SELECT COUNT(*) FROM admin_activity_log WHERE target_id = 'whatsapp_number'`); n != before+2 {
			t.Errorf("%d rows, want %d", n, before+2)
		}
	})

	t.Run("deactivating a student records the id, also for an unknown one", func(t *testing.T) {
		_, row := send(t, "DELETE", fmt.Sprintf("/api/admin/students?id=%d", studentID), nil, 200)
		expect(t, row, fmt.Sprintf("admin student.deactivate DELETE /api/admin/students 200 ip=%s target=student:%d detail=-", l1IP, studentID))
		_, row = send(t, "DELETE", "/api/admin/students?id=999999", nil, 404)
		expect(t, row, "admin student.deactivate DELETE /api/admin/students 404 ip="+l1IP+" target=student:999999 detail=-")
		_, row = send(t, "DELETE", "/api/admin/students?id=abc", nil, 400)
		expect(t, row, "admin student.deactivate DELETE /api/admin/students 400 ip="+l1IP+" target=-:- detail=-")
	})

	t.Run("schedule save, import and export", func(t *testing.T) {
		_, row := send(t, "PUT", "/api/admin/schedule", e1Body("G1", "A", e1Day("الأحد", 5, "Math")...), 200)
		expect(t, row, "admin schedule.save PUT /api/admin/schedule 200 ip="+l1IP+" target=class:G1/A detail=periods=5")
		_, row = send(t, "PUT", "/api/admin/schedule", e1Body("G1", "A", e1Day("الأحد", 3, "Math")...), 400)
		expect(t, row, "admin schedule.save PUT /api/admin/schedule 400 ip="+l1IP+" target=-:- detail=-")
		book := i1Book(t, i1Sheet{"Sheet1", i1Table(i1Rows("G1", "A", map[string]int{"الأحد": 5, "الإثنين": 6}, "Math", ""))})
		_, row = send(t, "POST", "/api/admin/schedule/import", []f1Part{{"file", "s.xlsx", handlers.XLSXContentType, book}, f1Text("dry_run", "true"), f1Text("grade", "G1"), f1Text("section", "A")}, 200)
		expect(t, row, "admin schedule.import POST /api/admin/schedule/import 200 ip="+l1IP+" target=class:G1/A detail=classes=1 periods=11 dry_run")
		_, row = send(t, "POST", "/api/admin/schedule/import", []f1Part{{"file", "s.xlsx", handlers.XLSXContentType, book}}, 200)
		expect(t, row, "admin schedule.import POST /api/admin/schedule/import 200 ip="+l1IP+" target=-:- detail=classes=1 periods=11")
		_, row = send(t, "POST", "/api/admin/schedule/import", []f1Part{{"file", "s.xlsx", handlers.XLSXContentType, []byte("not a workbook")}}, 400)
		expect(t, row, "admin schedule.import POST /api/admin/schedule/import 400 ip="+l1IP+" target=-:- detail=-")
		_, row = send(t, "GET", "/api/admin/schedule/export?grade=G1&section=A", nil, 200)
		expect(t, row, "admin export.schedule GET /api/admin/schedule/export 200 ip="+l1IP+" target=class:G1/A detail=-")
		_, row = send(t, "GET", "/api/admin/export/excel?date="+k1Today, nil, 200)
		expect(t, row, "admin export.attendance GET /api/admin/export/excel 200 ip="+l1IP+" target=date:"+k1Today+" detail=-")
		_, row = send(t, "GET", "/api/admin/export/excel?date=nope", nil, 400)
		expect(t, row, "admin export.attendance GET /api/admin/export/excel 400 ip="+l1IP+" target=-:- detail=-")
	})

	t.Run("broadcasts", func(t *testing.T) {
		_, row := send(t, "POST", "/api/admin/broadcasts", j1Body("T", "B", j1All(), "dry_run", true), 200)
		expect(t, row, "admin broadcast.send POST /api/admin/broadcasts 200 ip="+l1IP+" target=-:- detail=audience=all recipients=7 dry_run")
		r, row := send(t, "POST", "/api/admin/broadcasts", j1Body("T", "B", j1Class("G1", "A")), 200)
		id := int(r.json(t)["data"].(map[string]any)["id"].(float64))
		expect(t, row, fmt.Sprintf("admin broadcast.send POST /api/admin/broadcasts 200 ip=%s target=broadcast:%d detail=audience=class recipients=2", l1IP, id))
		_, row = send(t, "POST", "/api/admin/broadcasts", j1Body("T", "B", j1Class("G1", "A")), 409)
		expect(t, row, "admin broadcast.send POST /api/admin/broadcasts 409 ip="+l1IP+" target=-:- detail=audience=class")
	})

	t.Run("holidays", func(t *testing.T) {
		_, row := send(t, "POST", "/api/admin/holidays", k1Closure("kind", "pause", "start_date", "2027-06-01", "title", "x", "dry_run", true), 200)
		expect(t, row, "admin holiday.create POST /api/admin/holidays 200 ip="+l1IP+" target=-:- detail=kind=pause dry_run")
		r, row := send(t, "POST", "/api/admin/holidays", k1Closure("kind", "holiday", "start_date", "2026-10-11", "title", "x", "notify", false), 200)
		id := int(r.json(t)["data"].(map[string]any)["id"].(float64))
		expect(t, row, fmt.Sprintf("admin holiday.create POST /api/admin/holidays 200 ip=%s target=closure:%d detail=kind=holiday", l1IP, id))
		_, row = send(t, "POST", "/api/admin/holidays", k1Closure("kind", "holiday", "start_date", "2026-10-11", "title", "x", "notify", false), 409)
		expect(t, row, "admin holiday.create POST /api/admin/holidays 409 ip="+l1IP+" target=-:- detail=kind=holiday")
		_, row = send(t, "POST", "/api/admin/holidays/cancel", map[string]any{"id": id, "notify": false}, 200)
		expect(t, row, fmt.Sprintf("admin holiday.cancel POST /api/admin/holidays/cancel 200 ip=%s target=closure:%d detail=kind=holiday action=deleted", l1IP, id))
		_, row = send(t, "POST", "/api/admin/holidays/cancel", map[string]any{"id": id}, 404)
		expect(t, row, fmt.Sprintf("admin holiday.cancel POST /api/admin/holidays/cancel 404 ip=%s target=closure:%d detail=-", l1IP, id))
	})

	t.Run("banners", func(t *testing.T) {
		r, row := send(t, "POST", "/api/admin/banners", []f1Part{f1Image(f1JPEG(t, 1))}, 200)
		id := f1ID(t, r)
		expect(t, row, fmt.Sprintf("admin banner.create POST /api/admin/banners 200 ip=%s target=banner:%d detail=-", l1IP, id))
		_, row = send(t, "PUT", fmt.Sprintf("/api/admin/banners?id=%d", id), []f1Part{f1Text("title", "L1")}, 200)
		expect(t, row, fmt.Sprintf("admin banner.update PUT /api/admin/banners 200 ip=%s target=banner:%d detail=-", l1IP, id))
		_, row = send(t, "DELETE", fmt.Sprintf("/api/admin/banners?id=%d", id), nil, 200)
		expect(t, row, fmt.Sprintf("admin banner.delete DELETE /api/admin/banners 200 ip=%s target=banner:%d detail=-", l1IP, id))
		_, row = send(t, "DELETE", fmt.Sprintf("/api/admin/banners?id=%d", id), nil, 404)
		expect(t, row, fmt.Sprintf("admin banner.delete DELETE /api/admin/banners 404 ip=%s target=banner:%d detail=-", l1IP, id))
	})

	t.Run("results", func(t *testing.T) {
		for _, tc := range []struct {
			status int
			want   string
		}{{200, "success"}, {302, "success"}, {400, "rejected"}, {404, "rejected"}, {409, "rejected"}, {413, "rejected"}, {500, "failed"}, {503, "failed"}} {
			if got := handlers.ActivityResult(tc.status); got != tc.want {
				t.Errorf("%d: %s, want %s", tc.status, got, tc.want)
			}
		}
	})
}

// TestL1NotRecorded verifies reads, the log itself, requests the admin authentication rejects,
// parent and device requests, and logins answered 429 or rejected as malformed write nothing.
func TestL1NotRecorded(t *testing.T) {
	srv, db, admin := l1Server(t, "l1quiet", true)
	j1Seed(t, db)
	k1Exec(t, db, `INSERT INTO devices (serial_number, location_name, is_active) VALUES ('L1-DEV', 'Gate', true)`, `INSERT INTO students (full_name, rfid_tag, parent_id) VALUES ('L1 Kid', 'L1-RF', 3001)`)
	l1Marker(t, srv, db, admin, "l1_start")
	for _, path := range []string{
		"/api/admin/dashboard", "/api/admin/students", "/api/admin/devices", "/api/admin/attendance", "/api/admin/settings",
		"/api/admin/schedule?grade=G1&section=A", "/api/admin/schedule/classes", "/api/admin/broadcasts", "/api/admin/holidays",
		"/api/admin/banners", "/api/admin/banners/image?id=1", "/api/admin/activity-log",
	} {
		a14Do(t, srv, "GET", path, l1Header(admin), nil)
	}
	for _, path := range []string{"/api/admin/export/excel", "/api/admin/schedule/export?grade=G1&section=A"} {
		a14Do(t, srv, "HEAD", path, l1Header(admin), nil)
	}
	later, earlier := time.Now().Add(time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
	expired := e1Sign(t, jwt.MapClaims{"username": "admin", "role": "admin", "sv": 0, "exp": earlier})
	parent := e1Sign(t, jwt.MapClaims{"parent_id": 3001, "phone": "+9647000003001", "role": "parent", "sv": 0, "exp": later})
	setting := map[string]string{"key": "school_name", "value": "x"}
	for _, h := range []map[string]string{l1Header(""), l1Header("not-a-jwt"), l1Header(expired), l1Header(parent)} {
		if r := a14Do(t, srv, "PUT", "/api/admin/settings", h, setting); r.status != 401 && r.status != 403 {
			t.Errorf("rejected token: %d", r.status)
		}
	}
	a14Do(t, srv, "POST", "/iclock/cdata?SN=L1-DEV&table=ATTLOG", nil, "L1-RF\t"+k1Today+" 07:15:00\t1\t1\n")
	a14Do(t, srv, "PUT", "/api/mobile/device-token", l1Header(parent), map[string]string{"token": "l1-device-token-000000000001"})
	a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": "07000003001", "pin": "wrong"})
	for _, h := range []map[string]string{l1Header(admin)} {
		for _, m := range []string{"POST", "PUT", "DELETE"} {
			if r := a14Do(t, srv, m, "/api/admin/activity-log", h, nil); r.status != 405 {
				t.Errorf("%s activity-log: %d", m, r.status)
			}
		}
	}
	a14Do(t, srv, "POST", "/api/admin/login", map[string]string{"X-Real-IP": "198.51.100.77"}, "{")
	a14Do(t, srv, "POST", "/api/admin/login", map[string]string{"X-Real-IP": "198.51.100.77"}, `{"username":"admin","password":"`+strings.Repeat("a", 70<<10)+`"}`)
	l1Marker(t, srv, db, admin, "l1_end")
	if rows := l1Rows(t, db, `id > (SELECT id FROM admin_activity_log WHERE target_id = 'l1_start')`); len(rows) != 1 || rows[0].TargetID.String != "l1_end" {
		t.Errorf("unrecorded requests wrote %v", rows)
	}

	t.Run("a token from before a password rotation", func(t *testing.T) {
		script, err := os.ReadFile(filepath.Join("..", "..", "db", "scripts", "rotate_admin_password.sql"))
		if err != nil {
			t.Fatal(err)
		}
		k1Exec(t, db, strings.ReplaceAll(string(script), "NEW_PASSWORD_HERE", "l1-rotated-password"))
		n := k1Count(t, db, `SELECT COUNT(*) FROM admin_activity_log`)
		if r := a14Do(t, srv, "PUT", "/api/admin/settings", l1Header(admin), setting); r.status != 401 {
			t.Fatalf("old token: %d", r.status)
		}
		r := a14Do(t, srv, "POST", "/api/admin/login", map[string]string{"X-Real-IP": l1IP}, map[string]string{"username": "admin", "password": "l1-rotated-password"})
		if r.status != 200 {
			t.Fatalf("login: %d", r.status)
		}
		l1WaitCount(t, db, n+1)
		if row := l1Last(t, db); row.Action != "login.success" {
			t.Errorf("only the login may be recorded: %s", row)
		}
	})
}

// TestL1Login verifies admin logins: success records the account and IP, a failure records
// "unknown" and the IP and never the submitted username or password, the rate limit works as
// before and its 429 is not recorded.
func TestL1Login(t *testing.T) {
	srv, db, _ := l1Server(t, "l1login", true)
	login := func(ip, user, password string) a14Response {
		return a14Do(t, srv, "POST", "/api/admin/login", map[string]string{"X-Real-IP": ip}, map[string]string{"username": user, "password": password})
	}
	n := k1Count(t, db, `SELECT COUNT(*) FROM admin_activity_log`)
	if r := login("192.0.2.10", "admin", a14AdminPassword); r.status != 200 {
		t.Fatalf("login: %d", r.status)
	}
	l1WaitCount(t, db, n+1)
	if row := l1Last(t, db); row.String() != "admin login.success POST /api/admin/login 200 ip=192.0.2.10 target=-:- detail=-" {
		t.Errorf("success row %s", row)
	}
	for i := 1; i <= 5; i++ {
		if r := login("192.0.2.11", "l1-secret-user", "L1-Secret-Password!"); r.status != 401 {
			t.Fatalf("failure %d: %d", i, r.status)
		}
	}
	r := login("192.0.2.11", "l1-secret-user", "L1-Secret-Password!")
	if r.status != 429 {
		t.Fatalf("sixth failure: %d, want 429", r.status)
	}
	a14Retry(t, r, 900)
	if r := login("192.0.2.12", "admin", a14AdminPassword); r.status != 200 {
		t.Fatalf("login from another IP: %d", r.status)
	}
	l1WaitCount(t, db, n+7)
	rows := l1Rows(t, db, `ip = '192.0.2.11'`)
	if len(rows) != 5 {
		t.Fatalf("%d rows for the failing IP, want 5 (the 429 is not recorded)", len(rows))
	}
	for _, row := range rows {
		if row.String() != "unknown login.failed POST /api/admin/login 401 ip=192.0.2.11 target=-:- detail=-" {
			t.Errorf("failure row %s", row)
		}
	}
	var dump string
	db.QueryRow(`SELECT string_agg(row_to_json(a)::text, ' ') FROM admin_activity_log a`).Scan(&dump)
	if strings.Contains(dump, "l1-secret-user") || strings.Contains(dump, "L1-Secret-Password") {
		t.Errorf("the submitted username or password was stored")
	}
}

// TestL1Secrets plants a PIN, password, token, names, a phone number and notes in admin requests
// and finds none of them in any column of the log.
func TestL1Secrets(t *testing.T) {
	srv, db, admin := l1Server(t, "l1secrets", true)
	h := l1Header(admin)
	secrets := []string{"Zq8#L1SecretPin!42x", "L1SecretPassword!", admin, "L1SecretChild", "L1SecretParent", "7000099123", "L1SecretNote", "L1SecretTitle", "L1SecretBody", "L1SecretValue", "L1SecretLocation"}
	r := a14Do(t, srv, "POST", "/api/admin/students", h, map[string]any{"name": "L1SecretChild", "parent_name": "L1SecretParent", "parent_phone": "07000099123", "parent_pin": "Zq8#L1SecretPin!42x", "rfid_tag": "L1-S1"})
	if r.status != 200 {
		t.Fatalf("student: %d %s", r.status, r.body)
	}
	id := int(r.json(t)["data"].(map[string]any)["id"].(float64))
	for _, req := range []struct {
		method, path string
		body         any
	}{
		{"PUT", "/api/admin/students", map[string]any{"id": id, "name": "L1SecretChild", "parent_name": "L1SecretParent", "parent_phone": "07000099123", "parent_pin": "Zq8#L1SecretPin!42x", "rfid_tag": "L1-S1"}},
		{"POST", "/api/admin/leaves", map[string]any{"student_id": id, "leave_date": k1Today, "notes": "L1SecretNote"}},
		{"POST", "/api/admin/broadcasts", j1Body("L1SecretTitle", "L1SecretBody", j1All())},
		{"POST", "/api/admin/holidays", k1Closure("kind", "holiday", "start_date", "2026-10-11", "title", "L1SecretTitle", "notes", "L1SecretNote")},
		{"PUT", "/api/admin/settings", map[string]string{"key": "school_name", "value": "L1SecretValue"}},
		{"POST", "/api/admin/devices", map[string]any{"serial_number": "L1-SD", "location_name": "L1SecretLocation"}},
		{"PUT", "/api/admin/schedule", e1Body("G1", "A", e1Day("الأحد", 4, "L1SecretTitle")...)},
	} {
		a14Do(t, srv, req.method, req.path, h, req.body)
	}
	a14Do(t, srv, "POST", "/api/admin/login", map[string]string{"X-Real-IP": l1IP}, map[string]string{"username": "admin", "password": "L1SecretPassword!"})
	l1Marker(t, srv, db, admin, "l1_secrets_done")
	var dump string
	db.QueryRow(`SELECT string_agg(row_to_json(a)::text, ' ') FROM admin_activity_log a`).Scan(&dump)
	if k1Count(t, db, `SELECT COUNT(*) FROM admin_activity_log`) < 10 {
		t.Fatalf("too few rows recorded: %s", dump)
	}
	for _, s := range secrets {
		if strings.Contains(dump, s) {
			t.Errorf("the log holds %q", s)
		}
	}
}

// TestL1Wrapper verifies the wrapper in-process: the actor comes from the token, details are
// truncated by characters, a panic is recorded as 500 and re-raised unchanged, exports and
// response-controller deadlines pass through untouched, setting keys are joined and truncated,
// and the log table has no triggers or rules and can be corrected by plain SQL.
func TestL1Wrapper(t *testing.T) {
	db, _ := setupThrowawayDB(t, "l1wrap")
	auth.InitAuth("l1-test-secret")
	bg := &background.Group{}
	app := &handlers.AppEnv{DB: db, Background: bg, ClientIP: clientip.Resolver{}}
	settle := func(t *testing.T) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if pending, err := bg.Wait(ctx); err != nil {
			t.Fatalf("background work still running: %d", pending)
		}
	}
	mux := func(pattern string, h http.HandlerFunc) http.Handler {
		m := http.NewServeMux()
		m.HandleFunc(pattern, app.AdminMiddleware(app.ActivityLog(h)))
		return m
	}
	last := func(t *testing.T) l1Row { t.Helper(); return l1Last(t, db) }
	ar := func(n int) string { return strings.Repeat("ع", n) }

	t.Run("actor from the token claims; detail truncated by characters", func(t *testing.T) {
		k1Exec(t, db, `INSERT INTO admins (username, password_hash) VALUES ('l1-other', 'x')`)
		token, _ := auth.GenerateAdminToken("l1-other", 0)
		h := mux("POST /api/admin/l1-fake", func(w http.ResponseWriter, r *http.Request) {
			handlers.NoteActivity(r, "l1", ar(70), ar(250))
			w.WriteHeader(http.StatusCreated)
		})
		req := httptest.NewRequest("POST", "/api/admin/l1-fake", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.RemoteAddr = "192.0.2.50:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		settle(t)
		row := last(t)
		if row.Actor != "l1-other" || row.Action != "l1-fake.create" || row.Status != 201 || row.IP.String != "192.0.2.50" {
			t.Errorf("row %s", row)
		}
		if n := utf8.RuneCountInString(row.Detail.String); n != 200 || !strings.HasSuffix(row.Detail.String, "…") {
			t.Errorf("detail of %d characters", n)
		}
		if n := utf8.RuneCountInString(row.TargetID.String); n != 64 {
			t.Errorf("target_id of %d characters", n)
		}
	})

	token, _ := auth.GenerateAdminToken("admin", 0)
	t.Run("a panic is recorded as 500 and re-raised unchanged", func(t *testing.T) {
		boom := func(w http.ResponseWriter, r *http.Request) { panic("l1 boom") }
		h := mux("DELETE /api/admin/l1-boom", boom)
		req := httptest.NewRequest("DELETE", "/api/admin/l1-boom", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		got := func() (p any) {
			defer func() { p = recover() }()
			h.ServeHTTP(httptest.NewRecorder(), req)
			return nil
		}()
		if got != "l1 boom" {
			t.Fatalf("recovered %v, want the handler's panic value", got)
		}
		settle(t)
		if row := last(t); row.Status != 500 || row.Action != "l1-boom.delete" {
			t.Errorf("row %s", row)
		}
		wrapped := httptest.NewServer(mux("DELETE /api/admin/l1-boom", boom))
		defer wrapped.Close()
		plain := httptest.NewServer(http.HandlerFunc(boom))
		defer plain.Close()
		for name, base := range map[string]string{"wrapped": wrapped.URL, "plain": plain.URL} {
			req, _ := http.NewRequest("DELETE", base+"/api/admin/l1-boom", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
				t.Errorf("%s: a panicking handler answered %d; the server should drop the connection", name, resp.StatusCode)
			}
		}
		settle(t)
	})

	t.Run("exports are byte-identical through the wrapper", func(t *testing.T) {
		k1Exec(t, db, `INSERT INTO students (full_name, rfid_tag, grade, section) VALUES ('L1 Export', 'L1-E', 'G1', 'A')`,
			`INSERT INTO weekly_schedules (grade, section, day_of_week, period_number, subject_name) SELECT 'G1', 'A', 'الأحد', g, 'Math' FROM generate_series(1, 5) g`)
		for _, tc := range []struct {
			pattern, target string
			h               http.HandlerFunc
		}{
			{"GET /api/admin/export/excel", "/api/admin/export/excel?date=2026-09-24", app.AdminExportExcelHandler},
			{"GET /api/admin/schedule/export", "/api/admin/schedule/export?grade=G1&section=A", app.AdminScheduleExportHandler},
		} {
			call := func(h http.Handler) *httptest.ResponseRecorder {
				req := httptest.NewRequest("GET", tc.target, nil)
				req.Header.Set("Authorization", "Bearer "+token)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				return rec
			}
			plainMux := http.NewServeMux()
			plainMux.HandleFunc(tc.pattern, app.AdminMiddleware(tc.h))
			n := k1Count(t, db, `SELECT COUNT(*) FROM admin_activity_log`)
			a, b := call(plainMux), call(mux(tc.pattern, tc.h))
			settle(t)
			if a.Code != 200 || a.Code != b.Code || !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) || fmt.Sprint(a.Header()) != fmt.Sprint(b.Header()) {
				t.Errorf("%s: %d %v (%d bytes) vs wrapped %d %v (%d bytes)", tc.pattern, a.Code, a.Header(), a.Body.Len(), b.Code, b.Header(), b.Body.Len())
			}
			if k1Count(t, db, `SELECT COUNT(*) FROM admin_activity_log`) != n+1 {
				t.Errorf("%s: the wrapped export was not recorded once", tc.pattern)
			}
		}
	})

	t.Run("response-controller deadlines still work through the wrapper", func(t *testing.T) {
		srv := httptest.NewServer(mux("POST /api/admin/l1-upload", func(w http.ResponseWriter, r *http.Request) {
			rc := http.NewResponseController(w)
			err1 := rc.SetReadDeadline(time.Now().Add(time.Minute))
			err2 := rc.SetWriteDeadline(time.Now().Add(time.Minute))
			io.Copy(io.Discard, r.Body)
			fmt.Fprint(w, err1 == nil && err2 == nil)
		}))
		defer srv.Close()
		req, _ := http.NewRequest("POST", srv.URL+"/api/admin/l1-upload", bytes.NewReader(make([]byte, 3<<20)))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "true" {
			t.Errorf("deadlines through the wrapper: %s", body)
		}
		settle(t)
	})

	t.Run("setting keys are joined and truncated", func(t *testing.T) {
		if got := handlers.SettingKeysTarget("school_name", "whatsapp_number"); got != "school_name,whatsapp_number" {
			t.Errorf("two keys: %q", got)
		}
		if got := handlers.SettingKeysTarget(ar(40), ar(40)); utf8.RuneCountInString(got) != 64 {
			t.Errorf("long keys: %d characters", utf8.RuneCountInString(got))
		}
	})

	t.Run("the owner can correct rows by plain SQL", func(t *testing.T) {
		if n := k1Count(t, db, `SELECT COUNT(*) FROM pg_trigger WHERE tgrelid = 'admin_activity_log'::regclass AND NOT tgisinternal`); n != 0 {
			t.Errorf("%d triggers on the log", n)
		}
		if n := k1Count(t, db, `SELECT COUNT(*) FROM pg_rules WHERE tablename = 'admin_activity_log'`); n != 0 {
			t.Errorf("%d rules on the log", n)
		}
		res, err := db.Exec(`UPDATE admin_activity_log SET detail = 'corrected by the owner'`)
		if n, _ := res.RowsAffected(); err != nil || n == 0 {
			t.Errorf("UPDATE: %v (%d rows)", err, n)
		}
		res, err = db.Exec(`DELETE FROM admin_activity_log`)
		if n, _ := res.RowsAffected(); err != nil || n == 0 {
			t.Errorf("DELETE: %v (%d rows)", err, n)
		}
	})
}

// TestL1BestEffort verifies a failing log never changes a response: with the table renamed
// every admin action answers exactly as with it, and each failure is one ERROR line.
func TestL1BestEffort(t *testing.T) {
	srv, db, admin := l1Server(t, "l1effort", true)
	j1Seed(t, db)
	requests := []struct {
		method, path string
		body         any
	}{
		{"PUT", "/api/admin/settings", map[string]string{"key": "school_name", "value": "Fake School"}},
		{"DELETE", "/api/admin/leaves?student_id=1&date=" + k1Today, nil},
		{"POST", "/api/admin/broadcasts", j1Body("T", "B", j1All(), "dry_run", true)},
		{"POST", "/api/admin/holidays", k1Closure("kind", "holiday", "start_date", "2026-10-11", "title", "x", "dry_run", true)},
		{"DELETE", "/api/admin/students?id=999999", nil},
		{"POST", "/api/admin/devices", map[string]any{}},
		{"GET", "/api/admin/schedule/export?grade=G9&section=Z", nil},
	}
	answers := func() []string {
		var out []string
		for _, rq := range requests {
			r := a14Do(t, srv, rq.method, rq.path, l1Header(admin), rq.body)
			out = append(out, fmt.Sprintf("%d %s %s", r.status, r.header.Get("Content-Type"), r.body))
		}
		return out
	}
	with := answers()
	l1WaitCount(t, db, 1+len(requests))
	k1Exec(t, db, `ALTER TABLE admin_activity_log RENAME TO admin_activity_log_offline`)
	without := answers()
	k1Exec(t, db, `ALTER TABLE admin_activity_log_offline RENAME TO admin_activity_log`)
	if strings.Join(with, "\n") != strings.Join(without, "\n") {
		t.Errorf("answers changed without the log\nwith:\n%s\nwithout:\n%s", strings.Join(with, "\n"), strings.Join(without, "\n"))
	}
	deadline := time.Now().Add(10 * time.Second)
	for strings.Count(srv.out.String(), `"msg":"Activity log: insert failed"`) < len(requests) {
		if time.Now().After(deadline) {
			t.Fatalf("%d ERROR lines, want %d", strings.Count(srv.out.String(), `"msg":"Activity log: insert failed"`), len(requests))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := strings.Count(srv.out.String(), `"level":"ERROR","msg":"Activity log: insert failed"`); n != len(requests) {
		t.Errorf("%d ERROR lines, want one per request (%d)", n, len(requests))
	}
}

// TestL1ConcurrencyAndIP verifies 50 concurrent writes give 50 rows with unique ids and no 500,
// that a trusted proxy's X-Real-IP is recorded, that an untrusted one is ignored, and that a 413
// still closes the connection.
func TestL1ConcurrencyAndIP(t *testing.T) {
	srv, db, admin := l1Server(t, "l1busy", true)
	n := k1Count(t, db, `SELECT COUNT(*) FROM admin_activity_log`)
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _, err := e1Send(srv, "PUT", "/api/admin/settings", admin, map[string]string{"key": fmt.Sprintf("l1_key_%02d", i), "value": "v"})
			if err != nil {
				code = -1
			}
			mu.Lock()
			codes[code]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if codes[200] != 50 {
		t.Errorf("status counts %v, want 50 × 200", codes)
	}
	l1WaitCount(t, db, n+50)
	if k1Count(t, db, `SELECT COUNT(DISTINCT id) FROM admin_activity_log WHERE target_id LIKE 'l1_key_%'`) != 50 || k1Count(t, db, `SELECT COUNT(*) FROM admin_activity_log WHERE status >= 500`) != 0 {
		t.Errorf("concurrent rows are not 50 unique successes")
	}

	t.Run("a trusted proxy's client address is recorded", func(t *testing.T) {
		l1Marker(t, srv, db, admin, "l1_ip_trusted")
		if row := l1Rows(t, db, `target_id = 'l1_ip_trusted'`)[0]; row.IP.String != l1IP {
			t.Errorf("ip %q, want %s", row.IP.String, l1IP)
		}
	})

	t.Run("an untrusted X-Real-IP is ignored", func(t *testing.T) {
		usrv, udb, uadmin := l1Server(t, "l1untrusted", false)
		l1Marker(t, usrv, udb, uadmin, "l1_ip_untrusted")
		if row := l1Rows(t, udb, `target_id = 'l1_ip_untrusted'`)[0]; row.IP.String != "127.0.0.1" {
			t.Errorf("ip %q, want the connection's 127.0.0.1", row.IP.String)
		}
	})

	t.Run("an oversized body still gets Connection: close", func(t *testing.T) {
		for _, path := range []string{"/api/admin/broadcasts", "/api/admin/holidays", "/api/mobile/login"} {
			conn, err := net.Dial("tcp", strings.TrimPrefix(srv.url(""), "http://"))
			if err != nil {
				t.Fatal(err)
			}
			body := `{"title":"` + strings.Repeat("a", 100<<10) + `"}`
			go func() {
				fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: l1\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", path, admin, len(body), body)
			}()
			conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			rd := bufio.NewReader(conn)
			head, _ := rd.ReadString('\n')
			var lines []string
			for {
				l, err := rd.ReadString('\n')
				if err != nil || strings.TrimSpace(l) == "" {
					break
				}
				lines = append(lines, strings.TrimSpace(l))
			}
			conn.Close()
			if !strings.Contains(head, " 413 ") || !slices.Contains(lines, "Connection: close") {
				t.Errorf("%s: %q %v", path, strings.TrimSpace(head), lines)
			}
		}
	})
}

// TestL1List verifies GET /api/admin/activity-log: newest first, 50 per page with before,
// has_more and next_before, results computed from the status, nulls for unused fields, filters
// ignored, invalid cursors 400, a constant number of queries, 405 for other methods, and the
// admin authentication.
func TestL1List(t *testing.T) {
	app, db, token, counter := j1InProcess(t, "l1list")
	k1Exec(t, db, `INSERT INTO admin_activity_log (actor, action, method, path, status, ip, target_type, target_id, detail)
		SELECT 'admin', 'device.create', 'POST', '/api/admin/devices', (ARRAY[200, 302, 404, 500])[1 + g % 4], CASE WHEN g % 2 = 0 THEN '192.0.2.1' END,
		       CASE WHEN g % 3 = 0 THEN 'device' END, CASE WHEN g % 3 = 0 THEN 'SN-' || g END, NULL
		FROM generate_series(1, 120) g`)
	list := app.AdminMiddleware(app.ActivityLog(app.AdminActivityLogHandler))
	type page struct {
		Data       []map[string]any `json:"data"`
		HasMore    bool             `json:"has_more"`
		NextBefore *int64           `json:"next_before"`
	}
	get := func(t *testing.T, query string) page {
		t.Helper()
		rec := serve(t, list, "GET", "/api/admin/activity-log"+query, token, "")
		var p page
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || rec.Code != 200 {
			t.Fatalf("%s: %d %s", query, rec.Code, rec.Body.String())
		}
		return p
	}
	var ids []int64
	sizes := []int{}
	for q := ""; ; {
		p := get(t, q)
		sizes = append(sizes, len(p.Data))
		for _, it := range p.Data {
			id := int64(it["id"].(float64))
			ids = append(ids, id)
			status := int(it["status"].(float64))
			if it["result"] != handlers.ActivityResult(status) {
				t.Errorf("item %d: status %d result %v", id, status, it["result"])
			}
			created, _ := it["created_at"].(string)
			if _, err := time.Parse(time.RFC3339, created); err != nil || !strings.HasSuffix(created, "+03:00") {
				t.Errorf("created_at %q", created)
			}
			for _, k := range []string{"ip", "target_type", "target_id", "detail"} {
				if _, ok := it[k]; !ok {
					t.Errorf("item %d has no %s key", id, k)
				}
			}
			if it["detail"] != nil {
				t.Errorf("item %d detail %v, want null", id, it["detail"])
			}
		}
		if !p.HasMore {
			if p.NextBefore != nil {
				t.Errorf("last page has next_before")
			}
			break
		}
		if p.NextBefore == nil || *p.NextBefore != ids[len(ids)-1] {
			t.Fatalf("next_before %v, want %d", p.NextBefore, ids[len(ids)-1])
		}
		q = fmt.Sprintf("?before=%d", *p.NextBefore)
	}
	if fmt.Sprint(sizes) != "[50 50 20]" || len(ids) != 120 {
		t.Errorf("pages %v, %d items", sizes, len(ids))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] >= ids[i-1] {
			t.Fatalf("not newest first at %d", i)
		}
	}
	if a, b := get(t, "?action=login.failed&status=500"), get(t, ""); len(a.Data) != len(b.Data) || a.Data[0]["id"] != b.Data[0]["id"] {
		t.Errorf("filters must be ignored")
	}
	for _, bad := range []string{"abc", "0", "-1", "1.5", "99999999999999999999"} {
		rec := serve(t, list, "GET", "/api/admin/activity-log?before="+url.QueryEscape(bad), token, "")
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "before must be a positive activity log id") {
			t.Errorf("before=%s: %d %s", bad, rec.Code, rec.Body.String())
		}
	}
	for _, size := range []int{1, 10, 50} {
		k1Exec(t, db, `DELETE FROM admin_activity_log`)
		k1Exec(t, db, fmt.Sprintf(`INSERT INTO admin_activity_log (actor, action, method, path, status) SELECT 'admin', 'x.create', 'POST', '/x', 200 FROM generate_series(1, %d)`, size))
		before := counter.n.Load()
		if p := get(t, ""); len(p.Data) != size {
			t.Fatalf("page of %d", len(p.Data))
		}
		if used := counter.n.Load() - before; used != 2 {
			t.Errorf("a page of %d used %d statements, want 2 (session check, page)", size, used)
		}
	}
	if n := k1Count(t, db, `SELECT COUNT(*) FROM admin_activity_log`); n != 50 {
		t.Errorf("reading the log wrote rows: %d", n)
	}

	t.Run("methods and authentication on the real server", func(t *testing.T) {
		srv, sdb, admin := l1Server(t, "l1listauth", true)
		for _, m := range []string{"POST", "PUT", "DELETE"} {
			r := a14Do(t, srv, m, "/api/admin/activity-log", l1Header(admin), nil)
			if r.status != 405 || r.header.Get("Allow") != "GET, HEAD" || r.json(t)["message"] != "Method not allowed" {
				t.Errorf("%s: %d Allow=%q %s", m, r.status, r.header.Get("Allow"), r.body)
			}
		}
		later, earlier := time.Now().Add(time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
		for _, tc := range []struct {
			name   string
			header map[string]string
			status int
		}{
			{"no token", nil, 401},
			{"malformed token", a14Bearer("not-a-jwt"), 401},
			{"expired token", a14Bearer(e1Sign(t, jwt.MapClaims{"username": "admin", "role": "admin", "sv": 0, "exp": earlier})), 401},
			{"parent token", a14Bearer(e1Sign(t, jwt.MapClaims{"parent_id": 1, "phone": "+9647000000001", "role": "parent", "sv": 0, "exp": later})), 403},
			{"admin token", a14Bearer(admin), 200},
		} {
			if r := a14Do(t, srv, "GET", "/api/admin/activity-log", tc.header, nil); r.status != tc.status {
				t.Errorf("%s: %d %s", tc.name, r.status, r.body)
			}
		}
		script, _ := os.ReadFile(filepath.Join("..", "..", "db", "scripts", "rotate_admin_password.sql"))
		k1Exec(t, sdb, strings.ReplaceAll(string(script), "NEW_PASSWORD_HERE", "l1-rotated-password"))
		if r := a14Do(t, srv, "GET", "/api/admin/activity-log", a14Bearer(admin), nil); r.status != 401 {
			t.Errorf("token from before the rotation: %d", r.status)
		}
	})
}

// l1RouteProblems lists the admin routes in a cmd/api/main.go source that skip the activity
// wrapper: every /api/admin/ route must be AdminMiddleware(ActivityLog(handler)), or, for the
// login, ActivityLog(handler).
func l1RouteProblems(t *testing.T, src string) []string {
	t.Helper()
	fset := gotoken.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	routes := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok {
			return true
		}
		pattern, _ := strconv.Unquote(lit.Value)
		if !strings.Contains(pattern, "/api/admin/") {
			return true
		}
		routes++
		var chain []string
		ast.Inspect(call.Args[1], func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok {
				chain = append(chain, s.Sel.Name)
			}
			return true
		})
		want := "AdminMiddleware ActivityLog"
		if pattern == "POST /api/admin/login" {
			want = "ActivityLog"
		}
		if len(chain) < 2 || strings.Join(chain[:len(chain)-1], " ") != want {
			problems = append(problems, fmt.Sprintf("%s is registered as %v, want %s(handler)", pattern, chain, want))
		}
		return true
	})
	if routes == 0 {
		problems = append(problems, "no admin routes found")
	}
	return problems
}

// TestL1EveryAdminRouteIsLogged fails when an admin route is registered without the activity
// wrapper, and proves it catches an unwrapped route.
func TestL1EveryAdminRouteIsLogged(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "api", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range l1RouteProblems(t, string(src)) {
		t.Error(p)
	}
	for name, line := range map[string]string{
		"an admin route without the wrapper":       `mux.HandleFunc("POST /api/admin/fake", appEnv.AdminMiddleware(appEnv.FakeHandler))`,
		"the wrapper outside the admin middleware": `mux.HandleFunc("POST /api/admin/fake", appEnv.ActivityLog(appEnv.AdminMiddleware(appEnv.FakeHandler)))`,
		"an unwrapped login":                       `mux.HandleFunc("POST /api/admin/login", appEnv.AdminLoginHandler)`,
	} {
		fake := strings.Replace(string(src), "\tmux.HandleFunc(\"GET /health\"", "\t"+line+"\n\tmux.HandleFunc(\"GET /health\"", 1)
		if fake == string(src) {
			t.Fatal("could not insert the fake route")
		}
		if len(l1RouteProblems(t, fake)) == 0 {
			t.Errorf("%s passed the check", name)
		}
	}
}

// TestL1BeforePhoneNormalisation runs the new code on a database at 000028 without 000029, the
// state Staging is in between the steps of the README deploy notes.
func TestL1BeforePhoneNormalisation(t *testing.T) {
	db, dsn := setupThrowawayDB(t, "l1v28")
	m1Restore29(t, db)
	if _, err := db.Exec(a14Migration(t, "000029_normalize_phone_numbers.down.sql")); err != nil {
		t.Fatal(err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(a14AdminPassword), bcrypt.DefaultCost)
	k1Exec(t, db, fmt.Sprintf(`UPDATE admins SET password_hash = '%s' WHERE username = 'admin'`, hash))
	srv := g3Start(t, dsn, g3FreePort(t), true, "TRUSTED_PROXY_CIDRS=127.0.0.1/32", "ABSENCE_CRON_SCHEDULE=0 0 1 1 *", "FAKE_TODAY="+k1Today)
	admin := a14AdminToken(t, srv)
	l1Marker(t, srv, db, admin, "l1_on_28")
	if r := a14Do(t, srv, "GET", "/api/admin/activity-log", a14Bearer(admin), nil); r.status != 200 || !strings.Contains(string(r.body), `"target_id":"l1_on_28"`) {
		t.Errorf("log on 000028: %d %s", r.status, r.body)
	}
}

// TestL1ActivityLogMigration verifies migration 000028: it adds only admin_activity_log (with
// its checks, no extra index, no triggers) to a database at 000027, rolls back to the exact
// earlier schema and applies again; golang-migrate reaches it from versions 20 to 27; and phone
// normalisation (now 000029) still applies last, collision abort included.
func TestL1ActivityLogMigration(t *testing.T) {
	migrate, err := exec.LookPath("migrate")
	if err != nil {
		t.Skip("golang-migrate CLI not installed")
	}
	fresh := func(t *testing.T, label string) (*sql.DB, func(args ...string) (string, error)) {
		t.Helper()
		cli, cliDSN := setupThrowawayDB(t, label)
		for _, tbl := range []string{"admin_activity_log", "school_closures", "broadcasts", "announcements", "banner_images", "device_tokens", "settings", "notifications", "weekly_schedules", "student_leaves", "banners", "admins", "attendance_logs", "devices", "students", "parents"} {
			if _, err := cli.Exec("DROP TABLE IF EXISTS " + tbl + " CASCADE"); err != nil {
				t.Fatal(err)
			}
		}
		cli.Exec(`DROP FUNCTION IF EXISTS get_student_status(INT, DATE)`)
		cli.Exec(`DROP FUNCTION IF EXISTS notifications_fill_parent_id()`)
		u, _ := url.Parse(cliDSN)
		q := u.Query()
		q.Set("sslmode", "disable")
		u.RawQuery = q.Encode()
		return cli, func(args ...string) (string, error) {
			out, err := exec.Command(migrate, append([]string{"-path", testdb.MigrationsDir(), "-database", u.String()}, args...)...).CombinedOutput()
			return strings.TrimSpace(string(out)), err
		}
	}
	state := func(db *sql.DB) (log, phone bool) {
		db.QueryRow(`SELECT to_regclass('admin_activity_log') IS NOT NULL,
			EXISTS (SELECT 1 FROM information_schema.columns WHERE column_name = 'phone_number_original')`).Scan(&log, &phone)
		return
	}

	t.Run("000028 adds admin_activity_log alone, rolls back exactly and applies again", func(t *testing.T) {
		db, run := fresh(t, "l1mig")
		if out, err := run("goto", "27"); err != nil {
			t.Fatalf("goto 27: %v\n%s", err, out)
		}
		before := g3Schema(t, db)
		if out, err := run("goto", "28"); err != nil {
			t.Fatalf("goto 28: %v\n%s", err, out)
		}
		if log, phone := state(db); !log || phone {
			t.Fatalf("at 28: log %v, phone columns %v", log, phone)
		}
		k1Exec(t, db, `INSERT INTO admin_activity_log (actor, action, method, path, status) VALUES ('admin', 'x.create', 'POST', '/x', 200)`)
		for _, bad := range []string{
			`INSERT INTO admin_activity_log (actor, action, method, path, status) VALUES (repeat('a', 51), 'x', 'POST', '/x', 200)`,
			`INSERT INTO admin_activity_log (actor, action, method, path, status) VALUES ('', 'x', 'POST', '/x', 200)`,
			`INSERT INTO admin_activity_log (actor, action, method, path, status) VALUES ('a', repeat('x', 61), 'POST', '/x', 200)`,
			`INSERT INTO admin_activity_log (actor, action, method, path, status) VALUES ('a', 'x', 'POST', repeat('p', 201), 200)`,
			`INSERT INTO admin_activity_log (actor, action, method, path, status, ip) VALUES ('a', 'x', 'POST', '/x', 200, repeat('1', 65))`,
			`INSERT INTO admin_activity_log (actor, action, method, path, status, target_type) VALUES ('a', 'x', 'POST', '/x', 200, repeat('t', 41))`,
			`INSERT INTO admin_activity_log (actor, action, method, path, status, target_id) VALUES ('a', 'x', 'POST', '/x', 200, repeat('ع', 65))`,
			`INSERT INTO admin_activity_log (actor, action, method, path, status, detail) VALUES ('a', 'x', 'POST', '/x', 200, repeat('ع', 201))`,
			`INSERT INTO admin_activity_log (actor, action, method, path) VALUES ('a', 'x', 'POST', '/x')`,
		} {
			if _, err := db.Exec(bad); err == nil {
				t.Errorf("accepted: %s", bad)
			}
		}
		if n := k1Count(t, db, `SELECT COUNT(*) FROM pg_indexes WHERE tablename = 'admin_activity_log'`); n != 1 {
			t.Errorf("%d indexes, want only the primary key", n)
		}
		if out, err := run("goto", "27"); err != nil {
			t.Fatalf("down to 27: %v\n%s", err, out)
		}
		if after := g3Schema(t, db); after != before {
			t.Errorf("the schema after the rollback differs from 000027's")
		}
		if out, err := run("goto", "28"); err != nil {
			t.Fatalf("up again: %v\n%s", err, out)
		}
		if v, _ := run("version"); v != "28" {
			t.Errorf("version %q, want 28", v)
		}
	})

	for _, from := range []string{"20", "21", "22", "23", "24", "25", "26", "27"} {
		t.Run("golang-migrate from version "+from+" through 28 to 29", func(t *testing.T) {
			db, run := fresh(t, "l1v"+from)
			if out, err := run("goto", from); err != nil {
				t.Fatalf("goto %s: %v\n%s", from, err, out)
			}
			if out, err := run("goto", "28"); err != nil {
				t.Fatalf("goto 28: %v\n%s", err, out)
			}
			if log, phone := state(db); !log || phone {
				t.Errorf("at 28: log %v, phone columns %v", log, phone)
			}
			if out, err := run("goto", "29"); err != nil {
				t.Fatalf("goto 29: %v\n%s", err, out)
			}
			if v, _ := run("version"); v != "29" {
				t.Errorf("version %q, want 29", v)
			}
			if log, phone := state(db); !log || !phone {
				t.Errorf("at 29: log %v, phone columns %v", log, phone)
			}
		})
	}

	t.Run("000029 still aborts on collisions after 000028", func(t *testing.T) {
		db, run := fresh(t, "l1coll")
		if out, err := run("goto", "28"); err != nil {
			t.Fatalf("goto 28: %v\n%s", err, out)
		}
		k1Exec(t, db, `INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (993, 'A', '07000000993', 'x'), (994, 'B', '+9647000000993', 'x')`)
		out, err := run("up")
		if err == nil || !strings.Contains(out, "[993, 994]") {
			t.Fatalf("000029 should abort listing [993, 994]: %v\n%s", err, out)
		}
		if log, phone := state(db); !log || phone {
			t.Errorf("after the aborted 000029: log %v, phone columns %v; want true, false", log, phone)
		}
	})
}
