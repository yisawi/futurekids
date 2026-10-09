package warnings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TestG1DeleteLeave verifies Task G1: DELETE /api/admin/leaves?student_id=&date= removes that
// student's leave on that date and answers 200 whether or not one existed; the student is Absent
// again for the daily report and the noon absence job; other leaves are untouched; invalid
// parameters are 400 and every rejected token changes nothing.
func TestG1DeleteLeave(t *testing.T) {
	srv, db := a14Server(t, "g1")
	admin := a14AdminToken(t, srv)
	bearer := a14Bearer(admin)
	const day, otherDay, noLeaveDay = "2026-09-21", "2026-09-22", "2026-09-23"
	student := func(name string) int {
		t.Helper()
		r := a14Do(t, srv, "POST", "/api/admin/students", bearer, map[string]any{"name": name, "parent_name": "Omar Example", "parent_phone": "+9647000001301", "parent_pin": "604158"})
		if r.status != 200 {
			t.Fatalf("create %s: %d %s", name, r.status, r.body)
		}
		return int(r.json(t)["data"].(map[string]any)["id"].(float64))
	}
	sara, ali := student("Sara Example"), student("Ali Example")
	if _, err := db.Exec(`UPDATE students SET created_at = '2026-09-01 08:00' WHERE id IN ($1, $2)`, sara, ali); err != nil {
		t.Fatal(err)
	}
	for _, l := range []struct {
		id   int
		date string
	}{{sara, day}, {sara, otherDay}, {ali, day}} {
		if r := a14Do(t, srv, "POST", "/api/admin/leaves", bearer, map[string]any{"student_id": l.id, "leave_date": l.date, "notes": "Family visit"}); r.status != 200 {
			t.Fatalf("leave %d %s: %d %s", l.id, l.date, r.status, r.body)
		}
	}
	leaves := func() string {
		t.Helper()
		rows, err := db.Query(`SELECT id, student_id, leave_date::text, COALESCE(notes, '') FROM student_leaves ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var id, sid int
			var date, notes string
			rows.Scan(&id, &sid, &date, &notes)
			fmt.Fprintf(&b, "%d|%d|%s|%s\n", id, sid, date, notes)
		}
		return b.String()
	}
	has := func(id int, date string) bool {
		t.Helper()
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM student_leaves WHERE student_id = $1 AND leave_date = $2`, id, date).Scan(&n)
		return n == 1
	}
	report := func(date string) map[int]string {
		t.Helper()
		r := a14Do(t, srv, "GET", "/api/admin/attendance?date="+date, bearer, nil)
		if r.status != 200 {
			t.Fatalf("daily report: %d %s", r.status, r.body)
		}
		var resp struct {
			Data []struct {
				StudentID int    `json:"student_id"`
				Status    string `json:"status"`
			} `json:"data"`
		}
		json.Unmarshal(r.body, &resp)
		out := map[int]string{}
		for _, e := range resp.Data {
			out[e.StudentID] = e.Status
		}
		return out
	}
	cancel := func(query string, header map[string]string) a14Response {
		t.Helper()
		return a14Do(t, srv, "DELETE", "/api/admin/leaves"+query, header, nil)
	}
	const done = "No leave remains for this student on this date"
	ok := func(t *testing.T, r a14Response) {
		t.Helper()
		if m := r.json(t); r.status != 200 || m["status"] != "success" || m["message"] != done {
			t.Errorf("got %d %s, want 200 {status: success, message: %q}", r.status, r.body, done)
		}
	}

	t.Run("deleting a leave makes the student Absent again", func(t *testing.T) {
		if got := report(day); got[sara] != "Excused" || got[ali] != "Excused" {
			t.Fatalf("before: %v, want both Excused", got)
		}
		ok(t, cancel(fmt.Sprintf("?student_id=%d&date=%s", sara, day), bearer))
		if has(sara, day) {
			t.Errorf("the leave is still stored")
		}
		if !has(sara, otherDay) || !has(ali, day) {
			t.Errorf("other leaves were removed: same student other day %v, other student same day %v", has(sara, otherDay), has(ali, day))
		}
		if got := report(day); got[sara] != "Absent" || got[ali] != "Excused" {
			t.Errorf("daily report after cancelling: %v, want Sara Absent and Ali Excused", got)
		}
		if got := report(otherDay); got[sara] != "Excused" {
			t.Errorf("Sara on %s: %s, want Excused", otherDay, got[sara])
		}
		var status string
		db.QueryRow(`SELECT status FROM get_student_status($1, $2::date)`, sara, day).Scan(&status)
		if status != "Absent" {
			t.Errorf("get_student_status (what the noon absence job filters on): %q, want Absent", status)
		}
	})

	t.Run("deleting a leave that does not exist is also 200 and changes nothing", func(t *testing.T) {
		before := leaves()
		for _, q := range []string{
			fmt.Sprintf("?student_id=%d&date=%s", sara, day),
			fmt.Sprintf("?student_id=%d&date=%s", sara, noLeaveDay),
			fmt.Sprintf("?student_id=%d&date=%s", ali, otherDay),
			"?student_id=999999&date=" + day,
		} {
			ok(t, cancel(q, bearer))
		}
		if after := leaves(); after != before {
			t.Errorf("leaves changed\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	t.Run("invalid parameters are 400", func(t *testing.T) {
		before := leaves()
		for _, tc := range []struct{ query, msg string }{
			{"", "student_id and date are required"},
			{"?date=" + otherDay, "student_id and date are required"},
			{fmt.Sprintf("?student_id=%d", sara), "student_id and date are required"},
			{"?student_id=&date=" + otherDay, "student_id and date are required"},
			{"?student_id=0&date=" + otherDay, "student_id must be a positive student id"},
			{"?student_id=-1&date=" + otherDay, "student_id must be a positive student id"},
			{"?student_id=abc&date=" + otherDay, "student_id must be a positive student id"},
			{"?student_id=1.5&date=" + otherDay, "student_id must be a positive student id"},
			{fmt.Sprintf("?student_id=%d&date=22-09-2026", sara), "date must be formatted as YYYY-MM-DD"},
			{fmt.Sprintf("?student_id=%d&date=2026-02-30", sara), "date must be formatted as YYYY-MM-DD"},
			{fmt.Sprintf("?student_id=%d&date=2026-9-22", sara), "date must be formatted as YYYY-MM-DD"},
			{fmt.Sprintf("?student_id=%d&date=1999-12-31", sara), "date must have a year from 2000 to 2100"},
			{fmt.Sprintf("?student_id=%d&date=2101-01-01", sara), "date must have a year from 2000 to 2100"},
		} {
			r := cancel(tc.query, bearer)
			if m := r.json(t); r.status != 400 || m["status"] != "error" || m["message"] != tc.msg {
				t.Errorf("%q: %d %s, want 400 %q", tc.query, r.status, r.body, tc.msg)
			}
		}
		if after := leaves(); after != before {
			t.Errorf("rejected requests changed leaves")
		}
	})

	t.Run("authentication", func(t *testing.T) {
		before := leaves()
		query := fmt.Sprintf("?student_id=%d&date=%s", sara, otherDay)
		later, earlier := time.Now().Add(time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
		for _, tc := range []struct {
			name   string
			header map[string]string
			status int
			msg    string
		}{
			{"no token", nil, 401, "Unauthorized"},
			{"malformed token", a14Bearer("not-a-jwt"), 401, "Unauthorized"},
			{"wrong scheme", map[string]string{"Authorization": "Basic " + admin}, 401, "Unauthorized"},
			{"expired token", a14Bearer(e1Sign(t, jwt.MapClaims{"username": "admin", "role": "admin", "sv": 0, "exp": earlier})), 401, "Unauthorized"},
			{"parent token", a14Bearer(e1Sign(t, jwt.MapClaims{"parent_id": 1, "phone": "+9647000001301", "role": "parent", "sv": 0, "exp": later})), 403, "Forbidden"},
		} {
			r := cancel(query, tc.header)
			if r.status != tc.status || r.json(t)["message"] != tc.msg {
				t.Errorf("%s: %d %s, want %d %s", tc.name, r.status, r.body, tc.status, tc.msg)
			}
		}
		script, err := os.ReadFile(filepath.Join("..", "..", "db", "scripts", "rotate_admin_password.sql"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(strings.ReplaceAll(string(script), "NEW_PASSWORD_HERE", "g1-rotated-password")); err != nil {
			t.Fatalf("rotation script: %v", err)
		}
		if r := cancel(query, bearer); r.status != 401 {
			t.Errorf("token from before the rotation: %d %s, want 401", r.status, r.body)
		}
		if after := leaves(); after != before {
			t.Errorf("rejected tokens changed leaves")
		}
	})

	t.Run("each call is logged with student_id, date and whether a leave was removed", func(t *testing.T) {
		var removed []string
		for _, raw := range strings.Split(srv.out.String(), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(raw), &m) != nil || m["msg"] != "Leave cancelled" {
				continue
			}
			var keys []string
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if strings.Join(keys, ",") != "date,level,msg,removed,student_id,time" || m["level"] != "INFO" {
				t.Errorf("log line %v: want only time, level INFO, msg, student_id, date, removed", m)
			}
			removed = append(removed, fmt.Sprint(m["removed"]))
		}
		if strings.Join(removed, ",") != "true,false,false,false,false" {
			t.Errorf("removed values %v, want true then four false", removed)
		}
	})
}
