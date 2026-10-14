package warnings

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"future_kids/internal/auth"
	"future_kids/internal/background"
	"future_kids/internal/cron"
	"future_kids/internal/handlers"
	"future_kids/internal/ratelimit"

	"github.com/xuri/excelize/v2"
	"golang.org/x/crypto/bcrypt"
)

const (
	p2Pin        = "Pz7#kQ2m!Lx9wR4t"
	p2DeletePath = "/api/mobile/account/delete"
)

// p2Student creates a student and its parent through the admin API and returns the student id.
func p2Student(t *testing.T, srv *g3Server, admin, name, parentName, phone, pin, tag string) int {
	t.Helper()
	body := map[string]any{"name": name, "parent_name": parentName, "parent_phone": phone, "rfid_tag": tag}
	if pin != "" {
		body["parent_pin"] = pin
	}
	r := a14Do(t, srv, "POST", "/api/admin/students", a14Bearer(admin), body)
	if r.status != 200 {
		t.Fatalf("create %s: %d %s", name, r.status, r.body)
	}
	var id int
	fmt.Sscan(fmt.Sprint(r.json(t)["data"].(map[string]any)["id"]), &id)
	return id
}

// p2Login logs a parent in and returns the token, failing the test unless it gets 200.
func p2Login(t *testing.T, srv *g3Server, phone, pin string, extra ...string) string {
	t.Helper()
	body := map[string]string{"phone": phone, "pin": pin}
	for i := 0; i+1 < len(extra); i += 2 {
		body[extra[i]] = extra[i+1]
	}
	r := a14Do(t, srv, "POST", "/api/mobile/login", nil, body)
	if r.status != 200 {
		t.Fatalf("login %s: %d %s", phone, r.status, r.body)
	}
	return fmt.Sprint(r.json(t)["data"].(map[string]any)["token"])
}

func p2Delete(t *testing.T, srv *g3Server, token string, body any) a14Response {
	t.Helper()
	var h map[string]string
	if token != "" {
		h = a14Bearer(token)
	}
	return a14Do(t, srv, "POST", p2DeletePath, h, body)
}

func p2Expect(t *testing.T, what string, r a14Response, status int, body string) {
	t.Helper()
	if r.status != status || (body != "" && string(r.body) != body+"\n") {
		t.Errorf("%s: %d %s, want %d %s", what, r.status, r.body, status, body)
	}
}

func p2Error(message string) string { return `{"message":"` + message + `","status":"error"}` }

// p2MobileRoutes returns every "METHOD path" of cmd/api/main.go behind AuthMiddleware.
func p2MobileRoutes(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "api", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	var routes []string
	for _, m := range regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+ /api/mobile/[^"]+)", appEnv\.AuthMiddleware\(`).FindAllStringSubmatch(string(src), -1) {
		routes = append(routes, m[1])
	}
	if len(routes) < 15 {
		t.Fatalf("found only %d parent routes in main.go: %v", len(routes), routes)
	}
	return routes
}

// p2ActivityRows returns the parent.account_delete rows as "actor|method|path|status|ip|type|id|detail".
func p2ActivityRows(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT concat_ws('|', actor, method, path, status, COALESCE(ip, '-'), COALESCE(target_type, '-'), COALESCE(target_id, '-'), COALESCE(detail, '-'))
		FROM admin_activity_log WHERE action = $1 ORDER BY id`, handlers.ActionParentAccountDelete)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// TestP2ParentForeignKeys pins every foreign key that references parents(id) and its delete
// action, so a new one cannot silently block or break account deletion.
func TestP2ParentForeignKeys(t *testing.T) {
	db, _ := setupThrowawayDB(t, "p2fk")
	rows, err := db.Query(`
		SELECT c.conrelid::regclass::text || '.' || a.attname || ' ' || c.confdeltype::text
		FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
		WHERE c.contype = 'f' AND c.confrelid = 'parents'::regclass
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		got = append(got, s)
	}
	// r = RESTRICT (handled by unlinking first), c = CASCADE, n = SET NULL.
	want := []string{"broadcasts.audience_parent_id n", "device_tokens.parent_id c", "notifications.parent_id c", "students.parent_id r"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("foreign keys to parents: %v, want %v", got, want)
	}
	var nullable string
	db.QueryRow(`SELECT is_nullable FROM information_schema.columns WHERE table_name = 'students' AND column_name = 'parent_id'`).Scan(&nullable)
	if nullable != "YES" {
		t.Errorf("students.parent_id is_nullable %q, want YES", nullable)
	}
}

// TestP2DeleteAccount verifies Part A through the real API: validation, a wrong PIN that changes
// nothing and keeps the session, the deletion itself (children kept without a parent, tokens and
// notifications gone, attendance intact), every admin view of a parent-less child, the old token
// refused on every parent route, the activity log naming only the children, logs without the
// parent's data, and re-linking by the admin.
func TestP2DeleteAccount(t *testing.T) {
	srv, db, admin := l1Server(t, "p2delete", true)
	const phone, otherPhone, name = "+9647000006101", "+9647000006102", "P2 Parent Alpha"
	s1 := p2Student(t, srv, admin, "P2 Kid One", name, phone, p2Pin, "6101")
	s2 := p2Student(t, srv, admin, "P2 Kid Two", name, phone, "", "6102")
	s3 := p2Student(t, srv, admin, "P2 Other Kid", "P2 Parent Beta", otherPhone, p2Pin, "6103")
	if r := a14Do(t, srv, "DELETE", fmt.Sprintf("/api/admin/students?id=%d", s2), a14Bearer(admin), nil); r.status != 200 {
		t.Fatalf("deactivate: %d %s", r.status, r.body)
	}
	var parentID, otherID int
	db.QueryRow(`SELECT id FROM parents WHERE phone_number = $1`, phone).Scan(&parentID)
	db.QueryRow(`SELECT id FROM parents WHERE phone_number = $1`, otherPhone).Scan(&otherID)
	k1Exec(t, db,
		`UPDATE students SET created_at = '2026-01-01 08:00'`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('P2-DEV', 'Gate', true)`,
		fmt.Sprintf(`INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (%d, 'P2-DEV', '2026-09-23 07:40'), (%d, 'P2-DEV', '2026-09-24 07:41'), (%d, 'P2-DEV', '2026-09-22 07:42')`, s1, s1, s2),
		fmt.Sprintf(`INSERT INTO student_leaves (student_id, leave_date) VALUES (%d, '2026-09-21')`, s1),
		fmt.Sprintf(`INSERT INTO notifications (parent_id, title, body) VALUES (%d, 'T1', 'B1'), (%d, 'T2', 'B2'), (%d, 'T3', 'B3')`, parentID, parentID, otherID),
		fmt.Sprintf(`INSERT INTO broadcasts (title, body, audience_type, audience_parent_id, recipient_count) VALUES ('P2 B', 'P2 body', 'parent', %d, 1)`, parentID))
	token := p2Login(t, srv, phone, p2Pin, "fcm_token", b1Token("p2-one"))
	if r := a14Do(t, srv, "PUT", "/api/mobile/device-token", a14Bearer(token), map[string]string{"token": b1Token("p2-two")}); r.status != 200 {
		t.Fatalf("register: %d %s", r.status, r.body)
	}
	otherToken := p2Login(t, srv, otherPhone, p2Pin, "fcm_token", b1Token("p2-other"))
	count := func(q string, args ...any) int { return k1Count(t, db, q, args...) }
	attendance, leaves := count(`SELECT COUNT(*) FROM attendance_logs`), count(`SELECT COUNT(*) FROM student_leaves`)

	t.Run("invalid requests change nothing", func(t *testing.T) {
		p2Expect(t, "no pin", p2Delete(t, srv, token, map[string]any{}), 400, p2Error(handlers.AccountPINRequiredMessage))
		p2Expect(t, "empty pin", p2Delete(t, srv, token, map[string]any{"pin": ""}), 400, p2Error(handlers.AccountPINRequiredMessage))
		p2Expect(t, "null pin", p2Delete(t, srv, token, map[string]any{"pin": nil}), 400, p2Error(handlers.AccountPINRequiredMessage))
		p2Expect(t, "number pin", p2Delete(t, srv, token, map[string]any{"pin": 1234}), 400, p2Error("Invalid request body"))
		p2Expect(t, "malformed JSON", p2Delete(t, srv, token, "{"), 400, p2Error("Invalid request body"))
		p2Expect(t, "oversized", p2Delete(t, srv, token, `{"pin":"`+strings.Repeat("a", 70<<10)+`"}`), 413, p2Error("Request body too large"))
		p2Expect(t, "no token", p2Delete(t, srv, "", map[string]string{"pin": p2Pin}), 401, p2Error("Unauthorized"))
		p2Expect(t, "garbage token", p2Delete(t, srv, "not-a-jwt", map[string]string{"pin": p2Pin}), 401, p2Error("Invalid or expired token"))
		p2Expect(t, "admin token", p2Delete(t, srv, admin, map[string]string{"pin": p2Pin}), 403, p2Error("Access denied"))
		p2Expect(t, "wrong method", a14Do(t, srv, "GET", p2DeletePath, a14Bearer(token), nil), 405, "")

		p2Expect(t, "wrong PIN", p2Delete(t, srv, token, map[string]string{"pin": "Wrong#Pin123456789"}), 400, p2Error(handlers.IncorrectPINMessage))
		p2Expect(t, "100-character PIN", p2Delete(t, srv, token, map[string]string{"pin": strings.Repeat("Zz9!", 25)}), 400, p2Error(handlers.IncorrectPINMessage))
		p2Expect(t, "PIN with a trailing space", p2Delete(t, srv, token, map[string]string{"pin": p2Pin + " "}), 400, p2Error(handlers.IncorrectPINMessage))
		if n := count(`SELECT COUNT(*) FROM parents WHERE id = $1`, parentID); n != 1 {
			t.Fatalf("parent deleted by a rejected request")
		}
		if n := count(`SELECT COUNT(*) FROM device_tokens WHERE parent_id = $1`, parentID); n != 2 {
			t.Errorf("device tokens %d after rejected requests, want 2", n)
		}
		if n := count(`SELECT COUNT(*) FROM students WHERE parent_id = $1`, parentID); n != 2 {
			t.Errorf("linked students %d after rejected requests, want 2", n)
		}
		if r := a14Do(t, srv, "GET", "/api/mobile/students", a14Bearer(token), nil); r.status != 200 {
			t.Errorf("session after a wrong PIN: %d %s, want it to stay valid", r.status, r.body)
		}
		if rows := p2ActivityRows(t, db); len(rows) != 0 {
			t.Errorf("rejected requests wrote activity rows: %v", rows)
		}
	})

	t.Run("deletion", func(t *testing.T) {
		p2Expect(t, "delete", p2Delete(t, srv, token, map[string]string{"pin": p2Pin}), 200, `{"message":"Account deleted","status":"success"}`)
		for q, want := range map[string]int{
			`SELECT COUNT(*) FROM parents WHERE id = $1`:                    0,
			`SELECT COUNT(*) FROM device_tokens WHERE parent_id = $1`:       0,
			`SELECT COUNT(*) FROM notifications WHERE parent_id = $1`:       0,
			`SELECT COUNT(*) FROM students WHERE parent_id = $1`:            0,
			`SELECT COUNT(*) FROM broadcasts WHERE audience_parent_id = $1`: 0,
		} {
			if got := count(q, parentID); got != want {
				t.Errorf("%s: %d, want %d", q, got, want)
			}
		}
		if got := count(`SELECT COUNT(*) FROM broadcasts WHERE audience_type = 'parent' AND audience_parent_id IS NULL`); got != 1 {
			t.Errorf("the parent's broadcast: %d with a NULL audience parent, want it kept (SET NULL)", got)
		}
		if got := count(`SELECT COUNT(*) FROM device_tokens WHERE parent_id = $1`, otherID); got != 1 {
			t.Errorf("another parent's device tokens: %d, want 1", got)
		}
		if got := count(`SELECT COUNT(*) FROM students WHERE id = ANY($1::int[]) AND parent_id IS NULL`, fmt.Sprintf("{%d,%d}", s1, s2)); got != 2 {
			t.Errorf("unlinked students %d, want both kept with no parent", got)
		}
		if got := count(`SELECT COUNT(*) FROM students WHERE id = $1 AND is_active = false`, s2); got != 1 {
			t.Errorf("the inactive child changed its state")
		}
		if got := count(`SELECT COUNT(*) FROM students WHERE id = $1 AND parent_id = $2`, s3, otherID); got != 1 {
			t.Errorf("another parent's child was unlinked")
		}
		if got := count(`SELECT COUNT(*) FROM notifications WHERE parent_id = $1`, otherID); got != 1 {
			t.Errorf("another parent's notifications: %d, want 1", got)
		}
		if a, l := count(`SELECT COUNT(*) FROM attendance_logs`), count(`SELECT COUNT(*) FROM student_leaves`); a != attendance || l != leaves {
			t.Errorf("attendance %d→%d, leaves %d→%d: history must stay", attendance, a, leaves, l)
		}
		if r := a14Do(t, srv, "GET", "/api/mobile/students", a14Bearer(otherToken), nil); r.status != 200 {
			t.Errorf("another parent's session: %d", r.status)
		}
	})

	t.Run("the old token gets 401 on every parent route", func(t *testing.T) {
		for _, route := range p2MobileRoutes(t) {
			method, path, _ := strings.Cut(route, " ")
			r := a14Do(t, srv, method, path, a14Bearer(token), map[string]any{"pin": p2Pin, "token": b1Token("p2-after"), "push_enabled": true})
			p2Expect(t, route, r, 401, p2Error("Invalid or expired token"))
		}
		if r := a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": phone, "pin": p2Pin}); r.status != 401 {
			t.Errorf("login after deletion: %d, want 401", r.status)
		}
	})

	t.Run("admin views of a parent-less child", func(t *testing.T) {
		r := a14Do(t, srv, "GET", "/api/admin/students", a14Bearer(admin), nil)
		var found bool
		for _, s := range r.json(t)["data"].([]any) {
			m := s.(map[string]any)
			if fmt.Sprint(m["id"]) == fmt.Sprint(s1) {
				found = m["parent_name"] == "" && m["parent_phone"] == ""
			}
		}
		if r.status != 200 || !found {
			t.Errorf("students list: %d, child %d without parent shown as empty strings: %v", r.status, s1, found)
		}
		r = a14Do(t, srv, "GET", "/api/admin/export/excel", a14Bearer(admin), nil)
		if r.status != 200 {
			t.Fatalf("excel: %d %s", r.status, r.body)
		}
		f, err := excelize.OpenReader(bytes.NewReader(r.body))
		if err != nil {
			t.Fatal(err)
		}
		rows, _ := f.GetRows(f.GetSheetName(0))
		var row []string
		for _, rw := range rows {
			if len(rw) > 5 && rw[1] == "P2 Kid One" {
				row = rw
			}
		}
		if row == nil || row[4] != "-" || row[5] != "-" {
			t.Errorf("excel row of the parent-less child: %v, want '-' as parent and phone", row)
		}
		r = a14Do(t, srv, "GET", "/api/admin/broadcasts", a14Bearer(admin), nil)
		item := r.json(t)["data"].([]any)[0].(map[string]any)["audience"].(map[string]any)
		if r.status != 200 || item["type"] != "parent" || item["parent_name"] != nil || item["parent_phone"] != nil {
			t.Errorf("broadcast log of the deleted parent: %d %v", r.status, item)
		}
		r = a14Do(t, srv, "GET", "/api/admin/dashboard", a14Bearer(admin), nil)
		if d := r.json(t)["data"].(map[string]any); r.status != 200 || fmt.Sprint(d["total_parents"]) != "1" {
			t.Errorf("dashboard: %d %s", r.status, r.body)
		}
		if r := a14Do(t, srv, "GET", "/api/admin/attendance", a14Bearer(admin), nil); r.status != 200 {
			t.Errorf("daily report: %d", r.status)
		}
	})

	t.Run("activity log names only the children", func(t *testing.T) {
		got := p2ActivityRows(t, db)
		want := []string{
			fmt.Sprintf("parent|POST|%s|200|-|student|%d|P2 Kid One", p2DeletePath, s1),
			fmt.Sprintf("parent|POST|%s|200|-|student|%d|P2 Kid Two", p2DeletePath, s2),
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("activity rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		r := a14Do(t, srv, "GET", "/api/admin/activity-log", a14Bearer(admin), nil)
		if r.status != 200 || !strings.Contains(string(r.body), `"action":"parent.account_delete"`) {
			t.Errorf("activity log endpoint: %d", r.status)
		}
		forbidden := []string{phone, strings.TrimPrefix(phone, "+"), "0" + phone[4:], name, p2Pin}
		all := count(`SELECT COUNT(*) FROM admin_activity_log`)
		for _, f := range forbidden {
			if n := count(`SELECT COUNT(*) FROM admin_activity_log WHERE concat_ws('|', actor, action, path, ip, target_type, target_id, detail) LIKE '%' || $1 || '%'`, f); n != 0 {
				t.Errorf("%d of %d activity rows contain the parent's %q", n, all, f)
			}
			if strings.Contains(srv.out.String(), f) {
				t.Errorf("server log contains the parent's %q", f)
			}
		}
		if !strings.Contains(srv.out.String(), "Parent account deleted") {
			t.Errorf("no INFO line for the deletion")
		}
	})

	t.Run("admin re-links the children", func(t *testing.T) {
		put := func(id int, name, parentName, phone, pin string) a14Response {
			body := map[string]any{"id": id, "name": name, "parent_name": parentName, "parent_phone": phone}
			if pin != "" {
				body["parent_pin"] = pin
			}
			return a14Do(t, srv, "PUT", "/api/admin/students", a14Bearer(admin), body)
		}
		p2Expect(t, "re-link to the deleted number without a PIN", put(s1, "P2 Kid One", name, phone, ""), 400, p2Error(handlers.PINRequiredMessage))
		if n := count(`SELECT COUNT(*) FROM students WHERE id = $1 AND parent_id IS NULL`, s1); n != 1 {
			t.Errorf("a refused re-link changed the child")
		}
		p2Expect(t, "link to an existing parent without a PIN", put(s2, "P2 Kid Two", "P2 Parent Beta", otherPhone, ""), 200, "")
		if n := count(`SELECT COUNT(*) FROM students WHERE id = $1 AND parent_id = $2`, s2, otherID); n != 1 {
			t.Errorf("child not linked to the existing parent")
		}
		const newPin = "Nw5$tY8u!Kp3rE6q"
		p2Expect(t, "re-link with a new PIN", put(s1, "P2 Kid One", name, phone, newPin), 200, "")
		var newID, version int
		var push bool
		db.QueryRow(`SELECT id, session_version, push_enabled FROM parents WHERE phone_number = $1`, phone).Scan(&newID, &version, &push)
		if newID == 0 || newID == parentID || version != 0 || !push {
			t.Errorf("re-link parent id %d (old %d), session_version %d, push_enabled %v: want a brand-new row with defaults", newID, parentID, version, push)
		}
		if n := count(`SELECT COUNT(*) FROM students WHERE id = $1 AND parent_id = $2`, s1, newID); n != 1 {
			t.Errorf("child not linked to the new parent")
		}
		s4 := p2Student(t, srv, admin, "P2 Kid Four", name, phone, "", "6104")
		if n := count(`SELECT COUNT(*) FROM students WHERE id = $1 AND parent_id = $2`, s4, newID); n != 1 {
			t.Errorf("a new child for the re-linked number did not join the new parent")
		}
		fresh := p2Login(t, srv, phone, newPin)
		r := a14Do(t, srv, "GET", "/api/mobile/students", a14Bearer(fresh), nil)
		if list := r.json(t)["data"].([]any); r.status != 200 || len(list) != 2 {
			t.Errorf("new parent sees %d children (%d), want 2", len(list), r.status)
		}
		if r := a14Do(t, srv, "GET", "/api/mobile/students", a14Bearer(token), nil); r.status != 401 {
			t.Errorf("the deleted account's token works again after re-link: %d", r.status)
		}
	})
}

// TestP2DeleteWithoutChildren deletes a parent with no linked student: one activity row without
// a target.
func TestP2DeleteWithoutChildren(t *testing.T) {
	srv, db, admin := l1Server(t, "p2nokids", true)
	const phone = "+9647000006201"
	s := p2Student(t, srv, admin, "P2 Moved Kid", "P2 Lonely", phone, p2Pin, "6201")
	p2Student(t, srv, admin, "P2 Moved Kid", "P2 Elsewhere", "+9647000006202", p2Pin, "6202")
	k1Exec(t, db, fmt.Sprintf(`UPDATE students SET parent_id = (SELECT id FROM parents WHERE phone_number = '+9647000006202') WHERE id = %d`, s))
	token := p2Login(t, srv, phone, p2Pin)
	p2Expect(t, "delete", p2Delete(t, srv, token, map[string]string{"pin": p2Pin}), 200, `{"message":"Account deleted","status":"success"}`)
	if got := p2ActivityRows(t, db); len(got) != 1 || got[0] != "parent|POST|"+p2DeletePath+"|200|-|-|-|"+handlers.NoLinkedStudentsDetail {
		t.Errorf("activity rows %v", got)
	}
	if n := k1Count(t, db, `SELECT COUNT(*) FROM parents WHERE phone_number = $1`, phone); n != 0 {
		t.Errorf("parent not deleted")
	}
}

// TestP2DeleteRateLimit verifies that wrong PINs on deletion count against the login limiter for
// the parent's phone, that the limit blocks both login and deletion, and that a PIN longer than
// bcrypt's 72 bytes is judged exactly as login judges it.
func TestP2DeleteRateLimit(t *testing.T) {
	srv, db, admin := l1Server(t, "p2limit", true)
	const phoneA, phoneB, phoneC = "+9647000006301", "+9647000006302", "+9647000006303"
	pin72 := strings.Repeat("Ab1#", 18)
	p2Student(t, srv, admin, "P2 Limit A", "P2 Limit A", phoneA, p2Pin, "6301")
	p2Student(t, srv, admin, "P2 Limit B", "P2 Limit B", phoneB, p2Pin, "6302")
	p2Student(t, srv, admin, "P2 Limit C", "P2 Limit C", phoneC, pin72, "6303")
	tokA, tokB, tokC := p2Login(t, srv, phoneA, p2Pin), p2Login(t, srv, phoneB, p2Pin), p2Login(t, srv, phoneC, pin72)
	wrong := map[string]string{"pin": "Wrong#Pin123456789"}

	for i := 1; i <= 5; i++ {
		p2Expect(t, fmt.Sprintf("wrong PIN %d", i), p2Delete(t, srv, tokA, wrong), 400, p2Error(handlers.IncorrectPINMessage))
	}
	r := p2Delete(t, srv, tokA, map[string]string{"pin": p2Pin})
	if r.status != 429 {
		t.Fatalf("correct PIN after 5 wrong ones: %d %s, want 429", r.status, r.body)
	}
	a14Retry(t, r, 900)
	if r := a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": "07000006301", "pin": p2Pin}); r.status != 429 {
		t.Errorf("login after 5 wrong deletion PINs: %d, want 429 (one limiter per phone)", r.status)
	}
	if n := k1Count(t, db, `SELECT COUNT(*) FROM parents WHERE phone_number = $1`, phoneA); n != 1 {
		t.Errorf("rate-limited request deleted the parent")
	}
	if r := a14Do(t, srv, "GET", "/api/mobile/students", a14Bearer(tokA), nil); r.status != 200 {
		t.Errorf("rate limiting signed the parent out: %d", r.status)
	}

	for i := 0; i < 3; i++ {
		if r := a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": phoneB, "pin": "nope"}); r.status != 401 {
			t.Fatalf("failed login: %d", r.status)
		}
	}
	p2Delete(t, srv, tokB, wrong)
	p2Delete(t, srv, tokB, map[string]string{"pin": strings.Repeat("x", 100)})
	if r := p2Delete(t, srv, tokB, map[string]string{"pin": p2Pin}); r.status != 429 {
		t.Errorf("3 failed logins + 2 failed deletions: %d, want 429", r.status)
	}

	long := pin72 + strings.Repeat("z", 28)
	if r := a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": phoneC, "pin": long}); r.status != 200 {
		t.Errorf("login with the 72-byte credential plus 28 bytes: %d (bcrypt reads 72 bytes)", r.status)
	}
	p2Expect(t, "deletion judges a 100-byte PIN as login does", p2Delete(t, srv, tokC, map[string]string{"pin": long}), 200, "")
	if rows := p2ActivityRows(t, db); len(rows) != 1 || !strings.HasSuffix(rows[0], "|P2 Limit C") {
		t.Errorf("activity rows %v: only the successful deletion is recorded", rows)
	}
	if out := srv.out.String(); strings.Contains(out, phoneA) || strings.Contains(out, phoneB) {
		// The login handler logs the phone when it rate-limits; deletion must not.
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "AccountDeleteHandler") && (strings.Contains(l, phoneA) || strings.Contains(l, phoneB)) {
				t.Errorf("deletion logged a phone: %s", l)
			}
		}
	}
}

// TestP2DeleteConcurrency verifies that two simultaneous deletions give one 200 and one 401, and
// that requests racing a deletion never get 500 and leave no orphan.
func TestP2DeleteConcurrency(t *testing.T) {
	srv, db, admin := l1Server(t, "p2race", true)
	for round := 0; round < 10; round++ {
		phone := fmt.Sprintf("+96470000064%02d", round)
		p2Student(t, srv, admin, "P2 Race", "P2 Race", phone, p2Pin, fmt.Sprintf("64%02d", round))
		var id int
		db.QueryRow(`SELECT id FROM parents WHERE phone_number = $1`, phone).Scan(&id)
		token := p2Login(t, srv, phone, p2Pin)
		calls := []struct {
			method, path string
			body         any
		}{
			{"POST", p2DeletePath, map[string]string{"pin": p2Pin}},
			{"POST", p2DeletePath, map[string]string{"pin": p2Pin}},
			{"PUT", "/api/mobile/device-token", map[string]string{"token": b1Token(fmt.Sprintf("race-%02d", round))}},
			{"PUT", "/api/mobile/notification-settings", map[string]bool{"push_enabled": round%2 == 0}},
			{"PUT", "/api/mobile/notifications/read-all", nil},
			{"GET", "/api/mobile/notification-settings", nil},
		}
		statuses := make([]int, len(calls))
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, c := range calls {
			wg.Add(1)
			go func(i int, method, path string, body any) {
				defer wg.Done()
				<-start
				statuses[i] = a14Do(t, srv, method, path, a14Bearer(token), body).status
			}(i, c.method, c.path, c.body)
		}
		close(start)
		wg.Wait()
		deletes := []int{statuses[0], statuses[1]}
		sort.Ints(deletes)
		if deletes[0] != 200 || deletes[1] != 401 {
			t.Errorf("round %d: double delete gave %v, want [200 401]", round, deletes)
		}
		for i, s := range statuses[2:] {
			if s != 200 && s != 401 {
				t.Errorf("round %d: %s %s racing the deletion got %d", round, calls[i+2].method, calls[i+2].path, s)
			}
		}
		for _, q := range []string{`SELECT COUNT(*) FROM parents WHERE id = $1`, `SELECT COUNT(*) FROM device_tokens WHERE parent_id = $1`,
			`SELECT COUNT(*) FROM notifications WHERE parent_id = $1`, `SELECT COUNT(*) FROM students WHERE parent_id = $1`} {
			if n := k1Count(t, db, q, id); n != 0 {
				t.Errorf("round %d: %s = %d after deletion", round, q, n)
			}
		}
	}
	if strings.Contains(srv.out.String(), `"level":"ERROR"`) {
		t.Errorf("server logged an ERROR during the races:\n%s", srv.out)
	}
}

// TestP2ParentlessStudent verifies that a child whose parent deleted the account still has its
// punches recorded and produces no notification and no push, from a punch or the absence job.
func TestP2ParentlessStudent(t *testing.T) {
	capture := &w10Capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })
	db, _ := setupThrowawayDB(t, "p2orphan")
	hash, _ := bcrypt.GenerateFromPassword([]byte(p2Pin), bcrypt.MinCost)
	k1Exec(t, db,
		fmt.Sprintf(`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (6501, 'P2 Gone', '+9647000006501', '%s')`, hash),
		`INSERT INTO students (id, full_name, rfid_tag, parent_id, grade, section, is_active, created_at) VALUES
			(6501, 'P2 Present Kid', '6501', 6501, 'G1', 'A', true, '2026-01-01 08:00'),
			(6502, 'P2 Absent Kid', '6502', 6501, 'G1', 'A', true, '2026-01-01 08:00')`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('P2-DEV', 'Gate', true)`,
		fmt.Sprintf(`INSERT INTO device_tokens (parent_id, token) VALUES (6501, '%s')`, b1Token("p2-gone")))
	auth.InitAuth("p2-test-secret")
	fcm := newB1FakeFCM(t)
	bg := &background.Group{}
	app := &handlers.AppEnv{DB: db, FCMClient: fcm.client(t), Background: bg, LoginLimiter: ratelimit.NewLoginLimiter(5, handlers.LoginWindow)}
	settle := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if pending, err := bg.Wait(ctx); err != nil {
			t.Fatalf("background work still running: %d", pending)
		}
	}
	token, _ := auth.GenerateParentToken(6501, "+9647000006501", 0)
	if rec := serve(t, app.AuthMiddleware(app.AccountDeleteHandler), http.MethodPost, p2DeletePath, token, `{"pin":"`+p2Pin+`"}`); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}

	pinToday(t, k1Today+" 07:50")
	rec := httptest.NewRecorder()
	app.ADMSHandler(rec, httptest.NewRequest("POST", "/iclock/cdata?SN=P2-DEV&table=ATTLOG", strings.NewReader("6501\t"+k1Today+" 07:50:00\t1\t1\n")))
	settle()
	if rec.Code != 200 || rec.Body.String() != "OK" {
		t.Errorf("punch: %d %s", rec.Code, rec.Body)
	}
	if n := k1Count(t, db, `SELECT COUNT(*) FROM attendance_logs WHERE student_id = 6501`); n != 1 {
		t.Errorf("punch of a parent-less child stored %d rows, want 1", n)
	}
	pinToday(t, k1Today+" 12:00")
	cron.ProcessDailyAbsences(db, app.FCMClient, bg)
	settle()
	if n := k1Count(t, db, `SELECT COUNT(*) FROM notifications`); n != 0 {
		t.Errorf("%d notifications for parent-less children, want 0", n)
	}
	if sent := fcm.take(); len(sent) != 0 {
		t.Errorf("pushes for parent-less children: %v", sent)
	}
	var processed bool
	for _, l := range capture.snapshot() {
		if l.Level >= slog.LevelError {
			t.Errorf("ERROR logged: %s %v", l.Msg, l.Attrs)
		}
		if l.Msg == "ProcessDailyAbsences: processed absence" && l.Attrs["student_id"] == "6502" {
			processed = true
		}
		if line := l.Msg + fmt.Sprint(l.Attrs); strings.Contains(line, "+9647000006501") || strings.Contains(line, "P2 Gone") {
			t.Errorf("the deleted parent in a log line: %s", line)
		}
	}
	if !processed {
		t.Errorf("the absence job did not process the parent-less absent child")
	}
}
