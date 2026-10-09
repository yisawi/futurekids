package warnings

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"future_kids/internal/handlers"
	"future_kids/internal/tz"

	"github.com/golang-jwt/jwt/v5"
	excelize "github.com/xuri/excelize/v2"
)

var i1Header = []any{"المرحلة", "الشعبة", "اليوم", "الحصة", "المادة", "المعلم"}

// i1Sheet is one worksheet of a generated workbook. Cell values are strings, numbers, nil
// (blank) or i1Formula (a formula without a saved value).
type i1Sheet struct {
	name string
	rows [][]any
}

type i1Formula string

func i1Book(t *testing.T, sheets ...i1Sheet) []byte {
	t.Helper()
	f := excelize.NewFile()
	defer f.Close()
	for i, sh := range sheets {
		if i == 0 {
			f.SetSheetName("Sheet1", sh.name)
		} else if _, err := f.NewSheet(sh.name); err != nil {
			t.Fatal(err)
		}
		for r, row := range sh.rows {
			for c, v := range row {
				cell, _ := excelize.CoordinatesToCellName(c+1, r+1)
				switch x := v.(type) {
				case nil:
				case string:
					f.SetCellStr(sh.name, cell, x)
				case i1Formula:
					f.SetCellFormula(sh.name, cell, string(x))
				default:
					f.SetCellValue(sh.name, cell, x)
				}
			}
		}
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// i1Patch rewrites the zip entry name with edit (used to put raw cell XML into a sheet).
func i1Patch(t *testing.T, data []byte, name string, edit func(string) string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	found := false
	for _, zf := range zr.File {
		rc, _ := zf.Open()
		content, _ := io.ReadAll(rc)
		rc.Close()
		if zf.Name == name {
			content, found = []byte(edit(string(content))), true
		}
		w, _ := zw.Create(zf.Name)
		w.Write(content)
	}
	zw.Close()
	if !found {
		t.Fatalf("no %s in the workbook", name)
	}
	return buf.Bytes()
}

// i1SetCellXML replaces cell ref of the first worksheet with raw XML.
func i1SetCellXML(t *testing.T, data []byte, ref, xml string) []byte {
	t.Helper()
	re := regexp.MustCompile(`<c r="` + ref + `"[^>]*?(/>|>.*?</c>)`)
	return i1Patch(t, data, "xl/worksheets/sheet1.xml", func(s string) string {
		if !re.MatchString(s) {
			t.Fatalf("cell %s not in the sheet", ref)
		}
		return re.ReplaceAllLiteralString(s, xml)
	})
}

// i1PadTo returns a valid workbook of exactly size bytes: data plus a stored padding entry.
func i1PadTo(t *testing.T, data []byte, size int) []byte {
	t.Helper()
	build := func(n int) []byte {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, zf := range zr.File {
			rc, _ := zf.Open()
			content, _ := io.ReadAll(rc)
			rc.Close()
			w, _ := zw.Create(zf.Name)
			w.Write(content)
		}
		w, _ := zw.CreateHeader(&zip.FileHeader{Name: "xl/media/padding.bin", Method: zip.Store})
		w.Write(bytes.Repeat([]byte("p"), n))
		zw.Close()
		return buf.Bytes()
	}
	out := build(size - len(build(0)))
	if len(out) != size {
		t.Fatalf("padded workbook has %d bytes, want %d", len(out), size)
	}
	return out
}

// i1Rows lists class rows as "grade|section|day|period|subject|teacher" for the classes in the
// grid, n periods per day starting at 1.
func i1Rows(grade, section string, days map[string]int, subject, teacher string) [][]any {
	var out [][]any
	for _, day := range []string{"الأحد", "الإثنين", "الثلاثاء", "الأربعاء", "الخميس"} {
		for p := 1; p <= days[day]; p++ {
			var tv any
			if teacher != "" {
				tv = teacher
			}
			out = append(out, []any{grade, section, day, fmt.Sprint(p), subject, tv})
		}
	}
	return out
}

func i1Table(rows ...[][]any) [][]any {
	out := [][]any{i1Header}
	for _, r := range rows {
		out = append(out, r...)
	}
	return out
}

// i1Snapshot lists every weekly_schedules row; withIDs includes the ids.
func i1Snapshot(t *testing.T, db *sql.DB, withIDs bool) string {
	t.Helper()
	rows, err := db.Query(`SELECT id, grade, section, day_of_week, period_number, subject_name, COALESCE(teacher_name, '<NULL>')
		FROM weekly_schedules ORDER BY grade, section, day_of_week, period_number, id`)
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
		if withIDs {
			fmt.Fprintf(&b, "%d|", id)
		}
		fmt.Fprintf(&b, "%s|%s|%s|%d|%s|%s\n", grade, section, day, period, subject, teacher)
	}
	return b.String()
}

func i1Import(t *testing.T, srv *g3Server, token string, data []byte, extra ...f1Part) a14Response {
	t.Helper()
	parts := append([]f1Part{{"file", "schedule.xlsx", handlers.XLSXContentType, data}}, extra...)
	ct, body := f1Form(parts...)
	return f1Do(t, srv, "POST", "/api/admin/schedule/import", a14Bearer(token), ct, body)
}

func i1Send(srv *g3Server, method, path, token, contentType string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(method, srv.url(path), bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

type i1Problem struct {
	Sheet   string `json:"sheet"`
	Row     int    `json:"row"`
	Column  string `json:"column"`
	Message string `json:"message"`
}

type i1Report struct {
	Status       string `json:"status"`
	Message      string `json:"message"`
	DryRun       bool   `json:"dry_run"`
	TotalPeriods int    `json:"total_periods"`
	Classes      []struct {
		Grade           string `json:"grade"`
		Section         string `json:"section"`
		Periods         int    `json:"periods"`
		MatchedStudents int    `json:"matched_students"`
	} `json:"classes"`
	Warnings []string    `json:"warnings"`
	Errors   []i1Problem `json:"errors"`
}

func i1Decode(t *testing.T, r a14Response, status int) i1Report {
	t.Helper()
	if r.status != status {
		t.Fatalf("got %d %s, want %d", r.status, r.body, status)
	}
	var rep i1Report
	if err := json.Unmarshal(r.body, &rep); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return rep
}

func i1Workbook(t *testing.T, r a14Response) *excelize.File {
	t.Helper()
	if r.status != 200 {
		t.Fatalf("export: %d %s", r.status, r.body)
	}
	f, err := excelize.OpenReader(bytes.NewReader(r.body))
	if err != nil {
		t.Fatalf("export does not open: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func i1DataRows(t *testing.T, f *excelize.File) [][]string {
	t.Helper()
	rows, err := f.GetRows("Sheet1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 5 {
		t.Fatalf("export has %d rows, want the title and header rows", len(rows))
	}
	return rows[5:]
}

// TestI1ScheduleExcel verifies Task I1: the 5-or-6 periods rule on PUT /api/admin/schedule,
// subject_key on every schedule response, the class list, the Excel export and the Excel import.
func TestI1ScheduleExcel(t *testing.T) {
	srv, db := a14Server(t, "i1")
	admin := a14AdminToken(t, srv)
	put := func(body any) a14Response {
		t.Helper()
		return a14Do(t, srv, "PUT", "/api/admin/schedule", a14Bearer(admin), body)
	}
	get := func(path string) a14Response {
		t.Helper()
		return a14Do(t, srv, "GET", path, a14Bearer(admin), nil)
	}
	imports := 0
	importOK := func(t *testing.T, data []byte, extra ...f1Part) i1Report {
		t.Helper()
		rep := i1Decode(t, i1Import(t, srv, admin, data, extra...), 200)
		imports++
		return rep
	}

	exportClass := func(grade, section string) a14Response {
		return get("/api/admin/schedule/export?" + url.Values{"grade": {grade}, "section": {section}}.Encode())
	}

	t.Run("the export needs the grade and section of a known class", func(t *testing.T) {
		for _, tc := range []struct{ query, msg string }{
			{"", "grade is required"},
			{"?section=A", "grade is required"},
			{"?grade=G1", "section is required"},
			{"?grade=&section=A", "grade is required"},
			{"?grade=%20%20&section=A", "grade is required"},
			{"?grade=G1&section=%20", "section is required"},
			{"?grade=" + strings.Repeat("g", 51) + "&section=A", "grade must be at most 50 characters"},
			{"?grade=G1&section=" + strings.Repeat("s", 51), "section must be at most 50 characters"},
		} {
			e1Error(t, get("/api/admin/schedule/export"+tc.query), 400, tc.msg)
		}
		e1Error(t, exportClass("G1", "A"), 404, "No class has this grade and section: no active student and no schedule")
	})

	t.Run("PUT accepts days of exactly 5 or 6 periods", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			periods []e1Period
		}{
			{"5 periods", e1Day("الأحد", 5, "Math")},
			{"6 periods", e1Day("الأحد", 6, "Math")},
			{"mixed days", e1Join(e1Day("الأحد", 5, "Math"), e1Day("Monday", 6, "Art"), e1Rev(e1Day("الخميس", 5, "Music")))},
			{"omitted days", e1Join(e1Day("Tuesday", 6, "Math"), e1Day("Thursday", 5, "Art"))},
			{"a full week", e1Join(e1Day("الأحد", 6, "A"), e1Day("الإثنين", 6, "B"), e1Day("الثلاثاء", 6, "C"), e1Day("الأربعاء", 6, "D"), e1Day("الخميس", 6, "E"))},
		} {
			r := put(e1Body("R1", "A", tc.periods...))
			if r.status != 200 {
				t.Errorf("%s: %d %s", tc.name, r.status, r.body)
				continue
			}
			if got := len(e1Decode(t, r).Data); got != len(tc.periods) {
				t.Errorf("%s: saved %d periods, want %d", tc.name, got, len(tc.periods))
			}
		}
		if r := put(e1Body("R1", "A")); r.status != 200 || len(e1Decode(t, r).Data) != 0 {
			t.Errorf("an empty array must clear the class: %d %s", r.status, r.body)
		}
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM weekly_schedules WHERE grade = 'R1'`).Scan(&n)
		if n != 0 {
			t.Errorf("%d rows left after clearing", n)
		}
	})

	t.Run("PUT rejects any other day shape and names the day", func(t *testing.T) {
		put(e1Body("R2", "A", e1Day("الأحد", 5, "Kept")...))
		before := i1Snapshot(t, db, true)
		rule := "a school day must have exactly 5 or 6 periods, numbered 1 to 5 or 1 to 6"
		var thirtyOne []e1Period
		for _, d := range []string{"الأحد", "الإثنين", "الثلاثاء", "الأربعاء", "الخميس"} {
			thirtyOne = append(thirtyOne, e1Day(d, 6, "Math")...)
		}
		thirtyOne = append(thirtyOne, e1P("الأحد", 1, "Extra"))
		startAt2 := e1Day("الإثنين", 6, "Math")[1:]
		for _, tc := range []struct {
			name    string
			periods []e1Period
			msg     string
		}{
			{"4 periods", e1Join(e1Day("الأحد", 5, "Math"), e1Day("Monday", 4, "Art")), "periods[5].day_of_week الإثنين has periods 1, 2, 3, 4; " + rule},
			{"7 periods", e1Join(e1Day("الأحد", 6, "Math"), []e1Period{e1P("الأحد", 7, "Math")}), "periods[6].period_number must be from 1 to 6"},
			{"a gap", e1Join(e1Day("الخميس", 4, "Math"), []e1Period{e1P("Thursday", 6, "Math")}), "periods[0].day_of_week الخميس has periods 1, 2, 3, 4, 6; " + rule},
			{"starting at 2", startAt2, "periods[0].day_of_week الإثنين has periods 2, 3, 4, 5, 6; " + rule},
			{"a duplicate", e1Join(e1Day("الأحد", 5, "Math"), []e1Period{e1P("Sunday", 3, "Art")}), "periods contain day_of_week الأحد with period_number 3 more than once"},
			{"period 7 alone", []e1Period{e1P("الأربعاء", 7, "Math")}, "periods[0].period_number must be from 1 to 6"},
			{"one period", []e1Period{e1P("الأربعاء", 1, "Math")}, "periods[0].day_of_week الأربعاء has periods 1; " + rule},
			{"31 periods", thirtyOne, "periods must contain at most 30 entries"},
			{"a good day and a short one later", e1Join(e1Day("الثلاثاء", 6, "Math"), e1Day("الأحد", 3, "Art")), "periods[6].day_of_week الأحد has periods 1, 2, 3; " + rule},
		} {
			e1Error(t, put(e1Body("R2", "A", tc.periods...)), 400, tc.msg)
		}
		if after := i1Snapshot(t, db, true); after != before {
			t.Errorf("rejected saves changed the table\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	subjects := []struct{ name, key string }{
		{"الرياضيات", "math"}, {"اللغة العربية", "arabic"}, {"العلوم", "science"}, {"اللغة الإنكليزية", "english"},
		{"التربية الإسلامية", "islamic"}, {"الاجتماعيات", "social"}, {"التربية الفنية", "art"}, {"التربية الرياضية", "pe"},
		{"الحاسوب", "computer"}, {"نشاط حر", "other"}, {"الموسيقى", "music"},
	}
	var g1a []e1Period
	for i, s := range subjects {
		day := []string{"الأحد", "الإثنين"}[i/6]
		g1a = append(g1a, e1P(day, i%6+1, s.name, "معلم تجريبي"))
	}
	g1a = append(g1a, e1P("الإثنين", 6, "Mathematics"))

	t.Run("subject_key is on every schedule response and ignored in requests", func(t *testing.T) {
		raw := func(withKey func(i int) any) []map[string]any {
			var out []map[string]any
			for i, p := range g1a {
				m := map[string]any{"day_of_week": p.Day, "period_number": p.Period, "subject_name": p.Subject, "teacher_name": p.Teacher}
				if withKey != nil {
					m["subject_key"] = withKey(i)
				}
				out = append(out, m)
			}
			return out
		}
		wantKeys := func() []string {
			var out []string
			for _, s := range subjects {
				out = append(out, s.key)
			}
			return append(out, "math")
		}()
		keysOf := func(t *testing.T, r a14Response) []string {
			t.Helper()
			var resp struct {
				Data []map[string]any `json:"data"`
			}
			if err := json.Unmarshal(r.body, &resp); err != nil || r.status != 200 {
				t.Fatalf("%d %s", r.status, r.body)
			}
			var out []string
			for _, e := range resp.Data {
				out = append(out, fmt.Sprint(e["subject_key"]))
			}
			return out
		}
		r := put(map[string]any{"grade": "G1", "section": "أ", "periods": raw(nil)})
		if got := keysOf(t, r); fmt.Sprint(got) != fmt.Sprint(wantKeys) {
			t.Errorf("PUT response subject_key\n got %v\nwant %v", got, wantKeys)
		}
		if got := keysOf(t, get("/api/admin/schedule?grade=G1&section="+url.QueryEscape("أ"))); fmt.Sprint(got) != fmt.Sprint(wantKeys) {
			t.Errorf("GET subject_key\n got %v\nwant %v", got, wantKeys)
		}
		before := i1Snapshot(t, db, false)
		for name, key := range map[string]func(int) any{
			"the keys returned": func(i int) any { return wantKeys[i] },
			"wrong keys":        func(int) any { return "music" },
			"a number":          func(int) any { return 7 },
			"an object":         func(int) any { return map[string]any{"x": []any{1, nil}} },
			"null":              func(int) any { return nil },
		} {
			if r := put(map[string]any{"grade": "G1", "section": "أ", "periods": raw(key)}); r.status != 200 {
				t.Errorf("PUT with subject_key as %s: %d %s", name, r.status, r.body)
			} else if got := keysOf(t, r); fmt.Sprint(got) != fmt.Sprint(wantKeys) {
				t.Errorf("PUT with subject_key as %s changed the computed keys: %v", name, got)
			}
		}
		if after := i1Snapshot(t, db, false); after != before {
			t.Errorf("a PUT with subject_key stored something else\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	const parentPhone, parentPin = "+9647000001901", "Qm4!Zr8t#Lw2nB6x"
	createStudent := func(t *testing.T, name, grade, section string) int {
		t.Helper()
		r := a14Do(t, srv, "POST", "/api/admin/students", a14Bearer(admin), map[string]any{
			"name": name, "parent_name": "I1 Parent", "parent_phone": parentPhone, "parent_pin": parentPin, "grade": grade, "section": section,
		})
		if r.status != 200 {
			t.Fatalf("create %s: %d %s", name, r.status, r.body)
		}
		return int(r.json(t)["data"].(map[string]any)["id"].(float64))
	}
	firstChild := createStudent(t, "I1 Child One", "G1", "أ")
	secondChild := createStudent(t, "I1 Child Two", "G2", "ب")
	login := a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": parentPhone, "pin": parentPin})
	if login.status != 200 {
		t.Fatalf("parent login: %d %s", login.status, login.body)
	}
	parent := fmt.Sprint(login.json(t)["data"].(map[string]any)["token"])
	parentSchedule := func(t *testing.T) []byte {
		t.Helper()
		r := a14Do(t, srv, "GET", "/api/mobile/schedule", a14Bearer(parent), nil)
		if r.status != 200 {
			t.Fatalf("parent schedule: %d %s", r.status, r.body)
		}
		return r.body
	}

	t.Run("the parent schedule only gains subject_key", func(t *testing.T) {
		body := parentSchedule(t)
		dec := json.NewDecoder(bytes.NewReader(body))
		var order []string
		depth := 0
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			switch v := tok.(type) {
			case json.Delim:
				if v == '{' || v == '[' {
					depth++
				} else {
					depth--
				}
			case string:
				if depth == 3 && len(order) < 9 && dec.More() {
					order = append(order, v)
					var skip any
					dec.Decode(&skip)
				}
			}
		}
		want := "student_id student_name grade section day_of_week period_number subject_name teacher_name subject_key"
		if strings.Join(order, " ") != want {
			t.Errorf("keys of a parent schedule entry: %v, want %s", order, want)
		}
		var resp struct {
			Status string           `json:"status"`
			Data   []map[string]any `json:"data"`
		}
		json.Unmarshal(body, &resp)
		if len(resp.Data) != len(g1a) {
			t.Fatalf("parent sees %d entries, want %d", len(resp.Data), len(g1a))
		}
		for i, e := range resp.Data {
			p := g1a[i]
			teacher := ""
			if p.Teacher != nil {
				teacher = *p.Teacher
			}
			got := fmt.Sprintf("%v|%v|%v|%v|%v|%v|%v|%v|%v", e["student_id"], e["student_name"], e["grade"], e["section"], e["day_of_week"], e["period_number"], e["subject_name"], e["teacher_name"], e["subject_key"])
			wantRow := fmt.Sprintf("%d|I1 Child One|G1|أ|%s|%d|%s|%s|%s", firstChild, p.Day, p.Period, p.Subject, teacher, handlers.SubjectKey(p.Subject))
			if got != wantRow {
				t.Errorf("entry %d: %s, want %s", i, got, wantRow)
			}
			if len(e) != 9 {
				t.Errorf("entry %d has %d keys, want 9", i, len(e))
			}
		}
	})

	t.Run("classes are the students' and the stored classes with their counts", func(t *testing.T) {
		for _, q := range []string{
			`INSERT INTO students (full_name, rfid_tag, grade, section, is_active) VALUES
				('I1 Extra', 'I1-T-1', 'G1', 'أ', true),
				('I1 Former', 'I1-T-2', 'G1', 'أ', false),
				('I1 Former Only', 'I1-T-3', 'G5', 'ج', false),
				('I1 No Grade', 'I1-T-4', NULL, 'أ', true),
				('I1 Blank Grade', 'I1-T-5', '  ', 'أ', true),
				('I1 Blank Section', 'I1-T-6', 'G5', '', true)`,
			`INSERT INTO weekly_schedules (grade, section, day_of_week, period_number, subject_name) VALUES
				('L9', 'Z', 'Sunday', 1, 'Legacy A'), ('L9', 'Z', 'الاحد', 2, 'Legacy B'), ('L9', 'Z', 'Friday', 1, 'Legacy C'),
				('L9', 'Z', 'Holiday', 1, 'Legacy D'), ('L9', 'Z', 'الإثنين', 9, 'Legacy E'), ('L9', 'Z', 'الأحد', 1, 'Legacy F')`,
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		r := get("/api/admin/schedule/classes")
		if r.status != 200 {
			t.Fatalf("classes: %d %s", r.status, r.body)
		}
		var resp struct {
			Status string           `json:"status"`
			Data   []map[string]any `json:"data"`
		}
		json.Unmarshal(r.body, &resp)
		var got []string
		for _, c := range resp.Data {
			if len(c) != 5 {
				t.Errorf("class %v has %d keys, want 5", c, len(c))
			}
			got = append(got, fmt.Sprintf("%v/%v students=%v periods=%v days=%v", c["grade"], c["section"], c["student_count"], c["period_count"], c["day_count"]))
		}
		want := []string{
			"G1/أ students=2 periods=12 days=2",
			"G2/ب students=1 periods=0 days=0",
			"L9/Z students=0 periods=6 days=4",
			"R2/A students=0 periods=5 days=1",
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("classes\n got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})

	days := []string{"الأحد", "الإثنين", "الثلاثاء", "الأربعاء", "الخميس"}
	grid := func(t *testing.T, f *excelize.File, grade, section string) [][]string {
		t.Helper()
		data := i1DataRows(t, f)
		if len(data) != 30 {
			t.Fatalf("%s/%s: %d data rows, want 30", grade, section, len(data))
		}
		for i, row := range data {
			for len(row) < 6 {
				row = append(row, "")
			}
			want := fmt.Sprintf("%s|%s|%s|%d", grade, section, days[i/6], i%6+1)
			if got := strings.Join(row[:4], "|"); got != want {
				t.Fatalf("row %d is %s, want %s", i+6, got, want)
			}
			data[i] = row
		}
		return data
	}

	t.Run("the export is an editable grid of one class", func(t *testing.T) {
		if r := put(e1Body("R2", "A", e1Join(e1Day("الأحد", 5, "=SUM(A1:A2)"), e1Day("الخميس", 6, "+Plus", "@Teacher"))...)); r.status != 200 {
			t.Fatalf("PUT: %d %s", r.status, r.body)
		}
		r := exportClass("G1", "أ")
		if ct := r.header.Get("Content-Type"); ct != handlers.XLSXContentType {
			t.Errorf("Content-Type %q", ct)
		}
		f := i1Workbook(t, r)
		view, err := f.GetSheetView("Sheet1", 0)
		if err != nil || view.RightToLeft == nil || !*view.RightToLeft {
			t.Errorf("the sheet is not right-to-left")
		}
		for cell, want := range map[string]string{"A1": "وزارة التربية والتعليم", "A2": "مدرسة الرحمن الابتدائية الأهلية", "A3": "الجدول الأسبوعي - G1 أ"} {
			if v, _ := f.GetCellValue("Sheet1", cell); v != want {
				t.Errorf("%s = %q, want %q", cell, v, want)
			}
		}
		if sheets := f.GetSheetList(); len(sheets) != 1 {
			t.Errorf("worksheets %v, want one", sheets)
		}
		rows, _ := f.GetRows("Sheet1")
		if fmt.Sprint(rows[4]) != fmt.Sprint(i1Header) {
			t.Errorf("header row %v", rows[4])
		}
		data := grid(t, f, "G1", "أ")
		filled := 0
		for _, row := range data {
			if row[4] != "" {
				filled++
			}
		}
		if filled != len(g1a) {
			t.Errorf("G1/أ export has %d filled rows, want %d", filled, len(g1a))
		}
		for i, p := range g1a {
			row := data[dayIndex(p.Day)*6+p.Period-1]
			teacher := ""
			if p.Teacher != nil {
				teacher = *p.Teacher
			}
			if row[4] != p.Subject || row[5] != teacher {
				t.Errorf("G1/أ period %d: %v", i, row)
			}
		}

		template := grid(t, i1Workbook(t, exportClass("G2", "ب")), "G2", "ب")
		for _, row := range template {
			if row[4] != "" || row[5] != "" {
				t.Errorf("G2/ب has students but no schedule, yet exports %v", row)
			}
		}

		legacy := grid(t, i1Workbook(t, exportClass("L9", "Z")), "L9", "Z")
		if got := fmt.Sprint(legacy[0][4], legacy[1][4], legacy[6][4], legacy[2][4]); got != "Legacy ALegacy B" {
			t.Errorf("L9/Z Sunday 1, Sunday 2, Monday 1, Sunday 3: %q; want the lowest id for Sunday 1, then Legacy B and blanks", got)
		}
		for _, row := range legacy {
			if strings.Contains(row[4], "Legacy C") || strings.Contains(row[4], "Legacy D") || strings.Contains(row[4], "Legacy E") {
				t.Errorf("a legacy row outside the grid was exported: %v", row)
			}
		}

		rf := i1Workbook(t, get("/api/admin/schedule/export?grade=%20R2%20&section=A%20"))
		r2 := grid(t, rf, "R2", "A")
		if r2[0][4] != "=SUM(A1:A2)" || r2[24][4] != "+Plus" || r2[24][5] != "@Teacher" || r2[5][4] != "" {
			t.Errorf("R2/A rows: %v %v %v", r2[0], r2[24], r2[5])
		}
		for _, cell := range []string{"E6", "E30", "F30", "D6", "A6"} {
			if formula, _ := rf.GetCellFormula("Sheet1", cell); formula != "" {
				t.Errorf("%s holds the formula %q, want text", cell, formula)
			}
			if typ, _ := rf.GetCellType("Sheet1", cell); typ != excelize.CellTypeSharedString {
				t.Errorf("%s has cell type %v, want a string", cell, typ)
			}
		}
		for _, c := range [][2]string{{"g1", "أ"}, {"G1", "ب"}, {"G5", "ج"}} {
			e1Error(t, exportClass(c[0], c[1]), 404, "No class has this grade and section: no active student and no schedule")
		}
	})

	t.Run("the export's file name names the class and the date and is always a valid header", func(t *testing.T) {
		today := tz.Today()
		if cd := exportClass("R2", "A").header.Get("Content-Disposition"); cd != "attachment; filename=schedule_R2_A_"+today+".xlsx" {
			t.Errorf("ASCII class: %q", cd)
		}
		odd := "S/1\"'\\\n;%"
		if r := put(e1Body(odd, "ب/ج", e1Day("الأحد", 5, "Odd")...)); r.status != 200 {
			t.Fatalf("PUT odd class: %d %s", r.status, r.body)
		}
		for _, tc := range []struct{ grade, section, file string }{
			{"G1", "أ", "schedule_G1_أ_" + today + ".xlsx"},
			{"L9", "Z", "schedule_L9_Z_" + today + ".xlsx"},
			{odd, "ب/ج", "schedule_S_1" + strings.Repeat("_", 6) + "_ب_ج_" + today + ".xlsx"},
		} {
			r := exportClass(tc.grade, tc.section)
			if r.status != 200 {
				t.Errorf("%q/%q: %d %s", tc.grade, tc.section, r.status, r.body)
				continue
			}
			cd := r.header.Get("Content-Disposition")
			if strings.ContainsAny(cd, "\r\n") {
				t.Errorf("%q: header holds a line break", cd)
			}
			disposition, params, err := mime.ParseMediaType(cd)
			if err != nil || disposition != "attachment" || params["filename"] != tc.file {
				t.Errorf("%q parses as %q %v (%v), want attachment with filename %q", cd, disposition, params, err, tc.file)
			}
			if ascii := strings.IndexFunc(tc.file, func(r rune) bool { return r > 0x7E }) < 0; !ascii && !strings.HasPrefix(cd, "attachment; filename*=utf-8''") {
				t.Errorf("%q: a non-ASCII name must use filename*", cd)
			}
			if got := grid(t, i1Workbook(t, r), tc.grade, tc.section); len(got) != 30 {
				t.Errorf("%q/%q: %d rows", tc.grade, tc.section, len(got))
			}
		}
		if r := put(e1Body(odd, "ب/ج")); r.status != 200 {
			t.Errorf("clearing the odd class: %d", r.status)
		}
	})

	t.Run("exporting and importing a class unchanged leaves the schedules as they were", func(t *testing.T) {
		if r := put(e1Body("RT", "ج", e1Join(e1Day("الأحد", 6, "اللغة العربية", "معلمة تجريبية 5"), e1Day("الإثنين", 5, "الحاسوب"))...)); r.status != 200 {
			t.Fatalf("PUT RT/ج: %d %s", r.status, r.body)
		}
		before := i1Snapshot(t, db, false)
		for _, c := range []struct {
			grade, section string
			periods        int
		}{{"G1", "أ", len(g1a)}, {"R2", "A", 11}, {"RT", "ج", 11}} {
			data := exportClass(c.grade, c.section).body
			for _, dry := range []string{"true", "false"} {
				rep := importOK(t, data, f1Text("grade", c.grade), f1Text("section", c.section), f1Text("dry_run", dry))
				if rep.TotalPeriods != c.periods || len(rep.Classes) != 1 || rep.Classes[0].Grade != c.grade || rep.Classes[0].Section != c.section {
					t.Errorf("%s/%s dry_run=%s: report %+v", c.grade, c.section, dry, rep)
				}
			}
			if after := i1Snapshot(t, db, false); after != before {
				t.Errorf("round trip of %s/%s changed the schedules\nbefore:\n%s\nafter:\n%s", c.grade, c.section, before, after)
			}
		}
	})

	t.Run("an import for one class accepts only that class", func(t *testing.T) {
		put(e1Body("GD", "أ", e1Day("الأحد", 5, "Before")...))
		before := i1Snapshot(t, db, true)
		guard := func(grade, section string) []f1Part {
			return []f1Part{f1Text("grade", grade), f1Text("section", section)}
		}
		mine := i1Rows("GD", "أ", map[string]int{"الثلاثاء": 6}, "After", "")
		book := func(rows ...[][]any) []byte { return i1Book(t, i1Sheet{"Sheet1", i1Table(rows...)}) }

		dry := importOK(t, book(mine), append(guard(" GD ", "أ "), f1Text("dry_run", "true"))...)
		if !dry.DryRun || dry.TotalPeriods != 6 || len(dry.Classes) != 1 || dry.Classes[0].Grade != "GD" {
			t.Errorf("dry run report %+v", dry)
		}
		if after := i1Snapshot(t, db, true); after != before {
			t.Fatalf("a guarded dry run changed the table")
		}

		other := "this file is for grade GD, section أ only"
		for _, tc := range []struct {
			name  string
			rows  [][]any
			wants []i1Problem
		}{
			{"a row of another section", append(append([][]any{}, mine...), []any{"GD", "ب", "الأحد", "1", "Art", nil}), []i1Problem{{"Sheet1", 8, "الشعبة", other}}},
			{"a row of another grade", append([][]any{{"GX", "أ", "الأحد", "1", "Art", nil}}, mine...), []i1Problem{{"Sheet1", 2, "المرحلة", other}}},
			{"two whole classes", append(append([][]any{}, mine...), i1Rows("GE", "أ", map[string]int{"الأحد": 5}, "Art", "")...), []i1Problem{
				{"Sheet1", 8, "المرحلة", other}, {"Sheet1", 9, "المرحلة", other}, {"Sheet1", 10, "المرحلة", other}, {"Sheet1", 11, "المرحلة", other}, {"Sheet1", 12, "المرحلة", other}}},
		} {
			rep := i1Decode(t, i1Import(t, srv, admin, book(tc.rows), guard("GD", "أ")...), 400)
			if fmt.Sprint(rep.Errors) != fmt.Sprint(tc.wants) {
				t.Errorf("%s: problems\n got %v\nwant %v", tc.name, rep.Errors, tc.wants)
			}
		}

		for _, tc := range []struct {
			name  string
			parts []f1Part
			msg   string
		}{
			{"grade without section", []f1Part{f1Text("grade", "GD")}, "section is required when grade is sent"},
			{"section without grade", []f1Part{f1Text("section", "أ")}, "grade is required when section is sent"},
			{"blank grade", guard("  ", "أ"), "grade is required"},
			{"blank section", guard("GD", ""), "section is required"},
			{"grade of 51 characters", guard(strings.Repeat("ص", 51), "أ"), "grade must be at most 50 characters"},
			{"section of 51 characters", guard("GD", strings.Repeat("s", 51)), "section must be at most 50 characters"},
			{"grade over 200 bytes", guard(strings.Repeat("g", 201), "أ"), "grade must be at most 50 characters"},
			{"section over 200 bytes", guard("GD", strings.Repeat("ص", 101)), "section must be at most 50 characters"},
			{"grade sent twice", append(guard("GD", "أ"), f1Text("grade", "GD")), "grade must be sent once"},
		} {
			e1Error(t, i1Import(t, srv, admin, book(mine), tc.parts...), 400, tc.msg)
		}
		e1Error(t, i1Import(t, srv, admin, book(), guard("GD", "أ")...), 400, "The file has no rows for grade GD, section أ")
		blankGrid := i1Rows("GD", "أ", map[string]int{"الأحد": 6}, "", "")
		e1Error(t, i1Import(t, srv, admin, book(blankGrid), guard("GD", "أ")...), 400, "The file has no rows for grade GD, section أ")
		if ok := importOK(t, book(mine, i1Rows("GE", "أ", map[string]int{"الأحد": 5}, "Art", ""))); len(ok.Classes) != 2 {
			t.Errorf("without the class parts a two-class file must still import both: %+v", ok)
		}
		if after := i1Snapshot(t, db, true); after == before {
			t.Fatalf("the unguarded import saved nothing")
		}
		before = i1Snapshot(t, db, true)
		e1Error(t, i1Import(t, srv, admin, book(append(append([][]any{}, mine...), []any{"GD", "ب", "الأحد", "1", "Art", nil})), guard("GD", "أ")...), 400, "The file has 1 problem; nothing was saved")
		if after := i1Snapshot(t, db, true); after != before {
			t.Errorf("a rejected guarded import changed the table")
		}
		real := importOK(t, book(mine), guard("GD", "أ")...)
		if real.DryRun || len(real.Classes) != 1 {
			t.Errorf("guarded import %+v", real)
		}
		if got := e1List(e1Decode(t, get("/api/admin/schedule?grade=GD&section="+url.QueryEscape("أ"))).Data); got != e1List(e1Day("الثلاثاء", 6, "After")) {
			t.Errorf("GD/أ after the guarded import:\n%s", got)
		}
	})

	t.Run("a class in the file is replaced and other classes are untouched", func(t *testing.T) {
		put(e1Body("G2", "أ", e1Join(e1Day("الأحد", 6, "Old"), e1Day("الإثنين", 5, "Old"), e1Day("الخميس", 5, "Old"))...))
		put(e1Body("G2", "ج", e1Day("الأحد", 5, "Untouched")...))
		others := func() string {
			return e1Snapshot(t, db, `NOT (grade = 'G2' AND section IN ('أ', 'ب'))`)
		}
		before := others()
		book := i1Book(t, i1Sheet{"Sheet1", i1Table(
			i1Rows("G2", "أ", map[string]int{"الثلاثاء": 5}, "العلوم", "معلمة تجريبية"),
			i1Rows("G2", "ب", map[string]int{"الأحد": 6, "الأربعاء": 5}, "الحاسوب", ""),
		)})
		rep := importOK(t, book)
		if rep.TotalPeriods != 16 || len(rep.Classes) != 2 || rep.DryRun || rep.Message != "Schedule imported" {
			t.Errorf("report %+v", rep)
		}
		if got := e1List(e1Decode(t, get("/api/admin/schedule?grade=G2&section="+url.QueryEscape("أ"))).Data); got != e1List(e1Day("الثلاثاء", 5, "العلوم", "معلمة تجريبية")) {
			t.Errorf("G2/أ was not fully replaced:\n%s", got)
		}
		if got := e1List(e1Decode(t, get("/api/admin/schedule?grade=G2&section="+url.QueryEscape("ب"))).Data); got != e1List(e1Join(e1Day("الأحد", 6, "الحاسوب"), e1Day("الأربعاء", 5, "الحاسوب"))) {
			t.Errorf("G2/ب:\n%s", got)
		}
		if after := others(); after != before {
			t.Errorf("classes not in the file changed\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	t.Run("reading rules", func(t *testing.T) {
		shuffled := [][]any{
			{"الجدول الأسبوعي"},
			{nil, "مدرسة تجريبية"},
			{},
			{"ملاحظات", "Teacher", "Period", "SUBJECT", "Grade", "day", "Section"},
		}
		for p := 1; p <= 6; p++ {
			shuffled = append(shuffled, []any{"x", "Teacher Example", fmt.Sprint(p), "Mathematics", "H1", "sunday", "A"})
			if p == 3 {
				shuffled = append(shuffled, []any{}, []any{nil, nil, nil, nil, "H1", "Monday", "A"}, []any{"only a note"})
			}
		}
		aliases := [][]any{{"الصف", "الشعبه", "اليوم", "الحصه", "الماده", "المدرس"}}
		for _, v := range []any{"١", "٢", "۳", 4.0, "5"} {
			aliases = append(aliases, []any{"H1", "B", "الإثنين", v, "اللغة العربية", nil})
		}
		book := i1Book(t,
			i1Sheet{"الأحد", shuffled},
			i1Sheet{"Notes", [][]any{{"Anything", "else"}, {"المرحلة", "الشعبة"}}},
			i1Sheet{"Aliases", aliases},
		)
		rep := importOK(t, book)
		if rep.TotalPeriods != 11 || len(rep.Classes) != 2 {
			t.Fatalf("report %+v", rep)
		}
		if got := e1List(e1Decode(t, get("/api/admin/schedule?grade=H1&section=A")).Data); got != e1List(e1Day("الأحد", 6, "Mathematics", "Teacher Example")) {
			t.Errorf("shuffled columns under title rows:\n%s", got)
		}
		if got := e1List(e1Decode(t, get("/api/admin/schedule?grade=H1&section=B")).Data); got != e1List(e1Day("الإثنين", 5, "اللغة العربية")) {
			t.Errorf("aliases and digits:\n%s", got)
		}

		raw := i1Book(t, i1Sheet{"Sheet1", i1Table(
			[][]any{{"3", "N", "الأحد", "1", "PLACEHOLDER", nil}},
			i1Rows("3", "N", map[string]int{"الأحد": 5}, "Science", "")[1:],
			[][]any{{"3.0", "T", "الأحد", "1", "Text grade", nil}},
			i1Rows("3.0", "T", map[string]int{"الأحد": 5}, "Text grade", "")[1:],
		)})
		raw = i1SetCellXML(t, raw, "A2", `<c r="A2"><v>3.0</v></c>`)
		raw = i1SetCellXML(t, raw, "D3", `<c r="D3"><v>2.0</v></c>`)
		raw = i1SetCellXML(t, raw, "E2", `<c r="E2" t="str"><f>"Sci"&amp;"ence"</f><v>Science</v></c>`)
		importOK(t, raw)
		if got := e1List(e1Decode(t, get("/api/admin/schedule?grade=3&section=N")).Data); got != e1List(e1Day("الأحد", 5, "Science")) {
			t.Errorf("numeric grade 3.0, numeric period 2.0 and a formula with a saved value:\n%s", got)
		}
		if got := len(e1Decode(t, get("/api/admin/schedule?grade=3.0&section=T")).Data); got != 5 {
			t.Errorf("a text grade 3.0 must be kept as written; class 3.0/T has %d periods", got)
		}
	})

	t.Run("problems are reported with sheet, row and column, and nothing is saved", func(t *testing.T) {
		before := i1Snapshot(t, db, true)
		rule := "a school day must have exactly 5 or 6 periods, numbered 1 to 5 or 1 to 6"
		sheet1 := i1Table(
			[][]any{
				{"E9", "أ", "الجمعة", "1", "Art", nil},
				{"E9", "أ", "الأحد", "7", "Art", nil},
				{"E9", "ب", "الأحد", "1", nil, "Teacher Example"},
				{"E9", "ب", "الأحد", "2", strings.Repeat("م", 101), nil},
			},
			i1Rows("E1", "أ", map[string]int{"الأحد": 4}, "Art", ""),
			[][]any{{"E2", "أ", "الأحد", "1", "Art", nil}, {"E2", "أ", "الأحد", "2", "Art", nil}, {"E2", "أ", "الأحد", "3", "Art", nil}, {"E2", "أ", "الأحد", "5", "Art", nil}, {"E2", "أ", "الأحد", "6", "Art", nil}},
			i1Rows("E3", "أ", map[string]int{"الأحد": 5}, "Art", ""),
			[][]any{{"", "أ", "الأحد", "1", "Art", nil}, {"E4", "أ", "الأحد", "1.5", "Art", strings.Repeat("م", 101)}},
		)
		sheet2 := [][]any{
			{"المعلم", "الحصة", "اليوم", "الشعبة", "المادة", "المرحلة"},
			{nil, "3", "الأحد", "أ", "Art", "E3"},
			{nil, "1", "الأحد", "أ", i1Formula(`"Ar"&"t"`), "E5"},
		}
		r := i1Import(t, srv, admin, i1Book(t, i1Sheet{"Sheet1", sheet1}, i1Sheet{"Second", sheet2}))
		rep := i1Decode(t, r, 400)
		want := []i1Problem{
			{"Sheet1", 2, "اليوم", "day must be a school day, Sunday to Thursday (Arabic or English)"},
			{"Sheet1", 3, "الحصة", "period must be a whole number from 1 to 6"},
			{"Sheet1", 4, "المادة", "subject is required when a teacher is given"},
			{"Sheet1", 5, "المادة", "subject must be at most 100 characters"},
			{"Sheet1", 6, "الحصة", "grade E1, section أ has periods 1, 2, 3, 4 on الأحد; " + rule},
			{"Sheet1", 10, "الحصة", "grade E2, section أ has periods 1, 2, 3, 5, 6 on الأحد; " + rule},
			{"Sheet1", 20, "المرحلة", "grade is required"},
			{"Sheet1", 21, "الحصة", "period must be a whole number from 1 to 6"},
			{"Sheet1", 21, "المعلم", "teacher must be at most 100 characters"},
			{"Second", 2, "الحصة", "period 3 on الأحد for grade E3, section أ is already in sheet Sheet1 row 17"},
			{"Second", 3, "المادة", "cell E3 has a formula without a saved value; type the value, or open and save the file in Excel"},
		}
		if rep.Status != "error" || rep.Message != fmt.Sprintf("The file has %d problems; nothing was saved", len(want)) {
			t.Errorf("envelope %q %q", rep.Status, rep.Message)
		}
		if fmt.Sprint(rep.Errors) != fmt.Sprint(want) {
			t.Errorf("problems\n got: %v\nwant: %v", rep.Errors, want)
		}

		missing := i1Book(t, i1Sheet{"Sheet1", [][]any{{"Title"}, {"المرحلة", "الشعبة", "اليوم", "الحصة", "المادة", "ملاحظات"}, {"G1", "أ", "الأحد", "1", "Art"}}})
		rep = i1Decode(t, i1Import(t, srv, admin, missing), 400)
		if fmt.Sprint(rep.Errors) != fmt.Sprint([]i1Problem{{"Sheet1", 2, "المعلم", "the header row has no column المعلم"}}) {
			t.Errorf("missing header column: %v", rep.Errors)
		}
		twice := i1Book(t, i1Sheet{"Sheet1", [][]any{{"المرحلة", "الصف", "الشعبة", "اليوم", "الحصة", "المادة", "المعلم"}}})
		rep = i1Decode(t, i1Import(t, srv, admin, twice), 400)
		if fmt.Sprint(rep.Errors) != fmt.Sprint([]i1Problem{{"Sheet1", 1, "المرحلة", "the header row has the column المرحلة more than once"}}) {
			t.Errorf("repeated header column: %v", rep.Errors)
		}

		var many [][]any
		for i := 0; i < 60; i++ {
			many = append(many, []any{"G1", "أ", "Friday", "1", "Art", nil})
		}
		r = i1Import(t, srv, admin, i1Book(t, i1Sheet{"Sheet1", i1Table(many)}))
		rep = i1Decode(t, r, 400)
		if len(rep.Errors) != 51 || rep.Errors[49].Row != 51 || rep.Message != "The file has 60 problems; nothing was saved" {
			t.Errorf("capped list: %d items, message %q", len(rep.Errors), rep.Message)
		}
		var lastItem struct {
			Errors []map[string]any `json:"errors"`
		}
		json.Unmarshal(r.body, &lastItem)
		if last := lastItem.Errors[len(lastItem.Errors)-1]; len(last) != 1 || last["message"] != "and 10 more problems" {
			t.Errorf("last item %v, want only the message \"and 10 more problems\"", last)
		}

		single := i1Book(t, i1Sheet{"Sheet1", i1Table(i1Rows("G1", "أ", map[string]int{"الأحد": 6}, "Changed", ""), [][]any{{"G1", "أ", "Saturday", "1", "Art", nil}})})
		i1Decode(t, i1Import(t, srv, admin, single), 400)
		if after := i1Snapshot(t, db, true); after != before {
			t.Errorf("rejected imports changed the table\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	t.Run("only workbooks are read", func(t *testing.T) {
		before := i1Snapshot(t, db, true)
		var plainZip bytes.Buffer
		zw := zip.NewWriter(&plainZip)
		w, _ := zw.Create("notes.txt")
		w.Write([]byte("not a workbook"))
		zw.Close()
		var bomb bytes.Buffer
		zw = zip.NewWriter(&bomb)
		w, _ = zw.Create("xl/worksheets/sheet1.xml")
		w.Write(make([]byte, 21<<20))
		zw.Close()
		var liar bytes.Buffer
		zw = zip.NewWriter(&liar)
		w, err := zw.CreateRaw(&zip.FileHeader{Name: "xl/worksheets/sheet1.xml", Method: zip.Store, CompressedSize64: 5, UncompressedSize64: 1 << 40})
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte("<x/>\n"))
		zw.Close()
		truncated := i1Book(t, i1Sheet{"Sheet1", i1Table(i1Rows("G1", "أ", map[string]int{"الأحد": 5}, "X", ""))})
		for _, tc := range []struct {
			name string
			data []byte
			msg  string
		}{
			{"a text file named .xlsx", []byte("grade,section\nG1,أ\n"), "file must be an Excel workbook (.xlsx)"},
			{"an empty file", nil, "file is empty"},
			{"a zip that is not a workbook", plainZip.Bytes(), "file must be an Excel workbook (.xlsx)"},
			{"an old .xls (OLE) file", append([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, make([]byte, 512)...), "file must be an Excel workbook (.xlsx)"},
			{"a truncated workbook", truncated[:len(truncated)/2], "file must be an Excel workbook (.xlsx)"},
			{"a zip unpacking to more than 20 MiB", bomb.Bytes(), "file must unpack to at most 20 MiB"},
			{"a tiny zip declaring 1 TiB", liar.Bytes(), "file must unpack to at most 20 MiB"},
			{"no header row", i1Book(t, i1Sheet{"Sheet1", [][]any{{"Grade"}, {"المرحلة", "الشعبة"}}}), "No worksheet has the header row (المرحلة, الشعبة, اليوم, الحصة, المادة, المعلم) in its first 10 rows"},
			{"the header below row 10", i1Book(t, i1Sheet{"Sheet1", append(make([][]any, 10), i1Header)}), "No worksheet has the header row (المرحلة, الشعبة, اليوم, الحصة, المادة, المعلم) in its first 10 rows"},
		} {
			start := time.Now()
			e1Error(t, i1Import(t, srv, admin, tc.data), 400, tc.msg)
			if took := time.Since(start); took > 10*time.Second {
				t.Errorf("%s took %v", tc.name, took)
			}
		}
		if r := get("/api/admin/schedule/classes"); r.status != 200 {
			t.Errorf("the server is not answering after the zip checks: %d", r.status)
		}

		ct, body := f1Form(f1Text("dry_run", "true"))
		e1Error(t, f1Do(t, srv, "POST", "/api/admin/schedule/import", a14Bearer(admin), ct, body), 400, "file is required")
		valid := i1Book(t, i1Sheet{"Sheet1", i1Table(i1Rows("G1", "أ", map[string]int{"الأحد": 5}, "X", ""))})
		for _, v := range []string{"yes", "TRUE", "", "true ", "1"} {
			e1Error(t, i1Import(t, srv, admin, valid, f1Text("dry_run", v)), 400, `dry_run must be "true" or "false"`)
		}
		e1Error(t, i1Import(t, srv, admin, valid, f1Text("dry_run", strings.Repeat("t", 100))), 400, `dry_run must be "true" or "false"`)
		e1Error(t, i1Import(t, srv, admin, valid, f1Part{"file", "again.xlsx", "", valid}), 400, "file must be sent once")
		e1Error(t, a14Do(t, srv, "POST", "/api/admin/schedule/import", a14Bearer(admin), map[string]any{"file": "x"}), 400, "Request must be multipart/form-data")

		marker := len(srv.out.String())
		e1Error(t, i1Import(t, srv, admin, make([]byte, handlers.MaxScheduleFileBytes+1)), 413, "file must be at most 2 MiB (2097152 bytes)")
		logged := false
		for _, line := range strings.Split(srv.out.String()[marker:], "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "Request body too large" && m["level"] == "WARN" && m["limit_bytes"] == float64(handlers.MaxScheduleFileBytes) && m["path"] == "/api/admin/schedule/import" {
				logged = true
			}
		}
		if !logged {
			t.Errorf("the 413 was not logged as WARN Request body too large")
		}
		ct, body = f1Form(f1Part{"file", "s.xlsx", "", valid}, f1Text("padding", strings.Repeat("p", int(handlers.MaxScheduleImportBodyBytes))))
		e1Error(t, f1Do(t, srv, "POST", "/api/admin/schedule/import", a14Bearer(admin), ct, body), 413, "Request body too large")
		if after := i1Snapshot(t, db, true); after != before {
			t.Errorf("rejected files changed the table")
		}

		exact := i1PadTo(t, i1Book(t, i1Sheet{"Sheet1", i1Table(i1Rows("P2", "M", map[string]int{"الخميس": 5}, "Padded", ""))}), int(handlers.MaxScheduleFileBytes))
		if rep := importOK(t, exact, f1Text("dry_run", "false")); rep.TotalPeriods != 5 {
			t.Errorf("a workbook of exactly 2 MiB: %+v", rep)
		}
	})

	t.Run("a dry run reports the same and writes nothing", func(t *testing.T) {
		book := i1Book(t, i1Sheet{"Sheet1", i1Table(
			i1Rows("G1", "أ", map[string]int{"الأحد": 5, "الخميس": 6}, "الرياضيات", "معلم تجريبي 3"),
			i1Rows("Z9", "ب", map[string]int{"الإثنين": 5}, "الرسم", ""),
		)})
		before := i1Snapshot(t, db, true)
		dry := importOK(t, book, f1Text("dry_run", "true"))
		if after := i1Snapshot(t, db, true); after != before {
			t.Fatalf("a dry run changed the table\nbefore:\n%s\nafter:\n%s", before, after)
		}
		real := importOK(t, book, f1Text("dry_run", "false"))
		if !dry.DryRun || real.DryRun || dry.Message != "Dry run: the file is valid; nothing was saved" || real.Message != "Schedule imported" {
			t.Errorf("dry_run flags or messages: %+v / %+v", dry, real)
		}
		dry.DryRun, dry.Message, real.DryRun, real.Message = false, "", false, ""
		if fmt.Sprintf("%+v", dry) != fmt.Sprintf("%+v", real) {
			t.Errorf("the dry run reported\n%+v\nthe import reported\n%+v", dry, real)
		}
		if fmt.Sprintf("%+v", real.Classes) != "[{Grade:G1 Section:أ Periods:11 MatchedStudents:2} {Grade:Z9 Section:ب Periods:5 MatchedStudents:0}]" {
			t.Errorf("classes %+v", real.Classes)
		}
		if fmt.Sprint(real.Warnings) != "[grade Z9, section ب: no active student has this grade and section, parents will not see it]" {
			t.Errorf("warnings %q", real.Warnings)
		}
		if real.TotalPeriods != 16 {
			t.Errorf("total_periods %d", real.TotalPeriods)
		}
	})

	t.Run("after an import the parent sees the imported schedule in the usual order", func(t *testing.T) {
		book := i1Book(t, i1Sheet{"Sheet1", i1Table(
			i1Rows("G2", "ب", map[string]int{"الخميس": 5}, "التربية الإسلامية", ""),
			i1Rows("G2", "ب", map[string]int{"الأحد": 6}, "اللغة الإنكليزية", "معلمة تجريبية 4"),
		)})
		importOK(t, book)
		var resp struct {
			Data []struct {
				StudentID  int    `json:"student_id"`
				Day        string `json:"day_of_week"`
				Period     int    `json:"period_number"`
				Subject    string `json:"subject_name"`
				Teacher    string `json:"teacher_name"`
				SubjectKey string `json:"subject_key"`
			} `json:"data"`
		}
		json.Unmarshal(parentSchedule(t), &resp)
		var got []string
		for _, e := range resp.Data {
			if e.StudentID == secondChild {
				got = append(got, fmt.Sprintf("%s|%d|%s|%s|%s", e.Day, e.Period, e.Subject, e.Teacher, e.SubjectKey))
			}
		}
		var want []string
		for p := 1; p <= 6; p++ {
			want = append(want, fmt.Sprintf("الأحد|%d|اللغة الإنكليزية|معلمة تجريبية 4|english", p))
		}
		for p := 1; p <= 5; p++ {
			want = append(want, fmt.Sprintf("الخميس|%d|التربية الإسلامية||islamic", p))
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("parent view of G2/ب\n got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if len(resp.Data) == 0 || resp.Data[0].StudentID != firstChild {
			t.Errorf("children are not in id order")
		}
	})

	t.Run("a database failure saves nothing", func(t *testing.T) {
		before := i1Snapshot(t, db, true)
		if _, err := db.Exec(`ALTER TABLE weekly_schedules ADD CONSTRAINT i1_refuse_poison CHECK (subject_name <> 'Poison')`); err != nil {
			t.Fatal(err)
		}
		defer db.Exec(`ALTER TABLE weekly_schedules DROP CONSTRAINT i1_refuse_poison`)
		book := i1Book(t, i1Sheet{"Sheet1", i1Table(
			i1Rows("G1", "أ", map[string]int{"الأحد": 5}, "Fine", ""),
			i1Rows("ZZ", "ز", map[string]int{"الأحد": 5}, "Poison", ""),
		)})
		for _, dry := range []string{"true", "false"} {
			e1Error(t, i1Import(t, srv, admin, book, f1Text("dry_run", dry)), 500, "Failed to import schedule")
		}
		if after := i1Snapshot(t, db, true); after != before {
			t.Errorf("a failed import changed the table\nbefore:\n%s\nafter:\n%s", before, after)
		}
		if srv.out.index("AdminScheduleImportHandler: import failed") < 0 {
			t.Errorf("the 500 was not logged")
		}
	})

	t.Run("concurrent imports and PUTs never mix or fail", func(t *testing.T) {
		alpha := i1Book(t, i1Sheet{"Sheet1", i1Table(i1Rows("K1", "أ", map[string]int{"الأحد": 5, "الإثنين": 6}, "Alpha", ""))})
		gamma := i1Book(t, i1Sheet{"Sheet1", i1Table(i1Rows("K1", "أ", map[string]int{"الخميس": 6}, "Gamma", ""))})
		betaBody, _ := json.Marshal(e1Body("K1", "أ", e1Join(e1Day("الأحد", 6, "Beta"), e1Day("الثلاثاء", 5, "Beta"))...))
		want := map[string]bool{
			e1List(e1Join(e1Day("الأحد", 5, "Alpha"), e1Day("الإثنين", 6, "Alpha"))): true,
			e1List(e1Day("الخميس", 6, "Gamma")):                                      true,
			e1List(e1Join(e1Day("الأحد", 6, "Beta"), e1Day("الثلاثاء", 5, "Beta"))):  true,
		}
		for round := 0; round < 10; round++ {
			var wg sync.WaitGroup
			start := make(chan struct{})
			results := make([]string, 3)
			for i, send := range []func() (int, []byte, error){
				func() (int, []byte, error) {
					ct, body := f1Form(f1Part{"file", "a.xlsx", "", alpha})
					return i1Send(srv, "POST", "/api/admin/schedule/import", admin, ct, body)
				},
				func() (int, []byte, error) {
					ct, body := f1Form(f1Part{"file", "g.xlsx", "", gamma})
					return i1Send(srv, "POST", "/api/admin/schedule/import", admin, ct, body)
				},
				func() (int, []byte, error) {
					return i1Send(srv, "PUT", "/api/admin/schedule", admin, "application/json", betaBody)
				},
			} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					status, body, err := send()
					if err != nil || status != 200 {
						results[i] = fmt.Sprintf("%d %s %v", status, body, err)
					}
				}()
			}
			close(start)
			wg.Wait()
			imports += 2
			for i, res := range results {
				if res != "" {
					t.Fatalf("round %d request %d: %s", round, i, res)
				}
			}
			if got := e1List(e1Decode(t, get("/api/admin/schedule?grade=K1&section="+url.QueryEscape("أ"))).Data); !want[got] {
				t.Fatalf("round %d: the final schedule is a mix:\n%s", round, got)
			}
		}
	})

	t.Run("each import is logged with dry_run, classes and total_periods only", func(t *testing.T) {
		var lines []map[string]any
		for _, raw := range strings.Split(srv.out.String(), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(raw), &m) == nil && m["msg"] == "Schedule imported" {
				lines = append(lines, m)
			}
		}
		if len(lines) != imports {
			t.Errorf("%d \"Schedule imported\" lines for %d successful imports", len(lines), imports)
		}
		for _, m := range lines {
			var keys []string
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if strings.Join(keys, ",") != "classes,dry_run,level,msg,time,total_periods" || m["level"] != "INFO" {
				t.Errorf("log line %v", m)
			}
		}
		for _, secret := range []string{"Teacher Example", "معلمة تجريبية", "Padded", "Alpha"} {
			if strings.Contains(srv.out.String(), secret) {
				t.Errorf("the log contains cell content %q", secret)
			}
		}
	})

	t.Run("other routes keep their body limits", func(t *testing.T) {
		big := `{"grade":"G1","section":"A","pad":"` + strings.Repeat("a", int(handlers.MaxJSONBodyBytes)) + `"}`
		e1Error(t, put(big), 413, "Request body too large")
		ct, body := f1Form(f1Image(f1Padded(t, int(handlers.MaxBannerImageBytes)+1)))
		e1Error(t, f1Do(t, srv, "POST", "/api/admin/banners", a14Bearer(admin), ct, body), 413, fmt.Sprintf("image must be at most 2 MiB (%d bytes)", handlers.MaxBannerImageBytes))
	})

	t.Run("authentication", func(t *testing.T) {
		later, earlier := time.Now().Add(time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
		expired := e1Sign(t, jwt.MapClaims{"username": "admin", "role": "admin", "sv": 0, "exp": earlier})
		parentToken := e1Sign(t, jwt.MapClaims{"parent_id": 1, "phone": parentPhone, "role": "parent", "sv": 0, "exp": later})
		book := i1Book(t, i1Sheet{"Sheet1", i1Table(i1Rows("AU", "A", map[string]int{"الأحد": 5}, "Auth", ""))})
		ct, body := f1Form(f1Part{"file", "s.xlsx", "", book})
		call := func(method, path string, header map[string]string) a14Response {
			if method == "POST" {
				return f1Do(t, srv, method, path, header, ct, body)
			}
			return a14Do(t, srv, method, path, header, nil)
		}
		routes := []struct{ method, path string }{
			{"GET", "/api/admin/schedule/classes"}, {"GET", "/api/admin/schedule/export?grade=G1&section=" + url.QueryEscape("أ")}, {"POST", "/api/admin/schedule/import"},
		}
		before := i1Snapshot(t, db, true)
		for _, rt := range routes {
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
				{"parent token", a14Bearer(parentToken), 403, "Forbidden"},
			} {
				r := call(rt.method, rt.path, tc.header)
				if r.status != tc.status || r.json(t)["message"] != tc.msg {
					t.Errorf("%s %s with %s: %d %s, want %d %s", rt.method, rt.path, tc.name, r.status, r.body, tc.status, tc.msg)
				}
			}
		}
		script, err := os.ReadFile(filepath.Join("..", "..", "db", "scripts", "rotate_admin_password.sql"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(strings.ReplaceAll(string(script), "NEW_PASSWORD_HERE", "i1-rotated-password")); err != nil {
			t.Fatalf("rotation script: %v", err)
		}
		for _, rt := range routes {
			if r := call(rt.method, rt.path, a14Bearer(admin)); r.status != 401 {
				t.Errorf("%s %s with a token from before the rotation: %d %s", rt.method, rt.path, r.status, r.body)
			}
		}
		if after := i1Snapshot(t, db, true); after != before {
			t.Errorf("rejected tokens changed the table")
		}
		for _, tc := range []struct{ method, path, allow string }{
			{"POST", "/api/admin/schedule/classes", "GET, HEAD"},
			{"DELETE", "/api/admin/schedule/export", "GET, HEAD"},
			{"GET", "/api/admin/schedule/import", "POST"},
		} {
			if r := a14Do(t, srv, tc.method, tc.path, nil, nil); r.status != 405 || r.header.Get("Allow") != tc.allow {
				t.Errorf("%s %s: %d Allow=%q", tc.method, tc.path, r.status, r.header.Get("Allow"))
			}
		}
	})
}

// TestI1FullSchoolRoundTrip checks the row limit counts only filled rows: a file of 100 classes in
// the export's grid layout (3000 rows, 1100 of them filled) imports unchanged, and 2001 filled rows
// or 101 classes are refused.
func TestI1FullSchoolRoundTrip(t *testing.T) {
	srv, db := a14Server(t, "i1full")
	admin := a14AdminToken(t, srv)
	var values []string
	rows := [][]any{i1Header}
	for c := 1; c <= 100; c++ {
		grade := fmt.Sprintf("C%03d", c)
		for d, day := range []string{"الأحد", "الإثنين", "الثلاثاء", "الأربعاء", "الخميس"} {
			for p := 1; p <= 6; p++ {
				row := []any{grade, "أ", day, fmt.Sprint(p), nil, nil}
				if (d == 0 && p <= 5) || d == 1 {
					values = append(values, fmt.Sprintf("('%s', 'أ', '%s', %d, 'الرياضيات', 'معلم تجريبي')", grade, day, p))
					row[4], row[5] = "الرياضيات", "معلم تجريبي"
				}
				rows = append(rows, row)
			}
		}
	}
	if _, err := db.Exec(`INSERT INTO weekly_schedules (grade, section, day_of_week, period_number, subject_name, teacher_name) VALUES ` + strings.Join(values, ",")); err != nil {
		t.Fatal(err)
	}
	before := i1Snapshot(t, db, false)
	if len(rows) != 3001 {
		t.Fatalf("the file has %d data rows, want 3000", len(rows)-1)
	}
	file := i1Book(t, i1Sheet{"Sheet1", rows})
	for _, dry := range []string{"true", "false"} {
		rep := i1Decode(t, i1Import(t, srv, admin, file, f1Text("dry_run", dry)), 200)
		if rep.TotalPeriods != 1100 || len(rep.Classes) != 100 || len(rep.Warnings) != 100 {
			t.Errorf("dry_run=%s: %d periods, %d classes, %d warnings", dry, rep.TotalPeriods, len(rep.Classes), len(rep.Warnings))
		}
	}
	if after := i1Snapshot(t, db, false); after != before {
		t.Errorf("the full-school round trip changed the schedules")
	}

	var many [][]any
	for c := 1; c <= 400; c++ {
		many = append(many, i1Rows(fmt.Sprintf("D%03d", c), "أ", map[string]int{"الأحد": 5}, "Art", "")...)
	}
	many = append(many, []any{"D999", "أ", "الأحد", "1", "Art", nil})
	e1Error(t, i1Import(t, srv, admin, i1Book(t, i1Sheet{"Sheet1", i1Table(many)})), 400, "The file has more than 2000 schedule rows")
	e1Error(t, i1Import(t, srv, admin, i1Book(t, i1Sheet{"Sheet1", i1Table(many[:2000])})), 400, "The file has 400 classes; at most 100 can be imported at once")
	if rep := i1Decode(t, i1Import(t, srv, admin, i1Book(t, i1Sheet{"Sheet1", i1Table(many[:500])}), f1Text("dry_run", "true")), 200); len(rep.Classes) != 100 {
		t.Errorf("100 classes: %d", len(rep.Classes))
	}
	e1Error(t, i1Import(t, srv, admin, i1Book(t, i1Sheet{"Sheet1", i1Table(many[:505])})), 400, "The file has 101 classes; at most 100 can be imported at once")
	if after := i1Snapshot(t, db, false); after != before {
		t.Errorf("refused imports changed the schedules")
	}
}

func dayIndex(day string) int {
	for i, d := range []string{"الأحد", "الإثنين", "الثلاثاء", "الأربعاء", "الخميس"} {
		if d == day {
			return i
		}
	}
	return -1
}
