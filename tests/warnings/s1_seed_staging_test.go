package warnings

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// s1Build builds cmd/seed-staging into the test's temporary directory.
func s1Build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "seed-staging")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/seed-staging")
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build cmd/seed-staging: %v\n%s", err, out)
	}
	return bin
}

// s1Seed runs the seed tool with only the given environment and returns its output and exit code.
func s1Seed(t *testing.T, bin string, env map[string]string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	}
	t.Fatalf("run seed-staging: %v", err)
	return "", -1
}

// s1State lists everything the seed tool could change.
func s1State(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id, COALESCE(rfid_tag, ''), full_name, COALESCE(grade, '-'), COALESCE(section, '-'), COALESCE(parent_id, 0), is_active FROM students ORDER BY id`,
		`SELECT id, phone_number, full_name, pin_code, session_version FROM parents ORDER BY id`,
		`SELECT student_id, device_sn, check_time::text FROM attendance_logs ORDER BY student_id, check_time`,
		`SELECT id, parent_id, title, body FROM notifications ORDER BY id`,
		`SELECT id, grade, section, day_of_week, period_number, subject_name, COALESCE(teacher_name, '-') FROM weekly_schedules ORDER BY id`,
		`SELECT serial_number, COALESCE(location_name, '-'), COALESCE(is_active, false), COALESCE(last_sync::text, '-') FROM devices ORDER BY serial_number`,
		`SELECT id, student_id, leave_date::text, COALESCE(notes, '') FROM student_leaves ORDER BY id`,
		`SELECT b.id, COALESCE(b.title, '-'), COALESCE(b.action_link, '-'), COALESCE(b.is_active, false), COALESCE(bi.checksum, '-') FROM banners b LEFT JOIN banner_images bi ON bi.banner_id = b.id ORDER BY b.id`,
		`SELECT setting_key, setting_value FROM settings ORDER BY setting_key`,
		`SELECT username, session_version FROM admins ORDER BY username`,
		`SELECT COUNT(*) FROM device_tokens`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			fmt.Fprintln(&b, vals...)
		}
		rows.Close()
		b.WriteString("--\n")
	}
	return b.String()
}

func s1Count(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
	return n
}

// s1Notifications waits until the background notification writes have settled.
func s1Notifications(t *testing.T, db *sql.DB) int {
	t.Helper()
	last, stable := -1, 0
	for i := 0; i < 100 && stable < 5; i++ {
		n := s1Count(t, db, `SELECT COUNT(*) FROM notifications`)
		if n == last {
			stable++
		} else {
			last, stable = n, 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	return last
}

var s1TableLine = regexp.MustCompile(`(?m)^(\+96470000020[0-9]{2}) \| ([^|]+) \| (.+)$`)

// TestS1SeedStaging verifies Task S1: the seed tool refuses every URL but Staging (and
// localhost with --allow-local), changes nothing in a dry run, seeds students, parents,
// schedules, the fake device, leaves, attendance history and banners through the real API,
// creates nothing on a second run, and never touches the existing students or the real device.
func TestS1SeedStaging(t *testing.T) {
	bin := s1Build(t)
	srv, db := a14Server(t, "s1")
	admin := a14Bearer(a14AdminToken(t, srv))
	base := fmt.Sprintf("http://localhost:%d", srv.port)
	env := map[string]string{"BASE_URL": base, "ADMIN_USERNAME": "admin", "ADMIN_PASSWORD": a14AdminPassword}
	var outputs []string
	seed := func(t *testing.T, env map[string]string, args ...string) (string, int) {
		t.Helper()
		out, code := s1Seed(t, bin, env, args...)
		outputs = append(outputs, out)
		return out, code
	}

	for i, st := range []struct{ grade, section string }{{"G2", "A"}, {"G2", "A"}, {"G1", "B"}} {
		body := map[string]any{"name": fmt.Sprintf("طالب حالي تجريبي %d", i+1), "parent_name": "ولي أمر حالي تجريبي", "parent_phone": "+9647000001401", "rfid_tag": strconv.Itoa(1001 + i), "grade": st.grade, "section": st.section}
		if i == 0 {
			body["parent_pin"] = "Nc6?Fa3w@Ub8rZ5k"
		}
		if r := a14Do(t, srv, "POST", "/api/admin/students", admin, body); r.status != 200 {
			t.Fatalf("pre-existing student %d: %d %s", 1001+i, r.status, r.body)
		}
	}
	if r := a14Do(t, srv, "POST", "/api/admin/devices", admin, map[string]any{"serial_number": "QJT0000000001", "location_name": "Main gate", "is_active": true}); r.status != 200 {
		t.Fatalf("device: %d %s", r.status, r.body)
	}
	if r := a14Do(t, srv, "PUT", "/api/admin/schedule", admin, map[string]any{"grade": "G2", "section": "A", "periods": []map[string]any{
		{"day_of_week": "الأحد", "period_number": 1, "subject_name": "اختبار"}, {"day_of_week": "الأحد", "period_number": 2, "subject_name": "اختبار"},
		{"day_of_week": "الأحد", "period_number": 3, "subject_name": "اختبار"}, {"day_of_week": "الأحد", "period_number": 4, "subject_name": "اختبار"},
		{"day_of_week": "الأحد", "period_number": 5, "subject_name": "اختبار"}}}); r.status != 200 {
		t.Fatalf("G2/A schedule: %d %s", r.status, r.body)
	}
	existingRows := func() string {
		rows, _ := db.Query(`SELECT s.id, s.rfid_tag, s.full_name, s.grade, s.section, s.is_active, p.phone_number, p.pin_code, p.session_version FROM students s JOIN parents p ON p.id = s.parent_id WHERE s.rfid_tag IN ('1001', '1002', '1003') ORDER BY s.id`)
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var id, sv int
			var tag, name, grade, section, phone, pin string
			var active bool
			rows.Scan(&id, &tag, &name, &grade, &section, &active, &phone, &pin, &sv)
			fmt.Fprintln(&b, id, tag, name, grade, section, active, phone, pin, sv)
		}
		return b.String()
	}
	existingBefore := existingRows()
	baghdadNow := time.Now().In(time.FixedZone("Asia/Baghdad", 3*60*60))
	today := baghdadNow.Format("2006-01-02")
	firstDay := time.Date(baghdadNow.Year(), baghdadNow.Month()-1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	previousMonth := firstDay[:7]

	t.Run("unsafe configurations are refused before anything is sent", func(t *testing.T) {
		before := s1State(t, db)
		with := func(key, value string) map[string]string {
			m := map[string]string{}
			for k, v := range env {
				m[k] = v
			}
			if value == "" {
				delete(m, key)
			} else {
				m[key] = value
			}
			return m
		}
		for _, tc := range []struct {
			name string
			env  map[string]string
			args []string
			want string
		}{
			{"another host", with("BASE_URL", "https://example.com"), []string{"--allow-local"}, "BASE_URL must be exactly https://futurekids-staging.up.railway.app"},
			{"a production-looking URL", with("BASE_URL", "https://futurekids-production.up.railway.app"), []string{"--allow-local", "--apply"}, "BASE_URL must be exactly"},
			{"a look-alike of Staging", with("BASE_URL", "https://futurekids-staging.up.railway.app.example.com"), []string{"--apply"}, "BASE_URL must be exactly"},
			{"Staging over plain http", with("BASE_URL", "http://futurekids-staging.up.railway.app"), nil, "BASE_URL must be exactly"},
			{"localhost without --allow-local", env, []string{"--apply"}, "a local BASE_URL needs --allow-local"},
			{"127.0.0.1 instead of localhost", with("BASE_URL", strings.Replace(base, "localhost", "127.0.0.1", 1)), []string{"--allow-local"}, "BASE_URL must be exactly"},
			{"no BASE_URL", with("BASE_URL", ""), []string{"--allow-local"}, "BASE_URL must be exactly"},
			{"no password", with("ADMIN_PASSWORD", ""), []string{"--allow-local"}, "ADMIN_PASSWORD not set"},
			{"no username", with("ADMIN_USERNAME", ""), []string{"--allow-local"}, "ADMIN_USERNAME not set"},
			{"--resume-history without --apply", env, []string{"--allow-local", "--resume-history"}, "--resume-history needs --apply"},
			{"a password flag", env, []string{"--allow-local", "--password=" + a14AdminPassword}, "flag provided but not defined"},
		} {
			out, code := seed(t, tc.env, tc.args...)
			if code != 2 || !strings.Contains(out, tc.want) {
				t.Errorf("%s: exit %d, output %q; want exit 2 and %q", tc.name, code, out, tc.want)
			}
		}
		out, code := seed(t, with("ADMIN_PASSWORD", "s1-wrong-password"), "--allow-local")
		if code != 1 || !strings.Contains(out, "STOPPED during admin login") || strings.Contains(out, "s1-wrong-password") {
			t.Errorf("wrong password: exit %d, output %q", code, out)
		}
		if after := s1State(t, db); after != before {
			t.Errorf("refused runs changed the database")
		}
	})

	t.Run("a dry run changes nothing", func(t *testing.T) {
		before := s1State(t, db)
		out, code := seed(t, env, "--allow-local")
		if code != 0 {
			t.Fatalf("dry run: exit %d\n%s", code, out)
		}
		for _, want := range []string{"DRY RUN: nothing will be changed", "47 will be created", "REPLACES the existing schedule of G2/A (5 periods)", "G6/B keeps no schedule", "Device SEED-FAKE-0001: will be registered", "Leaves: 8 will be recorded", "Banners: 3 will be uploaded", "DRY RUN: nothing was changed"} {
			if !strings.Contains(out, want) {
				t.Errorf("dry-run output lacks %q\n%s", want, out)
			}
		}
		if after := s1State(t, db); after != before {
			t.Errorf("the dry run changed the database")
		}
	})

	var applyOut string
	t.Run("--apply seeds everything through the API", func(t *testing.T) {
		out, code := seed(t, env, "--allow-local", "--apply")
		applyOut = out
		t.Logf("seed-staging --apply output:\n%s", out)
		if code != 1 {
			t.Fatalf("apply: exit %d, want 1 (the tool's final check of yesterday's report fails since H1: students created today are not listed for yesterday)\n%s", code, out)
		}
		for _, want := range []string{"Students (rfid_tag 1004 to 1050): 47 created", "Parents (+9647000002001 to +9647000002037): 37 new, 0 already registered", "Weekly schedules: 11 classes set", "Device SEED-FAKE-0001: registered.", "Leaves: 8 recorded", "Banners: 3 uploaded", "STOPPED during verification", "in the daily report, want"} {
			if !strings.Contains(out, want) {
				t.Errorf("apply output lacks %q\n%s", want, out)
			}
		}
		if n := s1Count(t, db, `SELECT COUNT(*) FROM students WHERE is_active`); n != 50 {
			t.Errorf("%d active students, want 50", n)
		}
		rows, _ := db.Query(`SELECT s.rfid_tag::int, s.full_name, s.grade, s.section, p.phone_number, p.full_name, p.pin_code FROM students s JOIN parents p ON p.id = s.parent_id WHERE s.rfid_tag::int BETWEEN 1004 AND 1050 ORDER BY 1`)
		perParent, grades := map[string]int{}, map[string]map[string]bool{}
		longName, longParent, tags := 0, 0, 0
		pins := map[string]string{}
		classes := map[string]int{}
		for rows.Next() {
			var tag int
			var name, grade, section, phone, parentName, pin string
			rows.Scan(&tag, &name, &grade, &section, &phone, &parentName, &pin)
			tags++
			if !strings.Contains(name, "تجريبي") || !strings.Contains(parentName, "تجريبي") {
				t.Errorf("rfid_tag %d: names %q / %q must contain تجريبي", tag, name, parentName)
			}
			if !regexp.MustCompile(`^\+96470000020(0[1-9]|[1-2][0-9]|3[0-7])$`).MatchString(phone) {
				t.Errorf("rfid_tag %d: parent phone %s is outside the fake block", tag, phone)
			}
			if n := utf8.RuneCountInString(name); n > longName {
				longName = n
			}
			if n := utf8.RuneCountInString(parentName); n > longParent {
				longParent = n
			}
			perParent[phone]++
			if grades[phone] == nil {
				grades[phone] = map[string]bool{}
			}
			if grades[phone][grade] {
				t.Errorf("parent %s has two children in %s", phone, grade)
			}
			grades[phone][grade] = true
			pins[phone] = pin
			classes[grade+"/"+section]++
		}
		rows.Close()
		if tags != 47 {
			t.Errorf("%d students with rfid_tag 1004 to 1050, want 47", tags)
		}
		if longName < 85 || longName > 100 || longParent < 240 || longParent > 255 {
			t.Errorf("longest student name %d characters (want about 90), longest parent name %d (want near 255)", longName, longParent)
		}
		sizes := map[int]int{}
		for _, n := range perParent {
			sizes[n]++
		}
		if len(perParent) != 37 || sizes[1] != 28 || sizes[2] != 8 || sizes[3] != 1 {
			t.Errorf("parents %d with children counts %v, want 37: 28×1, 8×2, 1×3", len(perParent), sizes)
		}
		for phone, hash := range pins {
			if bcrypt.CompareHashAndPassword([]byte(hash), []byte("Sd7!Kx4p#Wm9qT2h")) != nil {
				t.Errorf("parent %s does not have the seed credential", phone)
			}
		}
		if len(classes) != 12 || classes["G6/B"] == 0 {
			t.Errorf("classes %v, want all 12 of G1 to G6 × A, B", classes)
		}
		var scheduled []string
		srows, _ := db.Query(`SELECT grade || '/' || section, COUNT(*), COUNT(*) FILTER (WHERE teacher_name IS NULL) FROM weekly_schedules GROUP BY 1 ORDER BY 1`)
		for srows.Next() {
			var c string
			var n, noTeacher int
			srows.Scan(&c, &n, &noTeacher)
			scheduled = append(scheduled, c)
			if n < 25 || n > 30 || noTeacher == 0 {
				t.Errorf("schedule %s: %d periods, %d without teacher; want 25 to 30 with some without a teacher", c, n, noTeacher)
			}
		}
		srows.Close()
		if len(scheduled) != 11 || strings.Contains(strings.Join(scheduled, ","), "G6/B") {
			t.Errorf("classes with a schedule %v, want the 11 seeded classes except G6/B", scheduled)
		}
		if n := s1Count(t, db, `SELECT COUNT(*) FROM devices WHERE serial_number = 'SEED-FAKE-0001' AND is_active AND location_name = 'Seed (fake)'`); n != 1 {
			t.Errorf("fake device not registered as active Seed (fake)")
		}
		if n := s1Count(t, db, `SELECT COUNT(*) FROM student_leaves l JOIN students s ON s.id = l.student_id WHERE s.rfid_tag::int BETWEEN 1004 AND 1050 AND l.notes LIKE 'إجازة تجريبية%'`); n != 8 {
			t.Errorf("%d seeded leaves, want 8", n)
		}
		if got := s1Count(t, db, `SELECT COUNT(*) FROM banners`); got != 3 {
			t.Errorf("%d banners, want 3", got)
		}
		if n := s1Count(t, db, `SELECT COUNT(*) FROM banners b JOIN banner_images i ON i.banner_id = b.id WHERE
			(b.title IS NOT NULL AND b.action_link = 'https://example.com/seed' AND b.is_active AND i.content_type = 'image/png')
			OR (b.title IS NOT NULL AND b.action_link IS NULL AND NOT b.is_active AND i.content_type = 'image/jpeg')
			OR (b.title IS NULL AND b.is_active AND i.content_type = 'image/jpeg')`); n != 3 {
			t.Errorf("%d banners match the three planned ones", n)
		}

		if after := existingRows(); after != existingBefore {
			t.Errorf("students 1001 to 1003 or their parent changed\nbefore:\n%s\nafter:\n%s", existingBefore, after)
		}
		for _, rule := range []struct {
			q    string
			args []any
		}{
			{`SELECT COUNT(*) FROM attendance_logs a JOIN students s ON s.id = a.student_id WHERE s.rfid_tag IN ('1001', '1002', '1003')`, nil},
			{`SELECT COUNT(*) FROM attendance_logs WHERE device_sn <> 'SEED-FAKE-0001'`, nil},
			{`SELECT COUNT(*) FROM attendance_logs WHERE check_time::date >= $1::date`, []any{today}},
			{`SELECT COUNT(*) FROM attendance_logs WHERE check_time::date < $1::date`, []any{firstDay}},
			{`SELECT COUNT(*) FROM attendance_logs WHERE EXTRACT(DOW FROM check_time) IN (5, 6)`, nil},
			{`SELECT COUNT(*) FROM attendance_logs a JOIN student_leaves l ON l.student_id = a.student_id AND l.leave_date = a.check_time::date`, nil},
			{`SELECT COUNT(*) FROM attendance_logs WHERE NOT (check_time::time BETWEEN '06:40' AND '09:25:59' OR check_time::time BETWEEN '09:35' AND '11:20:59'
				OR check_time::time BETWEEN '11:35' AND '13:25:59' OR check_time::time BETWEEN '13:40' AND '15:30:59')`, nil},
		} {
			if n := s1Count(t, db, rule.q, rule.args...); n != 0 {
				t.Errorf("%d punches break the rule:\n%s", n, rule.q)
			}
		}
		if n := s1Count(t, db, `SELECT COUNT(*) FROM attendance_logs`); n < 500 {
			t.Errorf("only %d punches stored", n)
		}
		for _, check := range []string{
			`SELECT COUNT(*) FROM (SELECT student_id, check_time::date FROM attendance_logs WHERE check_time::time < '09:30' GROUP BY 1, 2 HAVING COUNT(*) > 1) d`,
			`SELECT COUNT(*) FROM attendance_logs WHERE check_time::time BETWEEN '09:35' AND '11:20:59' OR check_time::time >= '13:40'`,
			`SELECT COUNT(*) FROM (SELECT student_id, check_time::date FROM attendance_logs GROUP BY 1, 2 HAVING COUNT(*) FILTER (WHERE check_time::time >= '11:30') = 0) d`,
		} {
			if s1Count(t, db, check) == 0 {
				t.Errorf("the history has no case of:\n%s", check)
			}
		}

		m := regexp.MustCompile(`Expected parent notifications from it: (\d+) check-in \+ (\d+) check-out = (\d+)`).FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("no notification estimate in the output")
		}
		want, _ := strconv.Atoi(m[3])
		if got := s1Notifications(t, db); got != want {
			t.Errorf("%d notifications stored, the tool expected %s", got, m[3])
		}
	})

	t.Run("a second --apply creates and sends nothing", func(t *testing.T) {
		s1Notifications(t, db)
		before := s1State(t, db)
		out, code := seed(t, env, "--allow-local", "--apply")
		if code != 0 {
			t.Fatalf("second apply: exit %d\n%s", code, out)
		}
		applyOut = out
		for _, want := range []string{"0 created, 47 already exist", "0 classes set, 11 already identical", "Device SEED-FAKE-0001: already registered.", "Leaves: 0 recorded", "Attendance history: nothing to send.", "Banners: 0 uploaded, 3 already exist"} {
			if !strings.Contains(out, want) {
				t.Errorf("second apply output lacks %q\n%s", want, out)
			}
		}
		time.Sleep(300 * time.Millisecond)
		if after := s1State(t, db); after != before {
			t.Errorf("the second --apply changed the database")
		}
	})

	t.Run("a seeded parent logs in with the printed phone and PIN", func(t *testing.T) {
		lines := s1TableLine.FindAllStringSubmatch(applyOut, -1)
		if len(lines) != 37 {
			t.Fatalf("%d parent lines in the table, want 37", len(lines))
		}
		checked := 0
		for _, l := range lines {
			phone, pin, kids := l[1], strings.TrimSpace(l[2]), strings.Split(l[3], " ; ")
			if pin != "Sd7!Kx4p#Wm9qT2h" {
				t.Errorf("%s: PIN column %q", phone, pin)
			}
			if len(kids) < 2 && !strings.Contains(l[3], "G6/B") && phone != "+9647000002002" {
				continue
			}
			checked++
			login := a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": phone, "pin": pin})
			if login.status != 200 {
				t.Fatalf("login %s: %d %s", phone, login.status, login.body)
			}
			parent := a14Bearer(fmt.Sprint(login.json(t)["data"].(map[string]any)["token"]))
			want := map[string]string{}
			for _, k := range kids {
				name, class, _ := strings.Cut(k, " — ")
				want[name] = class
			}
			r := a14Do(t, srv, "GET", "/api/mobile/students", parent, nil)
			var students struct {
				Data []struct {
					ID       int    `json:"id"`
					FullName string `json:"full_name"`
					Grade    string `json:"grade"`
					Section  string `json:"section"`
				} `json:"data"`
			}
			json.Unmarshal(r.body, &students)
			var got []string
			for _, s := range students.Data {
				got = append(got, s.FullName+" — "+s.Grade+"/"+s.Section)
			}
			sort.Strings(got)
			sort.Strings(kids)
			if strings.Join(got, " ; ") != strings.Join(kids, " ; ") {
				t.Errorf("%s children\n got: %v\nwant: %v", phone, got, kids)
			}
			r = a14Do(t, srv, "GET", "/api/mobile/attendance/monthly?month="+previousMonth, parent, nil)
			var monthly struct {
				Data []struct {
					FullName string `json:"full_name"`
					Records  []struct {
						Status string `json:"status"`
					} `json:"records"`
				} `json:"data"`
			}
			json.Unmarshal(r.body, &monthly)
			if len(monthly.Data) != 0 {
				t.Errorf("%s monthly %s: %d children; since H1 the history from before the students were created is not shown, want none", phone, previousMonth, len(monthly.Data))
			}
			r = a14Do(t, srv, "GET", "/api/mobile/schedule", parent, nil)
			var schedule struct {
				Data []struct {
					StudentName string `json:"student_name"`
				} `json:"data"`
			}
			json.Unmarshal(r.body, &schedule)
			perChild := map[string]int{}
			for _, e := range schedule.Data {
				perChild[e.StudentName]++
			}
			for name, class := range want {
				if class == "G6/B" && perChild[name] != 0 {
					t.Errorf("%s in G6/B has %d schedule entries, want none", name, perChild[name])
				}
				if class != "G6/B" && perChild[name] < 25 {
					t.Errorf("%s in %s has %d schedule entries", name, class, perChild[name])
				}
			}
		}
		if checked < 10 {
			t.Errorf("only %d parents checked", checked)
		}
	})

	t.Run("the password and tokens never appear in the output", func(t *testing.T) {
		for _, out := range outputs {
			if strings.Contains(out, a14AdminPassword) || strings.Contains(out, "eyJ") {
				t.Errorf("output leaks the password or a token:\n%s", out)
			}
		}
	})
}

// TestS1SeedStopsSafely verifies that the seed tool stops before writing anything when its fake
// device exists but is disabled, and says how to continue.
func TestS1SeedStopsSafely(t *testing.T) {
	bin := s1Build(t)
	srv, db := a14Server(t, "s1stop")
	admin := a14Bearer(a14AdminToken(t, srv))
	if r := a14Do(t, srv, "POST", "/api/admin/devices", admin, map[string]any{"serial_number": "SEED-FAKE-0001", "location_name": "Seed (fake)", "is_active": false}); r.status != 200 {
		t.Fatalf("device: %d %s", r.status, r.body)
	}
	before := s1State(t, db)
	out, code := s1Seed(t, bin, map[string]string{"BASE_URL": fmt.Sprintf("http://localhost:%d", srv.port), "ADMIN_USERNAME": "admin", "ADMIN_PASSWORD": a14AdminPassword}, "--allow-local", "--apply")
	if code != 1 || !strings.Contains(out, "STOPPED during the fake device") || !strings.Contains(out, "Re-running with --apply is safe") || !strings.Contains(out, "failed:    1") {
		t.Errorf("exit %d, output:\n%s", code, out)
	}
	if after := s1State(t, db); after != before {
		t.Errorf("the stopped run changed the database")
	}
}
