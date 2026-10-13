package warnings

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"future_kids/internal/handlers"
)

const o2Phone = "+9647000005101"

var o2TagSeq atomic.Int64

// o2NextTag returns a new valid rfid_tag for tests that create students through the API and do
// not care which tag they get.
func o2NextTag() string { return strconv.FormatInt(800000+o2TagSeq.Add(1), 10) }

// o2Server starts the real API on a database with one parent (so creates need no PIN) and a
// device, and returns it, its database and an admin token.
func o2Server(t *testing.T, label string) (*g3Server, *sql.DB, string) {
	t.Helper()
	srv, db, admin := l1Server(t, label, true)
	k1Exec(t, db,
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (5101, 'O2 Parent', '`+o2Phone+`', 'x')`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('O2-DEV', 'Gate', true)`)
	return srv, db, admin
}

// o2Body is a student write for the seeded parent; a nil tag leaves rfid_tag out.
func o2Body(tag any, kv ...any) map[string]any {
	m := map[string]any{"name": "O2 Kid", "parent_name": "O2 Parent", "parent_phone": o2Phone}
	if tag != nil {
		m["rfid_tag"] = tag
	}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func o2Students(t *testing.T, db *sql.DB) string {
	t.Helper()
	var s string
	db.QueryRow(`SELECT COALESCE(string_agg(id || '=' || rfid_tag || '|' || is_active, ',' ORDER BY id), '') FROM students`).Scan(&s)
	return s
}

func o2Tag(t *testing.T, db *sql.DB, id int) string {
	t.Helper()
	var tag string
	if err := db.QueryRow(`SELECT rfid_tag FROM students WHERE id = $1`, id).Scan(&tag); err != nil {
		t.Fatal(err)
	}
	return tag
}

func o2Keys(t *testing.T, body []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("not JSON: %s", body)
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, " ")
}

// TestO2Validation verifies the rfid_tag rule on create and update through the real API: ASCII
// digits, 1 to 14, not starting with 0, after trimming; required on create; on update an empty
// value keeps the stored one and a stored value that breaks the rule is accepted only when it is
// resent unchanged. A refused request writes nothing.
func TestO2Validation(t *testing.T) {
	srv, db, admin := o2Server(t, "o2valid")
	post := func(body any) a14Response {
		return a14Do(t, srv, "POST", "/api/admin/students", l1Header(admin), body)
	}
	put := func(body any) a14Response {
		return a14Do(t, srv, "PUT", "/api/admin/students", l1Header(admin), body)
	}
	format := handlers.RFIDTagFormatMessage
	invalid := []struct {
		name string
		tag  any
		msg  string
	}{
		{"zero", "0", format},
		{"leading zero", "01", format},
		{"leading zero, four digits", "0123", format},
		{"15 digits", "123456789012345", format},
		{"a letter", "12a", format},
		{"an inner space", "1 2", format},
		{"Arabic-Indic digits", "١٢٣", format},
		{"Arabic-Indic 33", "٣٣", format},
		{"Persian digits", "۱۲", format},
		{"a sign", "+12", format},
		{"NUL", "1\x002", format},
		{"the old placeholder", "admin-1791000000", format},
		{"51 characters", strings.Repeat("1", 51), "rfid_tag must be at most 50 characters"},
		{"a number, not a string", 33, "Invalid request body"},
	}

	t.Run("create", func(t *testing.T) {
		before := o2Students(t, db)
		for _, tc := range invalid {
			r := post(o2Body(tc.tag))
			if r.status != 400 || r.json(t)["message"] != tc.msg {
				t.Errorf("%s: %d %s, want 400 %q", tc.name, r.status, r.body, tc.msg)
			}
		}
		for name, body := range map[string]any{
			"invalid UTF-8": `{"name":"O2 Kid","parent_name":"O2 Parent","parent_phone":"` + o2Phone + `","rfid_tag":"1` + "\xff" + `"}`,
		} {
			if r := post(body); r.status != 400 || r.json(t)["message"] != format {
				t.Errorf("%s: %d %s", name, r.status, r.body)
			}
		}
		for name, body := range map[string]any{
			"omitted": o2Body(nil), "null": o2Body(nil, "rfid_tag", nil), "empty": o2Body(""), "blank": o2Body("   "),
		} {
			if r := post(body); r.status != 400 || r.json(t)["message"] != handlers.RFIDTagRequiredMessage {
				t.Errorf("%s: %d %s, want 400 %q", name, r.status, r.body, handlers.RFIDTagRequiredMessage)
			}
		}
		if r := post(map[string]any{"name": "O2 Kid", "parent_name": "O2 Parent"}); r.status != 400 || r.json(t)["message"] != "parent_phone is required" {
			t.Errorf("the other required fields are checked first: %d %s", r.status, r.body)
		}
		if after := o2Students(t, db); after != before {
			t.Errorf("refused creates wrote students:\nbefore %s\nafter  %s", before, after)
		}
		for _, tc := range []struct{ tag, stored string }{
			{"1", "1"}, {"99", "99"}, {"100", "100"}, {"1004", "1004"}, {"12345678901234", "12345678901234"}, {"  1005 ", "1005"}, {"\t2005\n", "2005"},
		} {
			r := post(o2Body(tc.tag))
			if r.status != 200 {
				t.Errorf("%q: %d %s", tc.tag, r.status, r.body)
				continue
			}
			data, _ := r.json(t)["data"].(map[string]any)
			id := int(data["id"].(float64))
			if got := o2Tag(t, db, id); got != tc.stored || data["rfid_tag"] != tc.stored {
				t.Errorf("%q: stored %q, returned %v, want %q", tc.tag, got, data["rfid_tag"], tc.stored)
			}
		}
	})

	t.Run("update", func(t *testing.T) {
		k1Exec(t, db, `INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES
			(5201, 'O2 Legacy', 'MM-7', 5101), (5202, 'O2 Placeholder', 'admin-123', 5101),
			(5203, 'O2 Spaced', 'MM-9 ', 5101), (5204, 'O2 Numeric', '3001', 5101)`)
		upd := func(id int, tag any) a14Response {
			b := o2Body(nil, "id", id, "rfid_tag", tag)
			if tag == "<omit>" {
				delete(b, "rfid_tag")
			}
			return put(b)
		}
		before := o2Students(t, db)
		for _, tc := range invalid {
			msg := tc.msg
			if msg == "Invalid request body" {
				msg = "Invalid request body or missing ID"
			}
			r := upd(5204, tc.tag)
			if r.status != 400 || r.json(t)["message"] != msg {
				t.Errorf("%s: %d %s, want 400 %q", tc.name, r.status, r.body, msg)
			}
		}
		for _, tc := range []struct {
			name string
			id   int
			tag  string
		}{
			{"another value on a legacy row", 5201, "MM-8"},
			{"a leading zero on a legacy row", 5201, "0123"},
			{"the legacy value of another student", 5204, "MM-7"},
			{"a legacy value trimmed differently from the stored one", 5203, "MM-9"},
			{"an unknown student", 999999, "MM-7"},
		} {
			if r := upd(tc.id, tc.tag); r.status != 400 || r.json(t)["message"] != format {
				t.Errorf("%s: %d %s, want 400 %q", tc.name, r.status, r.body, format)
			}
		}
		if after := o2Students(t, db); after != before {
			t.Errorf("refused updates wrote students:\nbefore %s\nafter  %s", before, after)
		}
		for _, tc := range []struct {
			name string
			id   int
			tag  any
			want string
		}{
			{"omitted keeps", 5204, "<omit>", "3001"},
			{"null keeps", 5204, nil, "3001"},
			{"empty keeps", 5204, "", "3001"},
			{"blank keeps", 5204, "   ", "3001"},
			{"the legacy value resent", 5201, "MM-7", "MM-7"},
			{"the legacy value resent with spaces", 5201, "  MM-7 ", "MM-7"},
			{"the placeholder resent", 5202, "admin-123", "admin-123"},
			{"a stored value with a trailing space, resent exactly", 5203, "MM-9 ", "MM-9 "},
			{"a valid value replaces a legacy one", 5201, "3002", "3002"},
			{"a valid value with spaces", 5204, " 3003 ", "3003"},
		} {
			if r := upd(tc.id, tc.tag); r.status != 200 {
				t.Errorf("%s: %d %s", tc.name, r.status, r.body)
			}
			if got := o2Tag(t, db, tc.id); got != tc.want {
				t.Errorf("%s: stored %q, want %q", tc.name, got, tc.want)
			}
		}
		if r := upd(999999, "3999"); r.status != 404 {
			t.Errorf("unknown student with a valid tag: %d %s", r.status, r.body)
		}
	})
}

// TestO2Suggestion verifies the rfid_tag 409: the message is unchanged, suggested_rfid_tag is the
// first number above the requested one that no student has (gaps, runs of taken numbers, inactive
// students, tags that are not numbers, the 14-digit maximum), on create and update, and under
// concurrency; no other error carries the field; the refused requests are in the activity log.
func TestO2Suggestion(t *testing.T) {
	srv, db, admin := o2Server(t, "o2suggest")
	k1Exec(t, db, `INSERT INTO students (id, full_name, rfid_tag, parent_id, is_active) VALUES
		(5301, 'S', '33', 5101, true),
		(5302, 'S', '40', 5101, true), (5303, 'S', '42', 5101, true),
		(5304, 'S', '50', 5101, true), (5305, 'S', '51', 5101, true), (5306, 'S', '52', 5101, true),
		(5307, 'S', '60', 5101, true), (5308, 'S', 'MM-60', 5101, true), (5309, 'S', '061', 5101, true), (5310, 'S', '62a', 5101, true), (5311, 'S', 'admin-61', 5101, true),
		(5312, 'S', '70', 5101, true), (5313, 'S', '71', 5101, false),
		(5314, 'S', '80', 5101, true), (5315, 'S', '81', 5101, true),
		(5316, 'S', '99999999999999', 5101, true), (5317, 'S', '99999999999998', 5101, true),
		(5318, 'S', '9999999999990', 5101, true)`)
	conflict := func(t *testing.T, r a14Response) any {
		t.Helper()
		m := r.json(t)
		if r.status != 409 || m["message"] != handlers.RFIDTagTakenMessage || m["status"] != "error" || o2Keys(t, r.body) != "message status suggested_rfid_tag" {
			t.Fatalf("%d %s", r.status, r.body)
		}
		return m["suggested_rfid_tag"]
	}
	for _, tc := range []struct {
		name, tag string
		want      any
	}{
		{"the next number is free", "33", "34"},
		{"a gap", "40", "41"},
		{"three taken in a row", "50", "53"},
		{"from inside a run", "51", "53"},
		{"tags that are not numbers are ignored", "60", "61"},
		{"an inactive student's tag is taken", "70", "72"},
		{"no 14-digit number above it", "99999999999999", nil},
		{"the only number above it is taken", "99999999999998", nil},
		{"13 digits growing to 14", "9999999999990", "9999999999991"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := conflict(t, a14Do(t, srv, "POST", "/api/admin/students", l1Header(admin), o2Body(tc.tag))); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("suggested %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("the exact body", func(t *testing.T) {
		r := a14Do(t, srv, "POST", "/api/admin/students", l1Header(admin), o2Body("33"))
		if want := `{"message":"rfid_tag is already used by another student","status":"error","suggested_rfid_tag":"34"}` + "\n"; string(r.body) != want || r.header.Get("Content-Type") != "application/json" {
			t.Errorf("body %q (%s), want %q", r.body, r.header.Get("Content-Type"), want)
		}
	})

	t.Run("an update to a taken tag", func(t *testing.T) {
		r := a14Do(t, srv, "PUT", "/api/admin/students", l1Header(admin), o2Body("81", "id", 5314))
		if got := conflict(t, r); got != "82" {
			t.Errorf("suggested %v, want 82", got)
		}
		if o2Tag(t, db, 5314) != "80" {
			t.Errorf("the refused update changed the tag")
		}
	})

	t.Run("no other error carries the field", func(t *testing.T) {
		for name, r := range map[string]a14Response{
			"400 format":        a14Do(t, srv, "POST", "/api/admin/students", l1Header(admin), o2Body("033")),
			"400 required":      a14Do(t, srv, "POST", "/api/admin/students", l1Header(admin), o2Body(nil)),
			"404 update":        a14Do(t, srv, "PUT", "/api/admin/students", l1Header(admin), o2Body("4001", "id", 999999)),
			"409 device serial": a14Do(t, srv, "POST", "/api/admin/devices", l1Header(admin), map[string]any{"serial_number": "O2-DEV"}),
		} {
			if keys := o2Keys(t, r.body); keys != "message status" || r.status < 400 {
				t.Errorf("%s: %d keys %q", name, r.status, keys)
			}
		}
		k1Exec(t, db, `ALTER TABLE parents RENAME TO parents_offline`)
		r := a14Do(t, srv, "POST", "/api/admin/students", l1Header(admin), o2Body("4002"))
		k1Exec(t, db, `ALTER TABLE parents_offline RENAME TO parents`)
		if keys := o2Keys(t, r.body); r.status != 500 || keys != "message status" {
			t.Errorf("500: %d keys %q", r.status, keys)
		}
	})

	t.Run("concurrent creates of one new tag: one 200, the rest 409 with another number", func(t *testing.T) {
		for _, tag := range []string{"90000", "91000"} {
			type result struct {
				status int
				hint   any
			}
			results := make(chan result, 20)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					r := a14Do(t, srv, "POST", "/api/admin/students", l1Header(admin), o2Body(tag, "name", fmt.Sprintf("O2 Race %d", i)))
					res := result{status: r.status}
					if r.status == 409 {
						res.hint = r.json(t)["suggested_rfid_tag"]
					}
					results <- res
				}(i)
			}
			close(start)
			wg.Wait()
			close(results)
			ok, conflicts := 0, 0
			for res := range results {
				switch res.status {
				case 200:
					ok++
				case 409:
					conflicts++
					if res.hint == nil || res.hint == tag {
						t.Errorf("%s: a 409 suggested %v", tag, res.hint)
					}
				default:
					t.Errorf("%s: status %d", tag, res.status)
				}
			}
			if ok != 1 || conflicts != 19 || k1Count(t, db, `SELECT COUNT(*) FROM students WHERE rfid_tag = $1`, tag) != 1 {
				t.Errorf("%s: %d created, %d conflicts", tag, ok, conflicts)
			}
		}
	})

	t.Run("refused creates are in the activity log", func(t *testing.T) {
		deadline := time.Now().Add(10 * time.Second)
		for {
			var c400, c409 int
			db.QueryRow(`SELECT COUNT(*) FILTER (WHERE status = 400), COUNT(*) FILTER (WHERE status = 409) FROM admin_activity_log WHERE action = 'student.create'`).Scan(&c400, &c409)
			if c400 >= 2 && c409 >= 10+19+19 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("activity log: %d refused (400) and %d conflicting (409) creates recorded", c400, c409)
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}

// TestO2DevicePunchesUnchanged verifies that a punch for a device PIN no student has, including
// the forms the new rule refuses, is still acknowledged with OK, stores nothing and is logged as
// an unknown PIN, while a punch for a valid rfid_tag is stored as before.
func TestO2DevicePunchesUnchanged(t *testing.T) {
	srv, db, admin := o2Server(t, "o2device")
	if r := a14Do(t, srv, "POST", "/api/admin/students", l1Header(admin), o2Body("1004")); r.status != 200 {
		t.Fatalf("create: %d %s", r.status, r.body)
	}
	for _, pin := range []string{"0123", "٣٣", "99990001"} {
		r := a14Do(t, srv, "POST", "/iclock/cdata?SN=O2-DEV&table=ATTLOG", nil, pin+"\t"+k1Today+" 07:10:00\t1\t1\n")
		if r.status != 200 || string(r.body) != "OK" {
			t.Errorf("unknown PIN %q: %d %q", pin, r.status, r.body)
		}
	}
	if n := k1Count(t, db, `SELECT COUNT(*) FROM attendance_logs`); n != 0 {
		t.Errorf("%d punches stored for unknown PINs", n)
	}
	r := a14Do(t, srv, "POST", "/iclock/cdata?SN=O2-DEV&table=ATTLOG", nil, "1004\t"+k1Today+" 07:10:00\t1\t1\n")
	if r.status != 200 || string(r.body) != "OK" || k1Count(t, db, `SELECT COUNT(*) FROM attendance_logs`) != 1 {
		t.Errorf("valid PIN: %d %q", r.status, r.body)
	}
	if !strings.Contains(srv.out.String(), "punches for unknown device PINs skipped") {
		t.Errorf("no unknown-PIN warning logged")
	}
}
