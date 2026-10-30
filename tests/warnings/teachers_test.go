package warnings

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"future_kids/internal/background"
	"future_kids/internal/handlers"
)

const tchPath = "/api/admin/teachers"

// tchBody is a valid teacher create body with kv overriding fields (a nil value deletes one).
func tchBody(kv ...any) map[string]any {
	m := map[string]any{"first_name": "أحمد", "father_name": "علي", "grandfather_name": "حسن", "phone": "07701230001", "gender": "male"}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == nil {
			delete(m, kv[i].(string))
		} else {
			m[kv[i].(string)] = kv[i+1]
		}
	}
	return m
}

// tchCreate creates a teacher and returns its id, failing the test unless it gets 200.
func tchCreate(t *testing.T, srv *g3Server, admin string, body map[string]any) int {
	t.Helper()
	r := a14Do(t, srv, "POST", tchPath, l1Header(admin), body)
	if r.status != 200 {
		t.Fatalf("create teacher %v: %d %s", body, r.status, r.body)
	}
	var id int
	fmt.Sscan(fmt.Sprint(r.json(t)["data"].(map[string]any)["id"]), &id)
	return id
}

// tchData returns the "data" object of a write response or the list of a list response.
func tchData(t *testing.T, r a14Response) map[string]any {
	t.Helper()
	d, _ := r.json(t)["data"].(map[string]any)
	return d
}

func tchList(t *testing.T, r a14Response) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, it := range r.json(t)["data"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

func tchExpect(t *testing.T, what string, r a14Response, status int, message string) {
	t.Helper()
	if r.status != status || (message != "" && fmt.Sprint(r.json(t)["message"]) != message) {
		t.Errorf("%s: %d %s, want %d %q", what, r.status, r.body, status, message)
	}
}

// TestTeachersValidation verifies the create rules: required and trimmed names, the phone in any
// Iraqi format stored canonically, gender, status, the 1 to 99 rfid_tag and the length limits.
func TestTeachersValidation(t *testing.T) {
	srv, db, admin := l1Server(t, "tchvalid", true)
	post := func(body any) a14Response { return a14Do(t, srv, "POST", tchPath, l1Header(admin), body) }
	long := func(n int) string { return strings.Repeat("ب", n) }

	for _, c := range []struct {
		name string
		body any
		msg  string
	}{
		{"first_name missing", tchBody("first_name", nil), "first_name is required"},
		{"father_name blank", tchBody("father_name", "   "), "father_name is required"},
		{"grandfather_name missing", tchBody("grandfather_name", nil), "grandfather_name is required"},
		{"phone missing", tchBody("phone", nil), "phone is required"},
		{"gender missing", tchBody("gender", nil), "gender is required"},
		{"phone not Iraqi", tchBody("phone", "12345"), handlers.TeacherPhoneMessage},
		{"gender capitalised", tchBody("gender", "Male"), handlers.TeacherGenderMessage},
		{"gender other", tchBody("gender", "other"), handlers.TeacherGenderMessage},
		{"status unknown", tchBody("status", "deleted"), handlers.TeacherStatusMessage},
		{"first_name 101 characters", tchBody("first_name", long(101)), "first_name must be at most 100 characters"},
		{"grandfather_name 101 characters", tchBody("grandfather_name", long(101)), "grandfather_name must be at most 100 characters"},
		{"specialization 101 characters", tchBody("specialization", long(101)), "specialization must be at most 100 characters"},
		{"notes 501 characters", tchBody("notes", long(501)), "notes must be at most 500 characters"},
		{"rfid_tag 0", tchBody("rfid_tag", "0"), handlers.TeacherRFIDTagMessage},
		{"rfid_tag 01", tchBody("rfid_tag", "01"), handlers.TeacherRFIDTagMessage},
		{"rfid_tag 100", tchBody("rfid_tag", "100"), handlers.TeacherRFIDTagMessage},
		{"rfid_tag 1a", tchBody("rfid_tag", "1a"), handlers.TeacherRFIDTagMessage},
		{"rfid_tag Arabic-Indic", tchBody("rfid_tag", "٣"), handlers.TeacherRFIDTagMessage},
		{"name not a string", tchBody("first_name", 5), "Invalid request body"},
		{"malformed JSON", "{", "Invalid request body"},
	} {
		tchExpect(t, c.name, post(c.body), 400, c.msg)
	}
	tchExpect(t, "oversized body", post(`{"pad":"`+strings.Repeat("a", 70<<10)+`"}`), 413, "")
	tchExpect(t, "no token", a14Do(t, srv, "POST", tchPath, nil, tchBody()), 401, "")
	if n := k1Count(t, db, `SELECT COUNT(*) FROM teachers`); n != 0 {
		t.Fatalf("%d teachers written by refused requests", n)
	}

	r := post(tchBody("first_name", "  أحمد  ", "phone", "٠٧٧٠١٢٣٠٠٠٢", "rfid_tag", " 7 ", "specialization", "  ", "notes", "  ملاحظة "))
	tchExpect(t, "valid, trimmed", r, 200, "Teacher created")
	d := tchData(t, r)
	for k, want := range map[string]any{"first_name": "أحمد", "full_name": "أحمد علي حسن", "phone": "+9647701230002", "gender": "male",
		"status": "active", "specialization": nil, "notes": "ملاحظة", "rfid_tag": "7"} {
		if fmt.Sprint(d[k]) != fmt.Sprint(want) {
			t.Errorf("created %s = %v, want %v", k, d[k], want)
		}
	}
	if ca, ua := fmt.Sprint(d["created_at"]), fmt.Sprint(d["updated_at"]); !strings.HasSuffix(ca, "+03:00") || ca != ua {
		t.Errorf("created_at %s, updated_at %s: want equal, with +03:00", ca, ua)
	}
	for i, tag := range []string{"1", "99"} {
		tchExpect(t, "rfid_tag "+tag, post(tchBody("phone", fmt.Sprintf("+96477012301%02d", i), "rfid_tag", tag, "status", "on_leave", "gender", "female")), 200, "Teacher created")
	}
}

// TestTeachersConflicts verifies the 409s: another teacher's phone (any format) or rfid_tag, a
// tag held by an older student, on create and on update; a parent's phone is allowed.
func TestTeachersConflicts(t *testing.T) {
	srv, db, admin := l1Server(t, "tchconflict", true)
	a := tchCreate(t, srv, admin, tchBody("phone", "07701230011", "rfid_tag", "11"))
	b := tchCreate(t, srv, admin, tchBody("phone", "07701230012", "rfid_tag", "12"))
	k1Exec(t, db, `INSERT INTO students (full_name, rfid_tag) VALUES ('Legacy Staff-Range Kid', '50')`)
	p2Student(t, srv, admin, "A Parent's Kid", "Parent", "+9647701230099", p2Pin, "4501")
	before := k1Count(t, db, `SELECT COUNT(*) FROM teachers`)

	post := func(body map[string]any) a14Response { return a14Do(t, srv, "POST", tchPath, l1Header(admin), body) }
	tchExpect(t, "phone of another teacher, other format", post(tchBody("phone", "+964 770 123 0011")), 409, handlers.TeacherPhoneTakenMessage)
	tchExpect(t, "rfid_tag of another teacher", post(tchBody("phone", "07701230013", "rfid_tag", "11")), 409, handlers.TeacherTagTakenMessage)
	tchExpect(t, "rfid_tag of a student", post(tchBody("phone", "07701230014", "rfid_tag", "50")), 409, handlers.TeacherTagStudentMessage)
	if n := k1Count(t, db, `SELECT COUNT(*) FROM teachers`); n != before {
		t.Errorf("a refused create wrote a teacher")
	}
	tchExpect(t, "phone of a parent", post(tchBody("phone", "07701230099")), 200, "Teacher created")

	put := func(id int, kv ...any) a14Response {
		return a14Do(t, srv, "PUT", tchPath, l1Header(admin), tchBody(append([]any{"id", id, "status", "active"}, kv...)...))
	}
	tchExpect(t, "update to another teacher's phone", put(b, "phone", "07701230011", "rfid_tag", "12"), 409, handlers.TeacherPhoneTakenMessage)
	tchExpect(t, "update to another teacher's rfid_tag", put(b, "phone", "07701230012", "rfid_tag", "11"), 409, handlers.TeacherTagTakenMessage)
	tchExpect(t, "update to a student's rfid_tag", put(b, "phone", "07701230012", "rfid_tag", "50"), 409, handlers.TeacherTagStudentMessage)
	tchExpect(t, "update keeping its own phone and tag", put(a, "phone", "07701230011", "rfid_tag", "11", "first_name", "خالد"), 200, "Teacher updated")
	var tag string
	db.QueryRow(`SELECT rfid_tag FROM teachers WHERE id = $1`, b).Scan(&tag)
	if tag != "12" {
		t.Errorf("a refused update changed the tag to %q", tag)
	}
}

// TestTeachersUpdate verifies PUT: a full replacement (an omitted optional field is cleared),
// status required, updated_at refreshed, 404 for an unknown id, and the activity log entries.
func TestTeachersUpdate(t *testing.T) {
	srv, db, admin := l1Server(t, "tchupdate", true)
	// Activity rows are inserted in the background after each answer, so wait for every row
	// before the next request: only then does the id order follow the request order.
	logged := 1
	recorded := func() { t.Helper(); logged++; l1WaitCount(t, db, logged) }
	id := tchCreate(t, srv, admin, tchBody("rfid_tag", "21", "specialization", "العلوم", "notes", "n"))
	recorded()
	r := a14Do(t, srv, "GET", tchPath+fmt.Sprintf("?id=%d", id), l1Header(admin), nil)
	created := tchList(t, r)[0]
	time.Sleep(1100 * time.Millisecond)

	put := func(body map[string]any) a14Response {
		t.Helper()
		r := a14Do(t, srv, "PUT", tchPath, l1Header(admin), body)
		recorded()
		return r
	}
	tchExpect(t, "status missing", put(tchBody("id", id)), 400, "status is required")
	tchExpect(t, "id missing", put(tchBody("status", "active")), 400, handlers.TeacherIDMessage)
	tchExpect(t, "id negative", put(tchBody("id", -1, "status", "active")), 400, handlers.TeacherIDMessage)
	tchExpect(t, "unknown id", put(tchBody("id", 999999, "status", "active")), 404, handlers.TeacherNotFoundMessage)

	r = put(tchBody("id", id, "status", "retired", "first_name", "خالد", "gender", "female", "phone", "+9647701230077"))
	tchExpect(t, "full replacement", r, 200, "Teacher updated")
	d := tchData(t, r)
	for k, want := range map[string]any{"first_name": "خالد", "full_name": "خالد علي حسن", "gender": "female", "status": "retired",
		"phone": "+9647701230077", "rfid_tag": nil, "specialization": nil, "notes": nil, "created_at": created["created_at"]} {
		if fmt.Sprint(d[k]) != fmt.Sprint(want) {
			t.Errorf("updated %s = %v, want %v", k, d[k], want)
		}
	}
	if fmt.Sprint(d["updated_at"]) <= fmt.Sprint(created["updated_at"]) {
		t.Errorf("updated_at %v did not advance past %v", d["updated_at"], created["updated_at"])
	}

	rows := l1Rows(t, db, "action LIKE 'teacher.%'")
	var got []string
	for _, row := range rows {
		got = append(got, fmt.Sprintf("%s %d %s %s", row.Action, row.Status, l1Null(row.TargetType), l1Null(row.TargetID)))
	}
	want := []string{
		fmt.Sprintf("teacher.create 200 teacher %d", id),
		"teacher.update 400 teacher " + fmt.Sprint(id), "teacher.update 400 - -", "teacher.update 400 - -",
		"teacher.update 404 teacher 999999", fmt.Sprintf("teacher.update 200 teacher %d", id),
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("activity log:\n got %q\nwant %q", got, want)
	}
	if n := k1Count(t, db, `SELECT COUNT(*) FROM admin_activity_log WHERE concat_ws('|', detail, target_id) ~ '7701230|خالد|أحمد'`); n != 0 {
		t.Errorf("%d activity rows contain a name or phone", n)
	}
}

// TestTeachersList verifies the list: sorted by full name, every status by default, the status
// filter, search by name or phone digits in any format, and ?id for one teacher.
func TestTeachersList(t *testing.T) {
	srv, _, admin := l1Server(t, "tchlist", true)
	tchCreate(t, srv, admin, tchBody("first_name", "زيد", "phone", "07701230031"))
	tchCreate(t, srv, admin, tchBody("first_name", "باسم", "phone", "07801230032", "status", "retired"))
	c := tchCreate(t, srv, admin, tchBody("first_name", "أمل", "father_name", "كريم", "phone", "07501230033", "status", "on_leave", "gender", "female"))
	get := func(q string) a14Response { return a14Do(t, srv, "GET", tchPath+q, l1Header(admin), nil) }
	names := func(r a14Response) string {
		var out []string
		for _, it := range tchList(t, r) {
			out = append(out, fmt.Sprint(it["first_name"]))
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct{ q, want string }{
		{"", "أمل,باسم,زيد"},
		{"?status=retired", "باسم"},
		{"?status=active", "زيد"},
		{"?search=" + "كريم", "أمل"},
		{"?search=%D8%A8%D8%A7%D8%B3", "باسم"},
		{"?search=0780%20123", "باسم"},
		{"?search=%2B964%20750", "أمل"},
		{"?search=" + "٠٧٧٠١٢٣٠٠٣١", "زيد"},
		{"?search=%25", ""},
		{"?search=12", ""},
		{fmt.Sprintf("?id=%d", c), "أمل"},
		{fmt.Sprintf("?id=%d&status=active", c), "__404"},
	} {
		r := get(c.q)
		if c.want == "__404" {
			tchExpect(t, c.q, r, 404, handlers.TeacherNotFoundMessage)
			continue
		}
		if r.status != 200 || names(r) != c.want {
			t.Errorf("GET %s: %d %q, want %q", c.q, r.status, names(r), c.want)
		}
	}
	tchExpect(t, "unknown status", get("?status=fired"), 400, handlers.TeacherStatusMessage)
	tchExpect(t, "id not a number", get("?id=x"), 400, handlers.TeacherIDMessage)
	tchExpect(t, "id 0", get("?id=0"), 400, handlers.TeacherIDMessage)
	tchExpect(t, "unknown id", get("?id=999999"), 404, handlers.TeacherNotFoundMessage)
	tchExpect(t, "no token", a14Do(t, srv, "GET", tchPath, nil, nil), 401, "")
}

// TestTeachersAttendance verifies GET /api/admin/teachers/attendance: the day type, first and last
// punch and count, not_punched only for an active teacher on a school day up to today, teachers of
// any status listed when they punched, and teachers added after the date left out.
func TestTeachersAttendance(t *testing.T) {
	srv, db, admin := l1Server(t, "tchattend", true) // FAKE_TODAY 2026-09-24, a Thursday
	a := tchCreate(t, srv, admin, tchBody("first_name", "أنس", "phone", "07701230041", "rfid_tag", "41"))
	b := tchCreate(t, srv, admin, tchBody("first_name", "بلال", "phone", "07701230042"))
	c := tchCreate(t, srv, admin, tchBody("first_name", "جمال", "phone", "07701230043", "status", "retired"))
	tchCreate(t, srv, admin, tchBody("first_name", "داود", "phone", "07701230044", "status", "retired"))
	e := tchCreate(t, srv, admin, tchBody("first_name", "عماد", "phone", "07701230045"))
	f := tchCreate(t, srv, admin, tchBody("first_name", "فراس", "phone", "07701230046", "status", "on_leave"))
	g := tchCreate(t, srv, admin, tchBody("first_name", "غسان", "phone", "07701230047"))
	k1Exec(t, db,
		`UPDATE teachers SET created_at = '2026-01-01 08:00+03'`,
		fmt.Sprintf(`UPDATE teachers SET created_at = '2026-09-25 08:00+03' WHERE id = %d`, e),
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('TCH-DEV', 'Gate', true)`,
		fmt.Sprintf(`INSERT INTO teacher_attendance_logs (teacher_id, device_sn, check_time) VALUES
			(%[1]d, 'TCH-DEV', '2026-09-24 07:12'), (%[2]d, 'TCH-DEV', '2026-09-24 09:40'),
			(%[3]d, 'TCH-DEV', '2026-09-24 07:05'), (%[3]d, 'TCH-DEV', '2026-09-24 10:00'), (%[3]d, 'TCH-DEV', '2026-09-24 13:05'),
			(%[1]d, 'TCH-DEV', '2026-09-23 07:00'), (%[1]d, 'TCH-DEV', '2026-09-25 07:00')`, a, c, g),
		`INSERT INTO school_closures (kind, start_date, end_date, title) VALUES ('holiday', '2026-09-23', '2026-09-23', 'عطلة')`)
	_ = f
	get := func(q string) map[string]any {
		t.Helper()
		r := a14Do(t, srv, "GET", tchPath+"/attendance"+q, l1Header(admin), nil)
		if r.status != 200 {
			t.Fatalf("GET %s: %d %s", q, r.status, r.body)
		}
		return r.json(t)
	}
	rowsOf := func(resp map[string]any) string {
		var out []string
		for _, it := range resp["data"].([]any) {
			m := it.(map[string]any)
			out = append(out, fmt.Sprintf("%s %s %v %v %v %v", strings.Fields(fmt.Sprint(m["full_name"]))[0], m["status"], m["first_punch"], m["last_punch"], m["punch_count"], m["not_punched"]))
		}
		return strings.Join(out, "\n")
	}
	expect := func(q string, day, school string, want ...string) {
		t.Helper()
		resp := get(q)
		if got := fmt.Sprint(resp["day_type"], " ", resp["school_day"]); got != day+" "+school {
			t.Errorf("%s: day %s, want %s %s", q, got, day, school)
		}
		if got := rowsOf(resp); got != strings.Join(want, "\n") {
			t.Errorf("%s rows:\n%s\nwant:\n%s", q, got, strings.Join(want, "\n"))
		}
	}
	today := []string{
		"غسان active 07:05 AM 01:05 PM 3 false",
		"أنس active 07:12 AM <nil> 1 false",
		"جمال retired 09:40 AM <nil> 1 false",
		"بلال active <nil> <nil> 0 true",
	}
	expect("", "school", "true", today...)
	expect("?date=2026-09-24", "school", "true", today...)
	if resp := get("?date=2026-09-24"); resp["date"] != "2026-09-24" || resp["day_title"] != nil {
		t.Errorf("date %v, day_title %v", resp["date"], resp["day_title"])
	}
	expect("?date=2026-09-23", "holiday", "false",
		"أنس active 07:00 AM <nil> 1 false", "بلال active <nil> <nil> 0 false", "غسان active <nil> <nil> 0 false")
	if resp := get("?date=2026-09-23"); resp["day_title"] != "عطلة" {
		t.Errorf("holiday day_title %v", resp["day_title"])
	}
	expect("?date=2026-09-25", "weekend", "false",
		"أنس active 07:00 AM <nil> 1 false", "بلال active <nil> <nil> 0 false", "عماد active <nil> <nil> 0 false", "غسان active <nil> <nil> 0 false")
	for _, future := range []string{"2026-09-27", "2026-12-01"} {
		expect("?date="+future, "school", "true",
			"أنس active <nil> <nil> 0 false", "بلال active <nil> <nil> 0 false", "عماد active <nil> <nil> 0 false", "غسان active <nil> <nil> 0 false")
	}
	expect("?date=2026-09-21", "school", "true",
		"أنس active <nil> <nil> 0 true", "بلال active <nil> <nil> 0 true", "غسان active <nil> <nil> 0 true")
	for _, bad := range []string{"?date=24-09-2026", "?date=1999-01-01"} {
		if r := a14Do(t, srv, "GET", tchPath+"/attendance"+bad, l1Header(admin), nil); r.status != 400 {
			t.Errorf("%s: %d", bad, r.status)
		}
	}
	_ = b
}

// TestTeachersSpecializations verifies the suggestion list: weekly schedule subjects and teacher
// specializations, trimmed, distinct, without empty values, sorted.
func TestTeachersSpecializations(t *testing.T) {
	srv, db, admin := l1Server(t, "tchspecs", true)
	k1Exec(t, db, `INSERT INTO weekly_schedules (grade, section, day_of_week, period_number, subject_name) VALUES
		('G1', 'A', 'الأحد', 1, 'الرياضيات'), ('G1', 'A', 'الأحد', 2, ' الرياضيات '), ('G1', 'A', 'الأحد', 3, 'العلوم'), ('G1', 'A', 'الأحد', 4, '  ')`)
	tchCreate(t, srv, admin, tchBody("phone", "07701230051", "specialization", "العلوم"))
	tchCreate(t, srv, admin, tchBody("phone", "07701230052", "specialization", "اللغة الإنجليزية"))
	tchCreate(t, srv, admin, tchBody("phone", "07701230053"))
	r := a14Do(t, srv, "GET", tchPath+"/specializations", l1Header(admin), nil)
	var got []string
	for _, v := range r.json(t)["data"].([]any) {
		got = append(got, fmt.Sprint(v))
	}
	want := []string{"الرياضيات", "العلوم", "اللغة الإنجليزية"}
	sort.Strings(want)
	sorted := append([]string{}, got...)
	sort.Strings(sorted)
	if r.status != 200 || strings.Join(sorted, "|") != strings.Join(want, "|") || len(got) != 3 {
		t.Errorf("specializations %d %q, want %q", r.status, got, want)
	}
}

// TestTeachersStudentTagRange verifies the student side of the disjoint ranges: a new or changed
// student tag must be 100 or more, an older tag under 100 still resends unchanged, and the 409
// suggestion never goes under 100.
func TestTeachersStudentTagRange(t *testing.T) {
	srv, db, admin := l1Server(t, "tchstudrange", true)
	student := func(tag string) a14Response {
		return a14Do(t, srv, "POST", "/api/admin/students", l1Header(admin), map[string]any{"name": "Range Kid", "parent_name": "Range Parent",
			"parent_phone": "+9647701230061", "parent_pin": p2Pin, "rfid_tag": tag})
	}
	for _, tag := range []string{"1", "50", "99"} {
		tchExpect(t, "student tag "+tag, student(tag), 400, handlers.RFIDTagStaffMessage)
	}
	tchExpect(t, "student tag 0099", student("0099"), 400, handlers.RFIDTagFormatMessage)
	tchExpect(t, "student tag 100", student("100"), 200, "Student created")

	k1Exec(t, db, `INSERT INTO students (id, full_name, rfid_tag) VALUES (9605, 'Legacy 5', '5'), (9699, 'Legacy 99', '99')`)
	put := func(tag string) a14Response {
		return a14Do(t, srv, "PUT", "/api/admin/students", l1Header(admin), map[string]any{"id": 9699, "name": "Legacy 99", "parent_name": "Range Parent",
			"parent_phone": "+9647701230061", "rfid_tag": tag})
	}
	tchExpect(t, "legacy tag resent unchanged", put("99"), 200, "Student updated")
	tchExpect(t, "legacy tag changed to another staff number", put("98"), 400, handlers.RFIDTagStaffMessage)
	tchExpect(t, "legacy tag moved to the student range", put("101"), 200, "Student updated")

	r := student("100")
	if r.status != 409 || fmt.Sprint(r.json(t)["suggested_rfid_tag"]) != "102" {
		t.Errorf("duplicate 100 with 5, 99, 100 and 101 taken: %d %s, want the suggestion 102", r.status, r.body)
	}
}

// tchApp is an in-process app with a fake FCM and a log capture, a device, a student with a parent
// phone, and teachers of three statuses.
type tchApp struct {
	app  *handlers.AppEnv
	db   *sql.DB
	fcm  *b1FakeFCM
	bg   *background.Group
	logs *w10Capture
}

func newTchApp(t *testing.T, label string) *tchApp {
	t.Helper()
	a := &tchApp{logs: &w10Capture{}, bg: &background.Group{}, fcm: newB1FakeFCM(t)}
	prev := slog.Default()
	slog.SetDefault(slog.New(a.logs))
	t.Cleanup(func() { slog.SetDefault(prev) })
	a.db, _ = setupThrowawayDB(t, label)
	k1Exec(t, a.db,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('TCH-DEV', 'Gate', true)`,
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (8601, 'TCH Parent', '+9647000008601', 'x')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id, grade, section, created_at) VALUES (8601, 'TCH Kid', '8601', 8601, 'G1', 'A', '2026-01-01 08:00')`,
		fmt.Sprintf(`INSERT INTO device_tokens (parent_id, token) VALUES (8601, '%s')`, b1Token("tch-parent")),
		`INSERT INTO teachers (id, first_name, father_name, grandfather_name, phone, gender, status, rfid_tag) VALUES
			(21, 'T', 'A', 'One', '+9647701230021', 'male', 'active', '21'),
			(22, 'T', 'B', 'Two', '+9647701230022', 'female', 'retired', '22'),
			(23, 'T', 'C', 'Three', '+9647701230023', 'male', 'on_leave', '23')`)
	a.app = &handlers.AppEnv{DB: a.db, FCMClient: a.fcm.client(t), Background: a.bg}
	return a
}

// punch sends one ATTLOG request and returns the response once background work has finished.
func (a *tchApp) punch(t *testing.T, lines ...string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	a.app.ADMSHandler(rec, httptest.NewRequest("POST", "/iclock/cdata?SN=TCH-DEV&table=ATTLOG", strings.NewReader(strings.Join(lines, "\t1\t1\n")+"\t1\t1\n")))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if pending, err := a.bg.Wait(ctx); err != nil {
		t.Fatalf("background work still running: %d", pending)
	}
	return rec
}

// teacherPunches lists the stored teacher punches as "teacher device time".
func (a *tchApp) teacherPunches(t *testing.T) []string {
	t.Helper()
	rows, err := a.db.Query(`SELECT teacher_id || ' ' || device_sn || ' ' || to_char(check_time, 'YYYY-MM-DD HH24:MI:SS') FROM teacher_attendance_logs ORDER BY teacher_id, check_time`)
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

// lastLog returns the newest captured record with msg, or nil.
func (a *tchApp) lastLog(msg string) *w10Log {
	logs := a.logs.snapshot()
	for i := len(logs) - 1; i >= 0; i-- {
		if logs[i].Msg == msg {
			return &logs[i]
		}
	}
	return nil
}

// TestTeachersADMS verifies the device path: a PIN that matches no student but a teacher (of any
// status) is stored in teacher_attendance_logs, idempotently, without a notification; a PIN that
// matches neither is skipped and logged at INFO; student punches are unchanged; the device gets the
// same bytes for every request.
func TestTeachersADMS(t *testing.T) {
	a := newTchApp(t, "tchadms")
	pinToday(t, k1Today+" 13:00")
	baseline := a.punch(t, "8601\t"+k1Today+" 07:20:00")
	expectOK := func(t *testing.T, what string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != 200 || rec.Body.String() != "OK" || fmt.Sprint(rec.Header()) != fmt.Sprint(baseline.Header()) {
			t.Errorf("%s: device response %d %v %q, want the student punch's 200 %v \"OK\"", what, rec.Code, rec.Header(), rec.Body, baseline.Header())
		}
	}
	if baseline.Code != 200 || baseline.Body.String() != "OK" || baseline.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("student punch response %d %v %q", baseline.Code, baseline.Header(), baseline.Body)
	}
	if n := k1Count(t, a.db, `SELECT COUNT(*) FROM notifications`); n != 1 {
		t.Fatalf("student punch notifications %d, want 1", n)
	}
	if sent := a.fcm.take(); len(sent) != 1 {
		t.Fatalf("student punch pushes %v", sent)
	}

	t.Run("a teacher punch is stored and never notified", func(t *testing.T) {
		expectOK(t, "teacher punch", a.punch(t, "21\t"+k1Today+" 07:10:00"))
		if got := a.teacherPunches(t); strings.Join(got, ",") != "21 TCH-DEV 2026-09-24 07:10:00" {
			t.Errorf("teacher punches %v", got)
		}
		if l := a.lastLog("ADMSHandler: batch stored"); l == nil || l.Attrs["teacher_inserted"] != "1" || l.Attrs["teacher_duplicates"] != "0" || l.Attrs["inserted"] != "0" {
			t.Errorf("batch summary %+v", l)
		}
		if n := k1Count(t, a.db, `SELECT COUNT(*) FROM notifications`); n != 1 || len(a.fcm.take()) != 0 {
			t.Errorf("a teacher punch notified someone")
		}
	})

	t.Run("a duplicate teacher punch is idempotent", func(t *testing.T) {
		expectOK(t, "duplicate", a.punch(t, "21\t"+k1Today+" 07:10:00"))
		if got := a.teacherPunches(t); len(got) != 1 {
			t.Errorf("teacher punches after a resend %v", got)
		}
		if l := a.lastLog("ADMSHandler: batch stored"); l == nil || l.Attrs["teacher_inserted"] != "0" || l.Attrs["teacher_duplicates"] != "1" {
			t.Errorf("batch summary %+v", l)
		}
	})

	t.Run("teachers of every status are stored", func(t *testing.T) {
		expectOK(t, "retired and on leave", a.punch(t, "22\t"+k1Today+" 07:11:00", "23\t"+k1Today+" 07:12:00"))
		if got := a.teacherPunches(t); len(got) != 3 {
			t.Errorf("teacher punches %v", got)
		}
	})

	t.Run("an unknown PIN is skipped and logged at INFO", func(t *testing.T) {
		before := len(a.teacherPunches(t))
		attendance := k1Count(t, a.db, `SELECT COUNT(*) FROM attendance_logs`)
		expectOK(t, "unknown PINs", a.punch(t, "2\t"+k1Today+" 07:13:00", "3\t"+k1Today+" 07:14:00"))
		if len(a.teacherPunches(t)) != before || k1Count(t, a.db, `SELECT COUNT(*) FROM attendance_logs`) != attendance {
			t.Errorf("an unknown PIN was stored")
		}
		l := a.lastLog("ADMSHandler: punches for unknown device PINs skipped")
		if l == nil || l.Level != slog.LevelInfo || l.Attrs["pins"] != "[2 3]" {
			t.Errorf("unknown-PIN log %+v, want INFO with pins [2 3]", l)
		}
		for _, r := range a.logs.snapshot() {
			if r.Level >= slog.LevelWarn && strings.Contains(r.Msg, "unknown device PINs") {
				t.Errorf("unknown PINs still logged at %v", r.Level)
			}
		}
	})

	t.Run("a mixed batch stores each kind once and notifies only the student", func(t *testing.T) {
		expectOK(t, "mixed", a.punch(t, "8601\t"+k1Today+" 12:05:00", "21\t"+k1Today+" 12:06:00", "2\t"+k1Today+" 12:07:00", "21\t2026-09-20 07:00:00"))
		if n := k1Count(t, a.db, `SELECT COUNT(*) FROM attendance_logs WHERE student_id = 8601`); n != 2 {
			t.Errorf("student punches %d, want 2", n)
		}
		if got := a.teacherPunches(t); len(got) != 5 {
			t.Errorf("teacher punches %v", got)
		}
		if n := k1Count(t, a.db, `SELECT COUNT(*) FROM notifications`); n != 2 || len(a.fcm.take()) != 1 {
			t.Errorf("notifications %d: want the student's check-out only", n)
		}
		if l := a.lastLog(h3Warn); l != nil {
			t.Errorf("an old teacher punch counted in the H3 warning: %+v", l)
		}
	})

	t.Run("teacher rows the database rejects are skipped; students still commit", func(t *testing.T) {
		k1Exec(t, a.db, `ALTER TABLE teacher_attendance_logs RENAME TO teacher_attendance_logs_off`)
		rec := a.punch(t, "8601\t2026-09-24 07:30:00", "21\t"+k1Today+" 07:31:00")
		k1Exec(t, a.db, `ALTER TABLE teacher_attendance_logs_off RENAME TO teacher_attendance_logs`)
		expectOK(t, "teacher rows rejected", rec)
		if n := k1Count(t, a.db, `SELECT COUNT(*) FROM attendance_logs WHERE student_id = 8601 AND check_time = '2026-09-24 07:30'`); n != 1 {
			t.Errorf("the student punch was not committed")
		}
		if l := a.lastLog("ADMSHandler: teacher punches rejected by the database — skipped"); l == nil || l.Level != slog.LevelError {
			t.Errorf("rejection not logged at ERROR: %+v", l)
		}
	})
}

// TestTeachersDeviceResponsesUnchanged replays the device scenarios against the reference binary
// (REFERENCE_API_BINARY, a build of the commit before the teachers module) and the current one, on
// identically seeded databases: device responses (status, body, headers but Date), stored student
// punches and notifications must be identical; only the current binary stores teacher punches.
func TestTeachersDeviceResponsesUnchanged(t *testing.T) {
	ref := os.Getenv("REFERENCE_API_BINARY")
	if ref == "" {
		t.Skip("set REFERENCE_API_BINARY to a build of the previous commit to run the differential test")
	}
	type side struct {
		srv *g3Server
		db  *sql.DB
	}
	start := func(label, binary string) side {
		db, dsn := setupThrowawayDB(t, label)
		k1Exec(t, db,
			`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1, 'PA', '+9647000008701', 'x'), (2, 'PB', '+9647000008702', 'x')`,
			`INSERT INTO students (id, full_name, rfid_tag, parent_id, grade, section, created_at) VALUES
				(1, 'Kid A', '8701', 1, 'G1', 'A', '2026-01-01 08:00'), (2, 'Kid B', '8702', 2, 'G1', 'A', '2026-01-01 08:00'),
				(3, 'Kid C', '8703', NULL, 'G1', 'B', '2026-01-01 08:00')`,
			`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('TCH-DEV', 'Gate', true), ('TCH-OFF', 'Old', false)`,
			`INSERT INTO teachers (id, first_name, father_name, grandfather_name, phone, gender, status, rfid_tag) VALUES
				(1, 'T', 'A', 'One', '+9647701230081', 'male', 'active', '21'), (2, 'T', 'B', 'Two', '+9647701230082', 'female', 'retired', '22')`)
		t.Setenv("G3_API_BINARY", binary)
		return side{g3Start(t, dsn, g3FreePort(t), true, "FAKE_TODAY="+k1Today, "ABSENCE_CRON_SCHEDULE=0 0 1 1 *"), db}
	}
	old := start("tchref", ref)
	cur := start("tchcur", "")

	lines := func(ls ...string) string { return strings.Join(ls, "\t1\t1\n") + "\t1\t1\n" }
	adms := "/iclock/cdata?SN=TCH-DEV&table=ATTLOG"
	xs := []h3Exchange{
		{"student batch", "POST", adms, lines("8701\t2026-09-24 07:15:00", "8701\t2026-09-24 07:18:00", "8702\t2026-09-24 07:03:00", "8703\t2026-09-24 07:26:00", "8701\t2026-09-24 12:05:00")},
		{"resend", "POST", adms, lines("8701\t2026-09-24 07:15:00")},
		{"teacher batch", "POST", adms, lines("21\t2026-09-24 07:10:00", "22\t2026-09-24 07:11:00")},
		{"teacher resend", "POST", adms, lines("21\t2026-09-24 07:10:00")},
		{"unknown PINs", "POST", adms, lines("2\t2026-09-24 07:12:00", "3\t2026-09-24 07:13:00")},
		{"mixed batch", "POST", adms, lines("8702\t2026-09-24 12:10:00", "21\t2026-09-24 12:11:00", "2\t2026-09-24 12:12:00", "21\t2026-09-20 07:00:00")},
		{"malformed lines", "POST", adms, "garbage\n21\tnot-a-date\t1\t1\n\n8703\t2026-09-24 12:20:00\t1\t1\n"},
		{"non-ATTLOG table", "POST", "/iclock/cdata?SN=TCH-DEV&table=OPERLOG", "OPLOG 4\t0\t2026-09-24 08:00:00\n"},
		{"unregistered device", "POST", "/iclock/cdata?SN=TCH-NOPE&table=ATTLOG", lines("21\t2026-09-24 07:32:00")},
		{"disabled device", "POST", "/iclock/cdata?SN=TCH-OFF&table=ATTLOG", lines("21\t2026-09-24 07:33:00")},
		{"missing SN", "POST", "/iclock/cdata?table=ATTLOG", lines("21\t2026-09-24 07:34:00")},
		{"empty batch", "POST", adms, ""},
		{"handshake", "GET", "/iclock/cdata?SN=TCH-DEV&options=all", nil},
		{"command poll", "GET", "/iclock/getrequest?SN=TCH-DEV", nil},
		{"oversized body", "POST", adms, strings.Repeat("x", int(handlers.MaxADMSBodyBytes)+1)},
		{"ADMS alias with a teacher", "POST", "/api/attendance/push?SN=TCH-DEV&table=ATTLOG", lines("22\t2026-09-24 12:30:00", "8701\t2026-09-24 12:31:00")},
		{"JSON push with a teacher tag", "POST", "/api/attendance/push/json?SN=TCH-DEV", map[string]string{"device_sn": "TCH-DEV", "rfid_tag": "21", "push_time": "2026-09-24 12:40:00"}},
		{"JSON push with a student tag", "POST", "/api/attendance/push/json?SN=TCH-DEV", map[string]string{"device_sn": "TCH-DEV", "rfid_tag": "8702", "push_time": "2026-09-24 12:41:00"}},
	}
	for _, x := range xs {
		ra, rb := a14Do(t, old.srv, x.method, x.path, nil, x.body), a14Do(t, cur.srv, x.method, x.path, nil, x.body)
		ha, hb := ra.header.Clone(), rb.header.Clone()
		ha.Del("Date")
		hb.Del("Date")
		if ra.status != rb.status || string(ra.body) != string(rb.body) || fmt.Sprint(ha) != fmt.Sprint(hb) {
			t.Errorf("%s: reference %d %v %q, current %d %v %q", x.name, ra.status, ha, ra.body, rb.status, hb, rb.body)
		}
	}
	dump := func(db *sql.DB, q string) []string {
		rows, err := db.Query(q)
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
		sort.Strings(out)
		return out
	}
	settled := func(db *sql.DB) []string {
		var last []string
		for deadline, stable := time.Now().Add(5*time.Second), time.Now(); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			n := dump(db, `SELECT concat_ws('|', parent_id, title, body) FROM notifications`)
			if strings.Join(n, "\n") != strings.Join(last, "\n") {
				last, stable = n, time.Now()
			} else if time.Since(stable) > 500*time.Millisecond {
				break
			}
		}
		return last
	}
	const punchesSQL = `SELECT concat_ws('|', student_id, COALESCE(device_sn, '-'), check_time, source) FROM attendance_logs`
	if a, b := dump(old.db, punchesSQL), dump(cur.db, punchesSQL); strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Errorf("student punches differ:\nreference %q\ncurrent   %q", a, b)
	}
	if a, b := settled(old.db), settled(cur.db); strings.Join(a, "\n") != strings.Join(b, "\n") || len(b) < 3 {
		t.Errorf("notifications differ (or too few):\nreference %q\ncurrent   %q", a, b)
	}
	const teacherSQL = `SELECT concat_ws('|', teacher_id, device_sn, check_time) FROM teacher_attendance_logs`
	refTeachers, curTeachers := dump(old.db, teacherSQL), dump(cur.db, teacherSQL)
	wantTeachers := []string{"1|TCH-DEV|2026-09-20 07:00:00", "1|TCH-DEV|2026-09-24 07:10:00", "1|TCH-DEV|2026-09-24 12:11:00", "2|TCH-DEV|2026-09-24 07:11:00", "2|TCH-DEV|2026-09-24 12:30:00"}
	if len(refTeachers) != 0 || strings.Join(curTeachers, ",") != strings.Join(wantTeachers, ",") {
		t.Errorf("teacher punches: reference %q, current %q; want none and %q", refTeachers, curTeachers, wantTeachers)
	}
	t.Logf("%d device requests byte-identical; student punches and notifications identical; current binary stored %d teacher punches", len(xs), len(curTeachers))
}

// TestTeachersMigration verifies that 000034 goes down and up again cleanly. Later migrations
// that reference teachers (000036, expenses.teacher_id) go down first and up after, as migrate does.
func TestTeachersMigration(t *testing.T) {
	db, _ := setupThrowawayDB(t, "tchmigr")
	up, down := a14Migration(t, "000034_create_teachers.up.sql"), a14Migration(t, "000034_create_teachers.down.sql")
	k1Exec(t, db, a14Migration(t, "000036_create_expenses.down.sql"))
	defer k1Exec(t, db, a14Migration(t, "000036_create_expenses.up.sql"))
	tables := func() int {
		return k1Count(t, db, `SELECT COUNT(*) FROM information_schema.tables WHERE table_name IN ('teachers', 'teacher_attendance_logs')`)
	}
	if tables() != 2 {
		t.Fatalf("tables missing after up")
	}
	k1Exec(t, db, down)
	if tables() != 0 {
		t.Errorf("tables left after down")
	}
	k1Exec(t, db, up, up)
	if tables() != 2 {
		t.Errorf("tables missing after up again (twice)")
	}
}
