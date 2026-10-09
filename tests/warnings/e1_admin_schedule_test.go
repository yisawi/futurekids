package warnings

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type e1Period struct {
	Day     string  `json:"day_of_week"`
	Period  int     `json:"period_number"`
	Subject string  `json:"subject_name"`
	Teacher *string `json:"teacher_name"`
}

func (p e1Period) String() string {
	teacher := "<null>"
	if p.Teacher != nil {
		teacher = *p.Teacher
	}
	return fmt.Sprintf("%s|%d|%s|%s", p.Day, p.Period, p.Subject, teacher)
}

type e1Schedule struct {
	Status  string     `json:"status"`
	Grade   string     `json:"grade"`
	Section string     `json:"section"`
	Data    []e1Period `json:"data"`
}

func e1P(day string, period int, subject string, teacher ...string) e1Period {
	p := e1Period{Day: day, Period: period, Subject: subject}
	if len(teacher) > 0 {
		p.Teacher = &teacher[0]
	}
	return p
}

func e1Body(grade, section string, periods ...e1Period) map[string]any {
	if periods == nil {
		periods = []e1Period{}
	}
	return map[string]any{"grade": grade, "section": section, "periods": periods}
}

func e1List(ps []e1Period) string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return strings.Join(out, "\n")
}

func e1Decode(t *testing.T, r a14Response) e1Schedule {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("got %d %s, want 200", r.status, r.body)
	}
	var s e1Schedule
	if err := json.Unmarshal(r.body, &s); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	if s.Status != "success" || s.Data == nil {
		t.Fatalf("body %s: want status success and a data array", r.body)
	}
	return s
}

func e1Error(t *testing.T, r a14Response, status int, message string) {
	t.Helper()
	if r.status != status {
		t.Errorf("got %d %s, want %d %q", r.status, r.body, status, message)
		return
	}
	if m := r.json(t); m["status"] != "error" || m["message"] != message {
		t.Errorf("body %s, want {status: error, message: %q}", r.body, message)
	}
}

// e1Snapshot lists the weekly_schedules rows matching where, in id order.
func e1Snapshot(t *testing.T, db *sql.DB, where string, args ...any) string {
	t.Helper()
	rows, err := db.Query(`SELECT id, grade, section, day_of_week, period_number, subject_name, COALESCE(teacher_name, '<NULL>')
		FROM weekly_schedules WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, period int
		var grade, section, day, subject, teacher string
		if err := rows.Scan(&id, &grade, &section, &day, &period, &subject, &teacher); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%d|%s|%s|%s|%d|%s|%s\n", id, grade, section, day, period, subject, teacher)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// e1Send is a14Do for goroutines: it reports errors instead of failing the test.
func e1Send(srv *g3Server, method, path, token string, body any) (int, []byte, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(method, srv.url(path), bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func e1Sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("g3"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestE1AdminSchedule verifies Task E1: GET and PUT /api/admin/schedule read and replace one
// class's weekly schedule, store canonical Arabic day names, reject invalid input without
// touching the stored schedule, serialize concurrent saves, and feed GET /api/mobile/schedule.
// Student grade and section are trimmed on save so they match the schedule.
func TestE1AdminSchedule(t *testing.T) {
	srv, db := a14Server(t, "e1")
	admin := a14AdminToken(t, srv)
	saves := 0
	put := func(body any) a14Response {
		t.Helper()
		r := a14Do(t, srv, "PUT", "/api/admin/schedule", a14Bearer(admin), body)
		if r.status == http.StatusOK {
			saves++
		}
		return r
	}
	get := func(grade, section string) a14Response {
		t.Helper()
		q := url.Values{"grade": {grade}, "section": {section}}
		return a14Do(t, srv, "GET", "/api/admin/schedule?"+q.Encode(), a14Bearer(admin), nil)
	}
	stored := func(grade, section string) []e1Period {
		t.Helper()
		s := e1Decode(t, get(grade, section))
		if s.Grade != strings.TrimSpace(grade) || s.Section != strings.TrimSpace(section) {
			t.Errorf("GET echoed class %q/%q, want the trimmed %q/%q", s.Grade, s.Section, grade, section)
		}
		return s.Data
	}

	t.Run("save then read back in school-week order", func(t *testing.T) {
		r := put(e1Body(" G3 ", " A ",
			e1P("الخميس", 2, "Art"),
			e1P("Sunday", 2, "Reading"),
			e1P("الاثنين", 1, "Science", "Teacher Example"),
			e1P("الأحد", 1, "Mathematics", "Teacher Example"),
			e1P("الأربعاء", 1, "History"),
			e1P("Tuesday", 3, "Music"),
			e1P("Tuesday", 1, "Geography"),
			e1P("thursday", 1, "Drawing"),
		))
		want := e1List([]e1Period{
			e1P("الأحد", 1, "Mathematics", "Teacher Example"),
			e1P("الأحد", 2, "Reading"),
			e1P("الإثنين", 1, "Science", "Teacher Example"),
			e1P("الثلاثاء", 1, "Geography"),
			e1P("الثلاثاء", 3, "Music"),
			e1P("الأربعاء", 1, "History"),
			e1P("الخميس", 1, "Drawing"),
			e1P("الخميس", 2, "Art"),
		})
		saved := e1Decode(t, r)
		if saved.Grade != "G3" || saved.Section != "A" {
			t.Errorf("PUT echoed class %q/%q, want the trimmed G3/A", saved.Grade, saved.Section)
		}
		if got := e1List(saved.Data); got != want {
			t.Errorf("PUT response\n got:\n%s\nwant:\n%s", got, want)
		}
		if got := e1List(stored("G3", "A")); got != want {
			t.Errorf("GET\n got:\n%s\nwant:\n%s", got, want)
		}
		if got := e1List(stored(" G3 ", "A ")); got != want {
			t.Errorf("GET with spaces around the class\n got:\n%s\nwant:\n%s", got, want)
		}
		if got := e1Snapshot(t, db, `grade <> 'G3' OR section <> 'A'`); got != "" {
			t.Errorf("rows stored under another class:\n%s", got)
		}
		if got := e1List(stored("g3", "a")); got != "" {
			t.Errorf("class names are case-sensitive, but g3/a returned:\n%s", got)
		}
	})

	t.Run("a second PUT replaces the first and leaves other classes alone", func(t *testing.T) {
		if r := put(e1Body("G4", "A", e1P("الأحد", 1, "Mathematics"), e1P("الأحد", 2, "Science"), e1P("الخميس", 6, "Music"))); r.status != 200 {
			t.Fatalf("first PUT: %d %s", r.status, r.body)
		}
		put(e1Body("G4", "B", e1P("الأحد", 1, "Reading", "Teacher Example")))
		if _, err := db.Exec(`INSERT INTO weekly_schedules (grade, section, day_of_week, period_number, subject_name)
			VALUES ('G4', 'A', 'Friday', 1, 'Legacy Club'), ('G4', 'A', 'Holiday', 1, 'Legacy Trip'), ('G9', 'Z', 'Sunday', 1, 'Legacy Row')`); err != nil {
			t.Fatal(err)
		}
		others := `NOT (grade = 'G4' AND section = 'A')`
		before := e1Snapshot(t, db, others)

		if r := put(e1Body("G4", "A", e1P("Monday", 3, "Art", "Teacher Sample"), e1P("الأحد", 2, "Drawing"))); r.status != 200 {
			t.Fatalf("second PUT: %d %s", r.status, r.body)
		}
		want := e1List([]e1Period{e1P("الأحد", 2, "Drawing"), e1P("الإثنين", 3, "Art", "Teacher Sample")})
		if got := e1List(stored("G4", "A")); got != want {
			t.Errorf("after the second PUT\n got:\n%s\nwant:\n%s", got, want)
		}
		if after := e1Snapshot(t, db, others); after != before {
			t.Errorf("other classes changed\nbefore:\n%s\nafter:\n%s", before, after)
		}

		saved := e1Decode(t, put(e1Body("G4", "A")))
		if len(saved.Data) != 0 {
			t.Errorf("PUT [] returned %v, want an empty schedule", saved.Data)
		}
		if got := e1Snapshot(t, db, `grade = 'G4' AND section = 'A'`); got != "" {
			t.Errorf("PUT [] left rows:\n%s", got)
		}
		if after := e1Snapshot(t, db, others); after != before {
			t.Errorf("PUT [] changed other classes\nbefore:\n%s\nafter:\n%s", before, after)
		}
		if got := stored("G4", "A"); len(got) != 0 {
			t.Errorf("GET after PUT []: %v", got)
		}
	})

	t.Run("day names are stored in their canonical Arabic form", func(t *testing.T) {
		for _, tc := range []struct{ in, want string }{
			{"الأحد", "الأحد"}, {"الاحد", "الأحد"}, {"Sunday", "الأحد"}, {" SUNDAY ", "الأحد"},
			{"الإثنين", "الإثنين"}, {"الاثنين", "الإثنين"}, {"monday", "الإثنين"},
			{"الثلاثاء", "الثلاثاء"}, {"TuEsDaY", "الثلاثاء"}, {"  الثلاثاء  ", "الثلاثاء"},
			{"الأربعاء", "الأربعاء"}, {"الاربعاء", "الأربعاء"}, {"Wednesday\t", "الأربعاء"},
			{"الخميس", "الخميس"}, {" الخميس", "الخميس"}, {"THURSDAY", "الخميس"},
		} {
			if r := put(e1Body("G5", "D", e1P(tc.in, 1, "Mathematics"))); r.status != 200 {
				t.Errorf("%q: %d %s", tc.in, r.status, r.body)
				continue
			}
			var day string
			if err := db.QueryRow(`SELECT day_of_week FROM weekly_schedules WHERE grade = 'G5' AND section = 'D'`).Scan(&day); err != nil {
				t.Fatal(err)
			}
			if day != tc.want {
				t.Errorf("%q stored as %q, want %q", tc.in, day, tc.want)
			}
		}
		put(e1Body("G5", "D", e1P("Sunday", 1, "Mathematics")))
		before := e1Snapshot(t, db, `TRUE`)
		for _, day := range []string{"Friday", "friday", "الجمعة", "Saturday", "السبت", "Funday", "Sun", "الأحد الأحد", "", "   ", "١"} {
			e1Error(t, put(e1Body("G5", "D", e1P(day, 1, "Mathematics"))), 400, "periods[0].day_of_week must be a school day, Sunday to Thursday (Arabic or English)")
		}
		if after := e1Snapshot(t, db, `TRUE`); after != before {
			t.Errorf("rejected day names changed the table\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	t.Run("validation", func(t *testing.T) {
		long := func(n int) string { return strings.Repeat("ص", n) }
		ok := e1P("الأحد", 1, "Mathematics")
		for _, tc := range []struct {
			name    string
			grade   string
			section string
			omit    string
			msg     string
		}{
			{"no grade", "", "A", "grade", "grade is required"},
			{"no section", "G6", "", "section", "section is required"},
			{"blank grade", "  ", "A", "", "grade is required"},
			{"blank section", "G6", " \t", "", "section is required"},
			{"grade too long", long(51), "A", "", "grade must be at most 50 characters"},
			{"section too long", "G6", long(51), "", "section must be at most 50 characters"},
		} {
			body := e1Body(tc.grade, tc.section, ok)
			if tc.omit != "" {
				delete(body, tc.omit)
			}
			e1Error(t, put(body), 400, tc.msg)
			if tc.omit != "section" {
				q := url.Values{"section": {tc.section}}
				if tc.omit == "" {
					q.Set("grade", tc.grade)
				}
				e1Error(t, a14Do(t, srv, "GET", "/api/admin/schedule?"+q.Encode(), a14Bearer(admin), nil), 400, tc.msg)
			} else {
				e1Error(t, a14Do(t, srv, "GET", "/api/admin/schedule?grade=G6", a14Bearer(admin), nil), 400, tc.msg)
			}
		}
		e1Error(t, a14Do(t, srv, "GET", "/api/admin/schedule", a14Bearer(admin), nil), 400, "grade is required")

		for _, tc := range []struct {
			name    string
			periods []e1Period
			msg     string
		}{
			{"period 0", []e1Period{ok, e1P("الأحد", 0, "Science")}, "periods[1].period_number must be from 1 to 12"},
			{"period 13", []e1Period{e1P("الخميس", 13, "Science")}, "periods[0].period_number must be from 1 to 12"},
			{"period -1", []e1Period{e1P("الخميس", -1, "Science")}, "periods[0].period_number must be from 1 to 12"},
			{"blank subject", []e1Period{ok, e1P("الأحد", 2, "Science"), e1P("الإثنين", 1, "   ")}, "periods[2].subject_name is required"},
			{"missing subject", []e1Period{e1P("الإثنين", 1, "")}, "periods[0].subject_name is required"},
			{"subject too long", []e1Period{e1P("الإثنين", 1, long(101))}, "periods[0].subject_name must be at most 100 characters"},
			{"teacher too long", []e1Period{e1P("الإثنين", 1, "Science", long(101))}, "periods[0].teacher_name must be at most 100 characters"},
			{"same pair twice", []e1Period{ok, e1P("الأحد", 2, "Science"), e1P("الأحد", 1, "Art")}, "periods contain day_of_week الأحد with period_number 1 more than once"},
			{"same pair in two spellings", []e1Period{e1P("Sunday", 4, "Art"), e1P("الاحد", 4, "Music")}, "periods contain day_of_week الأحد with period_number 4 more than once"},
		} {
			e1Error(t, put(e1Body("G6", "A", tc.periods...)), 400, tc.msg)
		}

		var many []e1Period
		for _, day := range []string{"الأحد", "الإثنين", "الثلاثاء", "الأربعاء", "الخميس"} {
			for n := 1; n <= 12; n++ {
				many = append(many, e1P(day, n, long(100), long(100)))
			}
		}
		e1Error(t, put(e1Body("G6", "A", append(many, e1P("الأحد", 1, "Extra"))...)), 400, "periods must contain at most 60 entries")
		body := e1Body("G6", "A")
		delete(body, "periods")
		e1Error(t, put(body), 400, "periods is required; send an empty array to clear the schedule")
		e1Error(t, put(map[string]any{"grade": "G6", "section": "A", "periods": nil}), 400, "periods is required; send an empty array to clear the schedule")
		e1Error(t, put("{"), 400, "Invalid request body")
		e1Error(t, put(`{"grade":"G6","section":"A","periods":[{"day_of_week":"Sunday","period_number":1.5,"subject_name":"Art"}]}`), 400, "Invalid request body")
		e1Error(t, put(`{"grade":"G6","section":"A","periods":[{"day_of_week":"Sunday","period_number":"1","subject_name":"Art"}]}`), 400, "Invalid request body")
		if got := e1Snapshot(t, db, `grade = 'G6'`); got != "" {
			t.Errorf("rejected requests stored rows:\n%s", got)
		}

		if r := put(e1Body("G6", "A", many...)); r.status != 200 {
			t.Fatalf("60 periods at every limit: %d %s", r.status, r.body)
		}
		var n, full int
		db.QueryRow(`SELECT COUNT(*), COUNT(*) FILTER (WHERE char_length(subject_name) = 100 AND char_length(teacher_name) = 100) FROM weekly_schedules WHERE grade = 'G6' AND section = 'A'`).Scan(&n, &full)
		if n != 60 || full != 60 {
			t.Errorf("stored %d periods, %d with 100-character names; want 60 and 60", n, full)
		}
		if r := put(e1Body(long(50), long(50), ok)); r.status != 200 {
			t.Errorf("grade and section of 50 characters: %d %s", r.status, r.body)
		} else if got := stored(long(50), long(50)); len(got) != 1 {
			t.Errorf("50-character class read back %v", got)
		}

		blank, padded := "   ", "  Teacher Example  "
		r := put(map[string]any{"grade": "G6", "section": "B", "periods": []map[string]any{
			{"day_of_week": "الأحد", "period_number": 1, "subject_name": "  Mathematics  ", "teacher_name": blank},
			{"day_of_week": "الأحد", "period_number": 2, "subject_name": "Science", "teacher_name": nil},
			{"day_of_week": "الأحد", "period_number": 3, "subject_name": "Art"},
			{"day_of_week": "الأحد", "period_number": 4, "subject_name": "Music", "teacher_name": padded},
		}})
		want := e1List([]e1Period{e1P("الأحد", 1, "Mathematics"), e1P("الأحد", 2, "Science"), e1P("الأحد", 3, "Art"), e1P("الأحد", 4, "Music", "Teacher Example")})
		if got := e1List(e1Decode(t, r).Data); got != want {
			t.Errorf("trimmed names and null teachers\n got:\n%s\nwant:\n%s", got, want)
		}
		var nulls int
		db.QueryRow(`SELECT COUNT(*) FROM weekly_schedules WHERE grade = 'G6' AND section = 'B' AND teacher_name IS NULL`).Scan(&nulls)
		if nulls != 3 {
			t.Errorf("%d NULL teacher_name rows, want 3 (blank, null and omitted)", nulls)
		}
	})

	t.Run("a failed PUT leaves the previous schedule exactly as it was", func(t *testing.T) {
		if r := put(e1Body("G7", "C", e1P("الأحد", 1, "Mathematics", "Teacher Example"), e1P("الإثنين", 2, "Science"))); r.status != 200 {
			t.Fatalf("baseline PUT: %d %s", r.status, r.body)
		}
		before := e1Snapshot(t, db, `TRUE`)
		e1Error(t, put(e1Body("G7", "C", e1P("الأحد", 1, "Art"), e1P("الأحد", 2, "Music"), e1P("الخميس", 13, "Drawing"))), 400, "periods[2].period_number must be from 1 to 12")
		if after := e1Snapshot(t, db, `TRUE`); after != before {
			t.Errorf("a 400 changed the table\nbefore:\n%s\nafter:\n%s", before, after)
		}

		if _, err := db.Exec(`ALTER TABLE weekly_schedules ADD CONSTRAINT e1_refuse_poison CHECK (subject_name <> 'Poison')`); err != nil {
			t.Fatal(err)
		}
		defer db.Exec(`ALTER TABLE weekly_schedules DROP CONSTRAINT e1_refuse_poison`)
		e1Error(t, put(e1Body("G7", "C", e1P("الأحد", 1, "Art"), e1P("الأحد", 2, "Music"), e1P("الخميس", 1, "Poison"))), 500, "Failed to save schedule")
		if after := e1Snapshot(t, db, `TRUE`); after != before {
			t.Errorf("a database failure mid-PUT changed the table\nbefore:\n%s\nafter:\n%s", before, after)
		}
		if srv.out.index("AdminScheduleHandler: save failed") < 0 {
			t.Errorf("the 500 was not logged as AdminScheduleHandler: save failed")
		}
	})

	t.Run("concurrent PUTs for one class never mix or fail", func(t *testing.T) {
		alpha := []e1Period{e1P("الأحد", 1, "Alpha"), e1P("الأحد", 2, "Alpha"), e1P("الإثنين", 1, "Alpha"), e1P("الخميس", 5, "Alpha")}
		beta := []e1Period{e1P("الأحد", 1, "Beta"), e1P("الأحد", 3, "Beta"), e1P("الإثنين", 1, "Beta"), e1P("الثلاثاء", 2, "Beta"), e1P("الخميس", 5, "Beta"), e1P("الخميس", 6, "Beta")}
		wantA, wantB := e1List(alpha), e1List(beta)
		for round := 0; round < 20; round++ {
			if round%2 == 0 {
				put(e1Body("G8", "A"))
			}
			var wg sync.WaitGroup
			start := make(chan struct{})
			statuses := make([]int, 2)
			bodies := make([][]byte, 2)
			errs := make([]error, 2)
			for i, ps := range [][]e1Period{alpha, beta} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					statuses[i], bodies[i], errs[i] = e1Send(srv, "PUT", "/api/admin/schedule", admin, e1Body("G8", "A", ps...))
				}()
			}
			close(start)
			wg.Wait()
			for i := range statuses {
				if errs[i] != nil || statuses[i] != 200 {
					t.Fatalf("round %d request %d: %d %s %v", round, i, statuses[i], bodies[i], errs[i])
				}
				saves++
			}
			if got := e1List(stored("G8", "A")); got != wantA && got != wantB {
				t.Fatalf("round %d: final schedule is neither request:\n%s", round, got)
			}
		}
	})

	t.Run("the parent app shows exactly what the admin saved", func(t *testing.T) {
		const parentPhone, parentPin = "+9647000001101", "Nc6?Fa3w@Ub8rZ5k"
		create := func(name, grade, section string) int {
			t.Helper()
			r := a14Do(t, srv, "POST", "/api/admin/students", a14Bearer(admin), map[string]any{
				"name": name, "parent_name": "Omar Example", "parent_phone": parentPhone, "parent_pin": parentPin, "grade": grade, "section": section,
			})
			if r.status != 200 {
				t.Fatalf("create %s: %d %s", name, r.status, r.body)
			}
			return int(r.json(t)["data"].(map[string]any)["id"].(float64))
		}
		classOf := func(id int) string {
			t.Helper()
			var g, s sql.NullString
			if err := db.QueryRow(`SELECT grade, section FROM students WHERE id = $1`, id).Scan(&g, &s); err != nil {
				t.Fatal(err)
			}
			return fmt.Sprintf("%q/%q", g.String, s.String)
		}
		inClass := create("Sara Example", " G9 ", "  A ")
		other := create("Ali Example", "G9", "B")
		if got := classOf(inClass); got != `"G9"/"A"` {
			t.Errorf("student created with \" G9 \"/\"  A \" stored as %s, want \"G9\"/\"A\"", got)
		}
		blank := create("Noor Example", "   ", " ")
		if got := classOf(blank); got != `""/""` {
			t.Errorf("blank grade and section stored as %s, want empty strings as before", got)
		}

		saved := e1Decode(t, put(e1Body("G9", "A",
			e1P("Thursday", 1, "Art"), e1P("الأحد", 2, "Science", "Teacher Example"), e1P("الأحد", 1, "Mathematics"), e1P("الاربعاء", 3, "Music", "Teacher Sample"),
		))).Data

		login := a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": parentPhone, "pin": parentPin})
		if login.status != 200 {
			t.Fatalf("parent login: %d %s", login.status, login.body)
		}
		parent := fmt.Sprint(login.json(t)["data"].(map[string]any)["token"])
		parentView := func() map[int]string {
			t.Helper()
			r := a14Do(t, srv, "GET", "/api/mobile/schedule", a14Bearer(parent), nil)
			if r.status != 200 {
				t.Fatalf("parent schedule: %d %s", r.status, r.body)
			}
			var resp struct {
				Data []struct {
					StudentID int    `json:"student_id"`
					Day       string `json:"day_of_week"`
					Period    int    `json:"period_number"`
					Subject   string `json:"subject_name"`
					Teacher   string `json:"teacher_name"`
				} `json:"data"`
			}
			json.Unmarshal(r.body, &resp)
			byChild := map[int][]string{}
			for _, e := range resp.Data {
				byChild[e.StudentID] = append(byChild[e.StudentID], fmt.Sprintf("%s|%d|%s|%s", e.Day, e.Period, e.Subject, e.Teacher))
			}
			out := map[int]string{}
			for id, rows := range byChild {
				out[id] = strings.Join(rows, "\n")
			}
			return out
		}
		var want []string
		for _, p := range saved {
			teacher := ""
			if p.Teacher != nil {
				teacher = *p.Teacher
			}
			want = append(want, fmt.Sprintf("%s|%d|%s|%s", p.Day, p.Period, p.Subject, teacher))
		}
		view := parentView()
		if got := view[inClass]; got != strings.Join(want, "\n") {
			t.Errorf("parent view of the class\n got:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
		}
		if got := view[other]; got != "" {
			t.Errorf("child in G9/B sees:\n%s", got)
		}
		if got := view[blank]; got != "" {
			t.Errorf("child without a class sees:\n%s", got)
		}

		r := a14Do(t, srv, "PUT", "/api/admin/students", a14Bearer(admin), map[string]any{
			"id": other, "name": "Ali Example", "parent_name": "Omar Example", "parent_phone": parentPhone, "grade": "  G9  ", "section": " A",
		})
		if r.status != 200 {
			t.Fatalf("update: %d %s", r.status, r.body)
		}
		if got := classOf(other); got != `"G9"/"A"` {
			t.Errorf("student updated with \"  G9  \"/\" A\" stored as %s", got)
		}
		if got := parentView()[other]; got != strings.Join(want, "\n") {
			t.Errorf("after moving to G9/A the child sees\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
		}
	})

	t.Run("each save is logged with grade, section and the number of periods only", func(t *testing.T) {
		var lines []map[string]any
		for _, raw := range strings.Split(srv.out.String(), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(raw), &m) == nil && m["msg"] == "Schedule saved" {
				lines = append(lines, m)
			}
		}
		if len(lines) != saves {
			t.Errorf("%d \"Schedule saved\" lines for %d successful PUTs", len(lines), saves)
		}
		for _, m := range lines {
			var keys []string
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if strings.Join(keys, ",") != "grade,level,msg,periods,section,time" || m["level"] != "INFO" {
				t.Errorf("log line %v: want only time, level INFO, msg, grade, section, periods", m)
			}
		}
		found := false
		for _, m := range lines {
			if m["grade"] == "G6" && m["section"] == "A" && m["periods"] == float64(60) {
				found = true
			}
		}
		if !found {
			t.Errorf("no log line for the 60-period save of G6/A")
		}
	})

	t.Run("authentication", func(t *testing.T) {
		later, earlier := time.Now().Add(time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
		expired := e1Sign(t, jwt.MapClaims{"username": "admin", "role": "admin", "sv": 0, "exp": earlier})
		parent := e1Sign(t, jwt.MapClaims{"parent_id": 1, "phone": "+9647000001101", "role": "parent", "sv": 0, "exp": later})
		calls := []struct{ method, path string }{{"GET", "/api/admin/schedule?grade=G3&section=A"}, {"PUT", "/api/admin/schedule"}}
		body := e1Body("G3", "A")
		before := e1Snapshot(t, db, `TRUE`)
		for _, c := range calls {
			for _, tc := range []struct {
				name   string
				header map[string]string
				status int
				msg    string
			}{
				{"no token", nil, 401, "Unauthorized"},
				{"malformed token", a14Bearer("not-a-jwt"), 401, "Unauthorized"},
				{"wrong scheme", map[string]string{"Authorization": "Basic " + admin}, 401, "Unauthorized"},
				{"expired token", a14Bearer(expired), 401, "Unauthorized"},
				{"parent token", a14Bearer(parent), 403, "Forbidden"},
			} {
				r := a14Do(t, srv, c.method, c.path, tc.header, body)
				if r.status != tc.status || r.json(t)["message"] != tc.msg {
					t.Errorf("%s %s with %s: %d %s, want %d %s", c.method, c.path, tc.name, r.status, r.body, tc.status, tc.msg)
				}
			}
		}

		script, err := os.ReadFile(filepath.Join("..", "..", "db", "scripts", "rotate_admin_password.sql"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(strings.ReplaceAll(string(script), "NEW_PASSWORD_HERE", "e1-rotated-password")); err != nil {
			t.Fatalf("rotation script: %v", err)
		}
		for _, c := range calls {
			if r := a14Do(t, srv, c.method, c.path, a14Bearer(admin), body); r.status != 401 {
				t.Errorf("%s %s with a token from before the rotation: %d %s, want 401", c.method, c.path, r.status, r.body)
			}
		}
		if after := e1Snapshot(t, db, `TRUE`); after != before {
			t.Errorf("rejected tokens changed the table")
		}
		login := a14AdminLogin(t, srv, "198.51.100.3", "admin", "e1-rotated-password")
		if login.status != 200 {
			t.Fatalf("login after rotation: %d %s", login.status, login.body)
		}
		fresh := fmt.Sprint(login.json(t)["data"].(map[string]any)["token"])
		if r := a14Do(t, srv, "GET", "/api/admin/schedule?grade=G3&section=A", a14Bearer(fresh), nil); r.status != 200 {
			t.Errorf("new token after rotation: %d %s", r.status, r.body)
		}
	})

	t.Run("wrong method", func(t *testing.T) {
		r := a14Do(t, srv, "DELETE", "/api/admin/schedule?grade=G3&section=A", nil, nil)
		if r.status != 405 || r.header.Get("Allow") != "GET, HEAD, PUT" {
			t.Errorf("DELETE: %d Allow=%q %s, want 405 Allow=\"GET, HEAD, PUT\"", r.status, r.header.Get("Allow"), r.body)
		}
	})
}
