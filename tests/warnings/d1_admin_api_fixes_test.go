package warnings

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"future_kids/internal/auth"
	"future_kids/internal/handlers"
	"future_kids/internal/ratelimit"

	"golang.org/x/crypto/bcrypt"
)

// TestD1AdminAPIFixes verifies Task D1 on the admin API: a new parent needs a PIN (the server
// never invents one); a duplicate rfid_tag is 409 without partial writes; a leave for an unknown
// or inactive student is 404; device updates keep omitted fields and new devices are active;
// over-long values and out-of-range years are 400 naming the field; the parent's today list is
// ordered by student id and the admin daily report has a deterministic tie-breaker.
func TestD1AdminAPIFixes(t *testing.T) {
	db, _ := setupThrowawayDB(t, "d1")
	auth.InitAuth("d1-test-secret")
	app := &handlers.AppEnv{DB: db}
	admin, err := auth.GenerateAdminToken("admin", 0)
	if err != nil {
		t.Fatal(err)
	}
	students := app.AdminMiddleware(app.AdminStudentsHandler)
	devices := app.AdminMiddleware(app.AdminDevicesHandler)
	leaves := app.AdminMiddleware(app.AdminCreateLeaveHandler)
	settings := app.AdminMiddleware(app.AdminSettingsHandler)

	call := func(h http.HandlerFunc, method, target, token string, body any) (int, map[string]any) {
		t.Helper()
		var raw string
		if body != nil {
			b, _ := json.Marshal(body)
			raw = string(b)
		}
		rec := serve(t, h, method, target, token, raw)
		var m map[string]any
		json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	expect := func(t *testing.T, what string, code int, body map[string]any, want int, mention string) {
		t.Helper()
		msg := fmt.Sprint(body["message"])
		if code != want || (mention != "" && !strings.Contains(msg, mention)) {
			t.Errorf("%s: %d %q, want %d mentioning %q", what, code, msg, want, mention)
		}
	}
	student := func(name, phone, pin, rfid string) map[string]any {
		b := map[string]any{"name": name, "parent_name": name + " Parent", "parent_phone": phone, "rfid_tag": rfid}
		if pin != "" {
			b["parent_pin"] = pin
		}
		return b
	}

	// ── D1: PIN required for a new parent ───────────────────────────────────────
	t.Run("D1/new parent without a PIN is 400 and writes nothing", func(t *testing.T) {
		code, body := call(students, "POST", "/api/admin/students", admin, student("D1 New", "+9647000000961", "", "D1-NEW"))
		expect(t, "create", code, body, 400, "parent_pin")
		if n := count(`SELECT COUNT(*) FROM parents WHERE phone_number = '+9647000000961'`) + count(`SELECT COUNT(*) FROM students WHERE rfid_tag = 'D1-NEW'`); n != 0 {
			t.Errorf("%d rows written for a rejected create", n)
		}
	})

	t.Run("D1/a blank PIN counts as missing", func(t *testing.T) {
		code, body := call(students, "POST", "/api/admin/students", admin, student("D1 Blank", "+9647000000962", "   ", "D1-BLANK"))
		expect(t, "create", code, body, 400, "parent_pin")
		if n := count(`SELECT COUNT(*) FROM parents WHERE phone_number = '+9647000000962'`); n != 0 {
			t.Errorf("a parent was created with a blank PIN")
		}
	})

	t.Run("D1/a valid PIN creates the parent and is the stored PIN", func(t *testing.T) {
		code, body := call(students, "POST", "/api/admin/students", admin, student("D1 Valid", "+9647000000963", "Pd8?Xt3m%Ka6wN2g", "D1-VALID"))
		expect(t, "create", code, body, 200, "")
		var hash string
		db.QueryRow(`SELECT pin_code FROM parents WHERE phone_number = '+9647000000963'`).Scan(&hash)
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte("Pd8?Xt3m%Ka6wN2g")) != nil {
			t.Errorf("the stored PIN does not match the one sent")
		}
	})

	t.Run("D1/an existing parent needs no PIN and keeps PIN, sessions and devices", func(t *testing.T) {
		var parentID, version int
		var hash string
		db.QueryRow(`SELECT id, pin_code, session_version FROM parents WHERE phone_number = '+9647000000963'`).Scan(&parentID, &hash, &version)
		if _, err := db.Exec(`INSERT INTO device_tokens (parent_id, token) VALUES ($1, 'test-device-d1-existing-000000')`, parentID); err != nil {
			t.Fatal(err)
		}
		for i, pin := range []string{"", "   "} {
			code, body := call(students, "POST", "/api/admin/students", admin, student("D1 Valid", "+9647000000963", pin, fmt.Sprintf("D1-SIB-%d", i)))
			expect(t, fmt.Sprintf("second child with PIN %q", pin), code, body, 200, "")
		}
		var hashAfter string
		var versionAfter int
		db.QueryRow(`SELECT pin_code, session_version FROM parents WHERE id = $1`, parentID).Scan(&hashAfter, &versionAfter)
		if hashAfter != hash || versionAfter != version || count(`SELECT COUNT(*) FROM device_tokens WHERE parent_id = $1`, parentID) != 1 {
			t.Errorf("PIN changed %v, session_version %d → %d, device tokens %d; want unchanged", hashAfter != hash, version, versionAfter, count(`SELECT COUNT(*) FROM device_tokens WHERE parent_id = $1`, parentID))
		}
	})

	t.Run("D1/concurrent creates without a PIN cannot make a PIN-less parent", func(t *testing.T) {
		var wg sync.WaitGroup
		codes := make([]int, 20)
		for i := range codes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				b, _ := json.Marshal(student("D1 Race", "+9647000000964", "", fmt.Sprintf("D1-RACE-%d", i)))
				req := httptest.NewRequest("POST", "/api/admin/students", strings.NewReader(string(b)))
				req.Header.Set("Authorization", "Bearer "+admin)
				rec := httptest.NewRecorder()
				students(rec, req)
				codes[i] = rec.Code
			}(i)
		}
		wg.Wait()
		for i, c := range codes {
			if c != 400 {
				t.Errorf("concurrent create %d: %d, want 400", i, c)
			}
		}
		if n := count(`SELECT COUNT(*) FROM parents WHERE phone_number = '+9647000000964'`); n != 0 {
			t.Errorf("%d parents created without a PIN", n)
		}
	})

	// ── D2: duplicate rfid_tag ──────────────────────────────────────────────────
	var sibling int
	db.QueryRow(`SELECT id FROM students WHERE rfid_tag = 'D1-SIB-0'`).Scan(&sibling)

	t.Run("D2/duplicate tag on create is 409 and writes nothing", func(t *testing.T) {
		code, body := call(students, "POST", "/api/admin/students", admin, student("D2 Dup", "+9647000000965", "Yb4!Sq7e-Cv2hF9n", "D1-VALID"))
		expect(t, "create", code, body, 409, "rfid_tag")
		if n := count(`SELECT COUNT(*) FROM parents WHERE phone_number = '+9647000000965'`); n != 0 {
			t.Errorf("a parent was written by a failed create")
		}
	})

	t.Run("D2/duplicate tag on update is 409; the student's own tag is fine", func(t *testing.T) {
		code, body := call(students, "PUT", "/api/admin/students", admin, map[string]any{"id": sibling, "name": "D1 Valid", "parent_name": "D1 Valid Parent", "parent_phone": "+9647000000963", "rfid_tag": "D1-VALID"})
		expect(t, "update to another student's tag", code, body, 409, "rfid_tag")
		code, body = call(students, "PUT", "/api/admin/students", admin, map[string]any{"id": sibling, "name": "D1 Valid", "parent_name": "D1 Valid Parent", "parent_phone": "+9647000000963", "rfid_tag": "D1-SIB-0"})
		expect(t, "update keeping its own tag", code, body, 200, "")
	})

	t.Run("D2/other database errors stay 500", func(t *testing.T) {
		if _, err := db.Exec(`ALTER TABLE students ADD CONSTRAINT d1_no_boom CHECK (full_name <> 'D2 Boom')`); err != nil {
			t.Fatal(err)
		}
		defer db.Exec(`ALTER TABLE students DROP CONSTRAINT d1_no_boom`)
		code, body := call(students, "POST", "/api/admin/students", admin, student("D2 Boom", "+9647000000966", "Yb4!Sq7e-Cv2hF9n", "D2-BOOM"))
		expect(t, "check violation", code, body, 500, "")
		if n := count(`SELECT COUNT(*) FROM parents WHERE phone_number = '+9647000000966'`); n != 0 {
			t.Errorf("a parent was written by a failed create")
		}
	})

	// ── D3: leave for an unknown student ────────────────────────────────────────
	t.Run("D3/unknown or inactive student is 404; valid leave works", func(t *testing.T) {
		code, body := call(leaves, "POST", "/api/admin/leaves", admin, map[string]any{"student_id": 999999, "leave_date": "2026-03-02"})
		expect(t, "unknown student", code, body, 404, "")
		if _, err := db.Exec(`INSERT INTO students (id, full_name, rfid_tag, is_active) VALUES (9799, 'D3 Inactive', 'D3-INACTIVE', false)`); err != nil {
			t.Fatal(err)
		}
		code, body = call(leaves, "POST", "/api/admin/leaves", admin, map[string]any{"student_id": 9799, "leave_date": "2026-03-02"})
		expect(t, "inactive student", code, body, 404, "")
		code, body = call(leaves, "POST", "/api/admin/leaves", admin, map[string]any{"student_id": -4, "leave_date": "2026-03-02"})
		expect(t, "negative id", code, body, 400, "student_id")
		code, body = call(leaves, "POST", "/api/admin/leaves", admin, map[string]any{"student_id": sibling, "leave_date": "2026-03-02"})
		expect(t, "valid leave", code, body, 200, "")
	})

	// ── D4: devices ─────────────────────────────────────────────────────────────
	deviceState := func(sn string) (string, bool) {
		var loc string
		var active bool
		db.QueryRow(`SELECT COALESCE(location_name, ''), is_active FROM devices WHERE serial_number = $1`, sn).Scan(&loc, &active)
		return loc, active
	}
	t.Run("D4/a device created without is_active is active and its punches are stored", func(t *testing.T) {
		code, body := call(devices, "POST", "/api/admin/devices", admin, map[string]any{"serial_number": "D4-DEV-1", "location_name": "Gate"})
		expect(t, "create", code, body, 200, "")
		if _, active := deviceState("D4-DEV-1"); !active {
			t.Errorf("device created without is_active is disabled")
		}
		rec := httptest.NewRecorder()
		app.ADMSHandler(rec, httptest.NewRequest("POST", "/iclock/cdata?SN=D4-DEV-1&table=ATTLOG", strings.NewReader("D1-VALID\t2026-03-02 07:15:00\t1\t1\n")))
		if rec.Code != 200 || rec.Body.String() != "OK" || count(`SELECT COUNT(*) FROM attendance_logs WHERE device_sn = 'D4-DEV-1'`) != 1 {
			t.Errorf("ADMS push from the new device: %d %q, %d punches stored; want 200 OK and 1", rec.Code, rec.Body.String(), count(`SELECT COUNT(*) FROM attendance_logs WHERE device_sn = 'D4-DEV-1'`))
		}
	})

	t.Run("D4/update keeps every omitted field", func(t *testing.T) {
		code, body := call(devices, "PUT", "/api/admin/devices", admin, map[string]any{"serial_number": "D4-DEV-1", "location_name": "Side Gate"})
		expect(t, "location only", code, body, 200, "")
		if loc, active := deviceState("D4-DEV-1"); loc != "Side Gate" || !active {
			t.Errorf("after a location-only update: %q, active %v; want Side Gate and active", loc, active)
		}
		code, body = call(devices, "PUT", "/api/admin/devices", admin, map[string]any{"serial_number": "D4-DEV-1", "is_active": false})
		expect(t, "is_active only", code, body, 200, "")
		if loc, active := deviceState("D4-DEV-1"); loc != "Side Gate" || active {
			t.Errorf("after an is_active-only update: %q, active %v; want Side Gate and inactive", loc, active)
		}
		code, body = call(devices, "PUT", "/api/admin/devices", admin, map[string]any{"serial_number": "D4-DEV-1"})
		expect(t, "neither field", code, body, 400, "")
		code, body = call(devices, "PUT", "/api/admin/devices", admin, map[string]any{"serial_number": "D4-DEV-1", "location_name": ""})
		expect(t, "empty location", code, body, 200, "")
		if loc, _ := deviceState("D4-DEV-1"); loc != "" {
			t.Errorf("a present empty location_name should clear the location, got %q", loc)
		}
		code, body = call(devices, "PUT", "/api/admin/devices", admin, map[string]any{"serial_number": "D4-NONE", "is_active": true})
		expect(t, "unknown device", code, body, 404, "")
	})

	// ── D5: lengths ─────────────────────────────────────────────────────────────
	t.Run("D5/each field at its limit works and one more is 400 naming it", func(t *testing.T) {
		n := 0
		next := func() (string, string) {
			n++
			return fmt.Sprintf("+96470000098%02d", n), fmt.Sprintf("D5-%02d", n)
		}
		ar := func(k int) string { return strings.Repeat("س", k) }
		for _, c := range []struct {
			field string
			limit int
			set   func(b map[string]any, v string)
		}{
			{"name", 100, func(b map[string]any, v string) { b["name"] = v }},
			{"parent_name", 255, func(b map[string]any, v string) { b["parent_name"] = v }},
			{"rfid_tag", 50, func(b map[string]any, v string) { b["rfid_tag"] = v }},
			{"grade", 50, func(b map[string]any, v string) { b["grade"] = v }},
			{"section", 50, func(b map[string]any, v string) { b["section"] = v }},
		} {
			for _, k := range []int{c.limit, c.limit + 1} {
				phone, rfid := next()
				b := student("D5", phone, "Yb4!Sq7e-Cv2hF9n", rfid)
				c.set(b, ar(k))
				code, body := call(students, "POST", "/api/admin/students", admin, b)
				if k == c.limit {
					expect(t, fmt.Sprintf("%s of %d", c.field, k), code, body, 200, "")
				} else {
					expect(t, fmt.Sprintf("%s of %d", c.field, k), code, body, 400, fmt.Sprintf("%s must be at most %d characters", c.field, c.limit))
				}
			}
		}
		phone, _ := next()
		code, body := call(students, "POST", "/api/admin/students", admin, student("D5", phone, "Yb4!Sq7e-Cv2hF9n", strings.Repeat("t", 50)+"   "))
		expect(t, "rfid_tag of 50 with trailing spaces", code, body, 200, "")
		code, body = call(students, "PUT", "/api/admin/students", admin, map[string]any{"id": sibling, "name": ar(101), "parent_name": "D1 Valid Parent", "parent_phone": "+9647000000963"})
		expect(t, "update name of 101", code, body, 400, "name must be at most 100 characters")
		for _, k := range []int{50, 51} {
			want, mention := 200, ""
			if k == 51 {
				want, mention = 400, "serial_number must be at most 50 characters"
			}
			code, body := call(devices, "POST", "/api/admin/devices", admin, map[string]any{"serial_number": strings.Repeat("S", k)})
			expect(t, fmt.Sprintf("serial_number of %d", k), code, body, want, mention)
			if k == 51 {
				mention = "location_name must be at most 50 characters"
			}
			code, body = call(devices, "POST", "/api/admin/devices", admin, map[string]any{"serial_number": fmt.Sprintf("D5-LOC-%d", k), "location_name": ar(k)})
			expect(t, fmt.Sprintf("create location_name of %d", k), code, body, want, mention)
			code, body = call(devices, "PUT", "/api/admin/devices", admin, map[string]any{"serial_number": "D4-DEV-1", "location_name": ar(k)})
			expect(t, fmt.Sprintf("update location_name of %d", k), code, body, want, mention)
		}
		for _, k := range []int{100, 101} {
			want, mention := 200, ""
			if k == 101 {
				want, mention = 400, "key must be at most 100 characters"
			}
			code, body := call(settings, "PUT", "/api/admin/settings", admin, map[string]any{"key": ar(k), "value": "v"})
			expect(t, fmt.Sprintf("setting key of %d", k), code, body, want, mention)
		}
	})

	t.Run("D1/a PIN that is set must follow the parent_pin rule", func(t *testing.T) {
		for i, c := range []struct {
			pin  string
			want int
		}{
			{"48261", 400},
			{"4826173", 400},
			{"48a617", 400},
			{"482 17", 400},
			{" 482617", 400},
			{"٤٨٢٦١٧", 400},
			{"۴۸۲۶۱۷", 400},
			{"Yb4!Sq7e-Cv2hF9n", 200},
		} {
			phone := fmt.Sprintf("+96470000009%02d", 70+i)
			code, body := call(students, "POST", "/api/admin/students", admin, student("D1 Format", phone, c.pin, fmt.Sprintf("D1-FMT-%d", i)))
			mention := ""
			if c.want == 400 {
				mention = handlers.PINFormatMessage
			}
			expect(t, fmt.Sprintf("create with PIN %q", c.pin), code, body, c.want, mention)
			if c.want == 400 && count(`SELECT COUNT(*) FROM parents WHERE phone_number = $1`, phone) != 0 {
				t.Errorf("a parent was written with PIN %q", c.pin)
			}
		}
		code, body := call(students, "PUT", "/api/admin/students", admin, map[string]any{"id": sibling, "name": "D1 Valid", "parent_name": "D1 Valid Parent", "parent_phone": "+9647000000963", "parent_pin": "7319"})
		expect(t, "update with a 4-digit PIN", code, body, 400, handlers.PINFormatMessage)
	})

	t.Run("D1/login still accepts a legacy 4-digit PIN", func(t *testing.T) {
		hash, _ := bcrypt.GenerateFromPassword([]byte("7319"), bcrypt.MinCost)
		if _, err := db.Exec(`INSERT INTO parents (full_name, phone_number, pin_code) VALUES ('D1 Legacy', '+9647000000990', $1)`, string(hash)); err != nil {
			t.Fatal(err)
		}
		loginApp := &handlers.AppEnv{DB: db, LoginLimiter: ratelimit.NewLoginLimiter(5, time.Minute)}
		code, body := call(loginApp.MobileLoginHandler, "POST", "/api/mobile/login", "", map[string]string{"phone": "07000000990", "pin": "7319"})
		expect(t, "login with the legacy PIN", code, body, 200, "")
	})

	// ── D6: year range ──────────────────────────────────────────────────────────
	var parentID, parentVersion int
	db.QueryRow(`SELECT id, session_version FROM parents WHERE phone_number = '+9647000000963'`).Scan(&parentID, &parentVersion)
	parent, err := auth.GenerateParentToken(parentID, "+9647000000963", parentVersion)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("D6/years outside 2000-2100 are 400 everywhere", func(t *testing.T) {
		type endpoint struct {
			name   string
			h      http.HandlerFunc
			method string
			target func(y string) string
			body   func(y string) any
			token  string
		}
		for _, e := range []endpoint{
			{"admin attendance", app.AdminMiddleware(app.AdminDailyAttendanceHandler), "GET", func(y string) string { return "/api/admin/attendance?date=" + y + "-01-01" }, nil, admin},
			{"admin export", app.AdminMiddleware(app.AdminExportExcelHandler), "GET", func(y string) string { return "/api/admin/export/excel?date=" + y + "-01-01" }, nil, admin},
			{"leave", leaves, "POST", func(string) string { return "/api/admin/leaves" }, func(y string) any { return map[string]any{"student_id": sibling, "leave_date": y + "-01-01"} }, admin},
			{"parent summary", app.AuthMiddleware(app.MobileAttendanceSummaryHandler), "GET", func(y string) string { return "/api/mobile/attendance/summary?month=" + y + "-01" }, nil, parent},
			{"parent monthly", app.AuthMiddleware(app.MobileMonthlyAttendanceHandler), "GET", func(y string) string { return "/api/mobile/attendance/monthly?month=" + y + "-01" }, nil, parent},
		} {
			for _, y := range []string{"0000", "1999", "2101", "2000", "2100"} {
				var body any
				if e.body != nil {
					body = e.body(y)
				}
				rec := serve(t, e.h, e.method, e.target(y), e.token, func() string {
					if body == nil {
						return ""
					}
					b, _ := json.Marshal(body)
					return string(b)
				}())
				want := 400
				if y == "2000" || y == "2100" {
					want = 200
				}
				if rec.Code != want || (want == 400 && !strings.Contains(rec.Body.String(), "2000 to 2100")) {
					t.Errorf("%s with year %s: %d %.120s, want %d", e.name, y, rec.Code, rec.Body.String(), want)
				}
			}
		}
	})

	// ── D7: ordering ────────────────────────────────────────────────────────────
	t.Run("D7/the parent's today list is ordered by student id", func(t *testing.T) {
		if _, err := db.Exec(`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (9700, 'D7 Parent', '+9647000000970', 'x');
			INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES (9703, 'D7 C', 'D7-3', 9700), (9701, 'D7 A', 'D7-1', 9700), (9704, 'D7 D', 'D7-4', 9700), (9702, 'D7 B', 'D7-2', 9700)`); err != nil {
			t.Fatal(err)
		}
		tok, _ := auth.GenerateParentToken(9700, "+9647000000970", 0)
		rec := serve(t, app.AuthMiddleware(app.MobileTodayAttendanceHandler), "GET", "/api/mobile/attendance/today", tok, "")
		var resp struct {
			Data []struct {
				StudentID int `json:"student_id"`
			} `json:"data"`
		}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		var ids []int
		for _, d := range resp.Data {
			ids = append(ids, d.StudentID)
		}
		if fmt.Sprint(ids) != "[9701 9702 9703 9704]" {
			t.Errorf("today order %v, want [9701 9702 9703 9704]", ids)
		}
	})

	t.Run("D7/the admin daily report breaks name ties by student id", func(t *testing.T) {
		if _, err := db.Exec(`INSERT INTO students (id, full_name, rfid_tag, created_at) VALUES (9712, 'D7 Same Name', 'D7-12', '2026-01-01 08:00'), (9711, 'D7 Same Name', 'D7-11', '2026-01-01 08:00')`); err != nil {
			t.Fatal(err)
		}
		rec := serve(t, app.AdminMiddleware(app.AdminDailyAttendanceHandler), "GET", "/api/admin/attendance?date=2026-03-04", admin, "")
		var resp struct {
			Data []struct {
				StudentID int `json:"student_id"`
			} `json:"data"`
		}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		var same []int
		for _, d := range resp.Data {
			if d.StudentID == 9711 || d.StudentID == 9712 {
				same = append(same, d.StudentID)
			}
		}
		if fmt.Sprint(same) != "[9711 9712]" {
			t.Errorf("same-name students in order %v, want [9711 9712]", same)
		}
	})
}
