package contract

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"future_kids/internal/testdb"
	"future_kids/internal/tz"

	"golang.org/x/crypto/bcrypt"
)

const (
	adminPassword = "contract-admin-password"
	phone1        = "+9647000000101"
	phone2        = "+9647000000202"
	phone3        = "+9647000000303"
	unknownPhone  = "+9647000000404"
	limitedPhone  = "+9647000000505"
)

type exchange struct {
	name, op, target string
	status           int
	header           http.Header
	body             []byte
	parts            []string
}

// formPart is one part of a multipart/form-data request; multipartBody is sent as one.
type formPart struct {
	name, filename, contentType string
	data                        []byte
}

type multipartBody []formPart

func (m multipartBody) encode() (string, []byte, []string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	names := []string{}
	for _, p := range m {
		h := textproto.MIMEHeader{}
		if p.filename != "" {
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, p.name, p.filename))
		} else {
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"`, p.name))
		}
		if p.contentType != "" {
			h.Set("Content-Type", p.contentType)
		}
		w, _ := mw.CreatePart(h)
		w.Write(p.data)
		names = append(names, p.name)
	}
	mw.Close()
	return mw.FormDataContentType(), buf.Bytes(), names
}

type runner struct {
	t        *testing.T
	srv      *apiServer
	db       *sql.DB
	xs       []*exchange
	tx       []*exchange
	admin    string
	parent   string
	parentID int
}

// TestOpenAPIContract runs the real API against a throwaway database and checks
// future_kids_api.yaml against it: every registered route is a spec operation and vice versa,
// every response (status, Content-Type, headers, JSON shape, types, nullability, required
// fields) matches its documented schema, and every operation has a success and an error case.
// The self-test proves the checks fail on deliberately broken copies of the spec.
func TestOpenAPIContract(t *testing.T) {
	specPath := filepath.Join("..", "..", contractSpec)
	if p := os.Getenv("CONTRACT_SPEC"); p != "" {
		specPath = p
	}
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	s, err := parseSpec(string(raw))
	if err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}

	db, dsn := testdb.New(t, "contract")
	seedFixtures(t, db)
	r := &runner{t: t, srv: startAPI(t, dsn), db: db}
	r.scenario()

	routes, err := mainRoutes(filepath.Join("..", "..", "cmd", "api", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	served := r.probe(routes)
	r.databaseFailures()

	t.Run("spec is well-formed", func(t *testing.T) { report(t, hygieneProblems(s)) })
	t.Run("every route is documented and every operation is routed", func(t *testing.T) {
		report(t, routeProblems(s, served))
	})
	t.Run("every response matches the spec", func(t *testing.T) { report(t, exchangeProblems(s, r.xs)) })
	t.Run("every accepted multipart request matches the spec", func(t *testing.T) { report(t, requestProblems(s, r.xs)) })
	t.Run("every operation has a success and an error case", func(t *testing.T) {
		report(t, coverageProblems(s, r.xs))
		if u := untestedStatuses(s, r.xs); len(u) > 0 {
			t.Logf("documented but not triggered live (need a database failure): %s", strings.Join(u, ", "))
		}
	})
	t.Run("documented transport behaviour", r.transport)
	t.Run("transport responses match the spec", func(t *testing.T) { report(t, transportProblems(s, r.tx)) })
	t.Run("self-test: broken specs fail", func(t *testing.T) { selfTest(t, raw, s, served, r.xs, r.tx) })
	t.Logf("%d exchanges over %d operations", len(r.xs), len(s.operations()))
}

func report(t *testing.T, problems []string) {
	t.Helper()
	for _, p := range problems {
		t.Error(p)
	}
}

func seedFixtures(t *testing.T, db *sql.DB) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []struct {
		q    string
		args []any
	}{
		{`UPDATE admins SET password_hash = $1 WHERE username = 'admin'`, []any{string(hash)}},
		{`INSERT INTO weekly_schedules (grade, section, day_of_week, period_number, subject_name, teacher_name) VALUES
			('G3', 'A', 'الخميس', 1, 'Art', NULL),
			('G3', 'A', 'Holiday', 1, 'Trip', NULL),
			('G3', 'A', 'الإثنين', 2, 'Science', 'Teacher Example'),
			('G3', 'A', 'الأحد', 2, 'Reading', NULL),
			('G3', 'A', 'Tuesday', 1, 'Music', NULL),
			('G3', 'A', 'الاحد', 1, 'Mathematics', 'Teacher Example'),
			('G1', 'B', 'السبت', 1, 'Club', NULL),
			('G1', 'B', 'Wednesday', 3, 'History', NULL),
			('G1', 'B', 'sunday', 1, 'Mathematics', NULL)`, nil},
		{`INSERT INTO banners (title, image_url, action_link, is_active, created_at) VALUES
			('Open day', 'https://example.com/banners/open-day.jpg', NULL, true, '2026-01-02 10:00'),
			(NULL, 'https://example.com/banners/sports.jpg', 'https://example.com/sports', true, '2026-01-03 10:00'),
			('Old news', 'https://example.com/banners/old.jpg', NULL, false, '2026-01-04 10:00')`, nil},
	}
	for _, st := range stmts {
		if _, err := db.Exec(st.q, st.args...); err != nil {
			t.Fatalf("fixture failed: %v\n%s", err, st.q)
		}
	}
}

func authHeader(bearer string) map[string]string {
	if bearer == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + bearer}
}

// call sends one request for the operation op ("METHOD /path") with the given query string.
func (r *runner) call(op, name, query, bearer string, body any, want int) (*exchange, any) {
	r.t.Helper()
	return r.callWith(op, name, query, authHeader(bearer), body, want)
}

func (r *runner) callWith(op, name, query string, header map[string]string, body any, want int) (*exchange, any) {
	t := r.t
	t.Helper()
	method, path, _ := strings.Cut(op, " ")
	h := http.Header{}
	var raw []byte
	var parts []string
	switch b := body.(type) {
	case nil:
	case string:
		raw = []byte(b)
	case []byte:
		raw = b
	case multipartBody:
		var ct string
		ct, raw, parts = b.encode()
		h.Set("Content-Type", ct)
	default:
		var err error
		if raw, err = json.Marshal(b); err != nil {
			t.Fatal(err)
		}
	}
	if raw != nil && parts == nil {
		if strings.HasPrefix(path, "/iclock/") || path == "/api/attendance/push" {
			h.Set("Content-Type", "text/plain")
		} else {
			h.Set("Content-Type", "application/json")
		}
	}
	for k, v := range header {
		h.Set(k, v)
	}
	status, respHeader, respBody, err := r.srv.send(method, path+query, h, raw)
	if err != nil {
		t.Fatalf("%s [%s]: %v", op, name, err)
	}
	x := &exchange{name: name, op: op, target: path + query, status: status, header: respHeader, body: respBody, parts: parts}
	r.xs = append(r.xs, x)
	if status != want {
		t.Errorf("%s%s [%s]: status %d, want %d: %.300s", op, query, name, status, want, respBody)
	}
	var data any
	if mt, _, _ := mime.ParseMediaType(respHeader.Get("Content-Type")); mt == "application/json" {
		v, err := decodeJSON(respBody)
		if err != nil {
			t.Errorf("%s [%s]: response is not JSON: %v", op, name, err)
		}
		data = v
	}
	return x, data
}

func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data after the JSON value")
	}
	return v, nil
}

func at(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			v = obj(v)[k]
		case int:
			l := list(v)
			if k >= len(l) {
				return nil
			}
			v = l[k]
		}
	}
	return v
}

func toInt(v any) int {
	n, _ := v.(json.Number)
	i, _ := strconv.Atoi(string(n))
	return i
}

func (r *runner) expect(what string, got, want any) {
	r.t.Helper()
	if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
		r.t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

func punch(deviceSN, rfid, at string) map[string]string {
	return map[string]string{"device_sn": deviceSN, "rfid_tag": rfid, "push_time": at}
}

func oversized() string { return `{"pad":"` + strings.Repeat("a", 70<<10) + `"}` }

func contractPicture(seed int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 12, 6))
	for x := 0; x < 12; x++ {
		for y := 0; y < 6; y++ {
			img.Set(x, y, color.RGBA{uint8(seed * 41), uint8(x * 20), uint8(y * 40), 255})
		}
	}
	return img
}

func contractJPEG(t *testing.T, seed int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, contractPicture(seed), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func contractPNG(t *testing.T, seed int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, contractPicture(seed)); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func contractGIF(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := gif.Encode(&buf, contractPicture(1), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// contractWebP is a RIFF/WEBP container with filler bytes: what content sniffing recognises.
func contractWebP() []byte {
	payload := bytes.Repeat([]byte{0x2f, 0x01}, 24)
	chunk := append(append([]byte("VP8L"), binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))...), payload...)
	body := append([]byte("WEBP"), chunk...)
	return append(append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...), body...)
}

func weekdayRank(day string) int {
	d := strings.NewReplacer("أ", "ا", "إ", "ا", "آ", "ا").Replace(strings.ToLower(strings.TrimSpace(day)))
	for i, names := range [][2]string{
		{"sunday", "الاحد"}, {"monday", "الاثنين"}, {"tuesday", "الثلاثاء"}, {"wednesday", "الاربعاء"},
		{"thursday", "الخميس"}, {"friday", "الجمعة"}, {"saturday", "السبت"},
	} {
		if d == names[0] || d == names[1] {
			return i + 1
		}
	}
	return 8
}

// schoolDays lists the Sunday–Thursday dates of today's month up to today, newest first.
func schoolDays(today time.Time) []string {
	var days []string
	for d := today; d.Month() == today.Month(); d = d.AddDate(0, 0, -1) {
		if d.Weekday() != time.Friday && d.Weekday() != time.Saturday {
			days = append(days, d.Format("2006-01-02"))
		}
	}
	return days
}

func (r *runner) scenario() {
	t := r.t
	now := tz.Now()
	today := now.Format("2006-01-02")
	month := now.Format("2006-01")
	later, earlier := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	someParent := signToken(t, claims("parent", 999999, later), contractSecret)
	expiredAdmin := signToken(t, claims("admin", nil, earlier), contractSecret)

	adminDenied := func(op, query string, body any) {
		t.Helper()
		r.call(op, "no token", query, "", body, 401)
		r.call(op, "expired admin token", query, expiredAdmin, body, 401)
		r.call(op, "parent token", query, someParent, body, 403)
	}

	// ── Admin login ────────────────────────────────────────────────────────────
	_, d := r.call("POST /api/admin/login", "valid credentials", "", "", map[string]string{"username": "admin", "password": adminPassword}, 200)
	r.admin = str(at(d, "data", "token"))
	if r.admin == "" {
		t.Fatalf("admin login returned no token")
	}
	r.call("POST /api/admin/login", "wrong password", "", "", map[string]string{"username": "admin", "password": "wrong-password"}, 401)
	r.call("POST /api/admin/login", "unknown user", "", "", map[string]string{"username": "nobody", "password": adminPassword}, 401)
	r.call("POST /api/admin/login", "malformed JSON", "", "", "{", 400)
	r.call("POST /api/admin/login", "oversized body", "", "", oversized(), 413)
	admin := r.admin

	// ── Devices ────────────────────────────────────────────────────────────────
	r.call("POST /api/admin/devices", "active device", "", admin, map[string]any{"serial_number": "TEST-SN-0001", "location_name": "Main Gate", "is_active": true}, 200)
	r.call("POST /api/admin/devices", "disabled device", "", admin, map[string]any{"serial_number": "TEST-SN-0002", "is_active": false}, 200)
	r.call("POST /api/admin/devices", "is_active omitted means active", "", admin, map[string]any{"serial_number": "TEST-SN-0004", "location_name": "Annex"}, 200)
	r.call("POST /api/admin/devices", "location_name too long", "", admin, map[string]any{"serial_number": "TEST-SN-0005", "location_name": strings.Repeat("L", 51)}, 400)
	r.call("POST /api/admin/devices", "spare device", "", admin, map[string]any{"serial_number": "TEST-SN-0003", "location_name": "Side Gate", "is_active": true}, 200)
	r.call("POST /api/admin/devices", "duplicate serial", "", admin, map[string]any{"serial_number": "TEST-SN-0001"}, 409)
	r.call("POST /api/admin/devices", "missing serial", "", admin, map[string]any{"location_name": "Nowhere"}, 400)
	r.call("POST /api/admin/devices", "malformed JSON", "", admin, "{", 400)
	r.call("POST /api/admin/devices", "oversized body", "", admin, oversized(), 413)
	adminDenied("POST /api/admin/devices", "", map[string]any{"serial_number": "TEST-SN-0009"})

	// ── Students ───────────────────────────────────────────────────────────────
	create := func(name string, body map[string]any) (int, any) {
		t.Helper()
		_, d := r.call("POST /api/admin/students", name, "", admin, body, 200)
		id := toInt(at(d, "data", "id"))
		if id == 0 {
			t.Fatalf("create student (%s) returned no id: %v", name, d)
		}
		return id, d
	}
	idA, d := create("every field", map[string]any{"name": "Sara Example", "parent_name": "Omar Example", "parent_phone": phone1, "parent_pin": "482193", "rfid_tag": "9001", "grade": "G3", "section": "A"})
	r.expect("created student echoes grade", at(d, "data", "grade"), "G3")
	idB, d := create("required fields only", map[string]any{"name": "Yousef Example", "parent_name": "Omar Example", "parent_phone": phone1})
	r.expect("omitted grade is null", at(d, "data", "grade"), nil)
	if tag := str(at(d, "data", "rfid_tag")); !strings.HasPrefix(tag, "admin-") {
		t.Errorf("omitted rfid_tag: got %q, want an admin-<number> placeholder", tag)
	}
	idC, _ := create("second child", map[string]any{"name": "Maryam Example", "parent_name": "Omar Example", "parent_phone": phone1, "rfid_tag": "9003", "grade": "G1", "section": "B"})
	idD, _ := create("other parent", map[string]any{"name": "Hadi Example", "parent_name": "Layla Example", "parent_phone": phone2, "parent_pin": "593047", "rfid_tag": "9004", "grade": "G3", "section": "A"})
	idE, _ := create("to deactivate", map[string]any{"name": "Temp Example", "parent_name": "Nour Example", "parent_phone": phone3, "parent_pin": "604158", "rfid_tag": "9005"})
	if _, err := r.db.Exec(`UPDATE students SET created_at = '2026-01-01 08:00' WHERE id IN ($1, $2, $3)`, idA, idB, idC); err != nil {
		t.Fatal(err)
	}
	r.call("POST /api/admin/students", "missing name", "", admin, map[string]any{"parent_name": "Omar Example", "parent_phone": phone1}, 400)
	r.call("POST /api/admin/students", "blank parent_phone", "", admin, map[string]any{"name": "Blank Example", "parent_name": "Omar Example", "parent_phone": "   "}, 400)
	_, d = r.call("POST /api/admin/students", "parent_phone not an Iraqi mobile number", "", admin, map[string]any{"name": "Bad Example", "parent_name": "Omar Example", "parent_phone": "+15551234567"}, 400)
	r.expect("invalid phone message", at(d, "message"), "parent_phone must be an Iraqi mobile number, for example 07XXXXXXXXX or +9647XXXXXXXXX")
	r.call("POST /api/admin/students", "malformed JSON", "", admin, "{", 400)
	r.call("POST /api/admin/students", "duplicate rfid_tag", "", admin, map[string]any{"name": "Dup Example", "parent_name": "Omar Example", "parent_phone": phone1, "rfid_tag": "9001"}, 409)
	r.call("POST /api/admin/students", "new parent without parent_pin", "", admin, map[string]any{"name": "New Example", "parent_name": "New Parent", "parent_phone": "+9647000000606", "rfid_tag": "9006"}, 400)
	r.call("POST /api/admin/students", "new parent with a blank parent_pin", "", admin, map[string]any{"name": "New Example", "parent_name": "New Parent", "parent_phone": "+9647000000606", "parent_pin": "   ", "rfid_tag": "9006"}, 400)
	for _, bad := range []string{"48219", "4821937", "48a193", "٤٨٢١٩٣"} {
		r.call("POST /api/admin/students", "parent_pin "+bad+" is not 6 digits", "", admin, map[string]any{"name": "Pin Example", "parent_name": "Pin Parent", "parent_phone": "+9647000000607", "parent_pin": bad, "rfid_tag": "9007"}, 400)
	}
	r.call("POST /api/admin/students", "name too long", "", admin, map[string]any{"name": strings.Repeat("n", 101), "parent_name": "Omar Example", "parent_phone": phone1}, 400)
	var pinless int
	r.db.QueryRow(`SELECT COUNT(*) FROM parents WHERE phone_number = '+9647000000606'`).Scan(&pinless)
	r.expect("no parent created without a PIN", pinless, 0)
	r.call("POST /api/admin/students", "oversized body", "", admin, oversized(), 413)
	adminDenied("POST /api/admin/students", "", map[string]any{"name": "X", "parent_name": "X", "parent_phone": phone1})

	r.call("PUT /api/admin/students", "omitted fields are kept", "", admin, map[string]any{"id": idA, "name": "Sara Example", "parent_name": "Omar Example", "parent_phone": phone1}, 200)
	r.call("PUT /api/admin/students", "unknown student", "", admin, map[string]any{"id": 999999, "name": "Ghost Example", "parent_name": "Omar Example", "parent_phone": phone1}, 404)
	r.call("PUT /api/admin/students", "missing id", "", admin, map[string]any{"name": "Sara Example", "parent_name": "Omar Example", "parent_phone": phone1}, 400)
	r.call("PUT /api/admin/students", "parent_phone not an Iraqi mobile number", "", admin, map[string]any{"id": idA, "name": "Sara Example", "parent_name": "Omar Example", "parent_phone": "12345"}, 400)
	r.call("PUT /api/admin/students", "blank name", "", admin, map[string]any{"id": idA, "name": " ", "parent_name": "Omar Example", "parent_phone": phone1}, 400)
	r.call("PUT /api/admin/students", "malformed JSON", "", admin, "{", 400)
	r.call("PUT /api/admin/students", "rfid_tag taken by another student", "", admin, map[string]any{"id": idB, "name": "Yousef Example", "parent_name": "Omar Example", "parent_phone": phone1, "rfid_tag": "9001"}, 409)
	r.call("PUT /api/admin/students", "parent_pin of 4 digits", "", admin, map[string]any{"id": idA, "name": "Sara Example", "parent_name": "Omar Example", "parent_phone": phone1, "parent_pin": "4821"}, 400)
	r.call("PUT /api/admin/students", "keeps its own rfid_tag", "", admin, map[string]any{"id": idA, "name": "Sara Example", "parent_name": "Omar Example", "parent_phone": phone1, "rfid_tag": "9001"}, 200)
	r.call("PUT /api/admin/students", "new parent without parent_pin", "", admin, map[string]any{"id": idA, "name": "Sara Example", "parent_name": "New Parent", "parent_phone": "+9647000000606"}, 400)
	r.call("PUT /api/admin/students", "grade and section with surrounding spaces", "", admin, map[string]any{"id": idC, "name": "Maryam Example", "parent_name": "Omar Example", "parent_phone": phone1, "grade": " G1 ", "section": "B  "}, 200)
	var trimmedClass string
	r.db.QueryRow(`SELECT grade || '/' || section FROM students WHERE id = $1`, idC).Scan(&trimmedClass)
	r.expect("grade and section are stored trimmed", trimmedClass, "G1/B")
	r.call("PUT /api/admin/students", "oversized body", "", admin, oversized(), 413)
	adminDenied("PUT /api/admin/students", "", map[string]any{"id": idA})

	r.call("DELETE /api/admin/students", "deactivate", "?id="+strconv.Itoa(idE), admin, nil, 200)
	r.call("DELETE /api/admin/students", "unknown student", "?id=999999", admin, nil, 404)
	r.call("DELETE /api/admin/students", "id not an integer", "?id=abc", admin, nil, 400)
	r.call("DELETE /api/admin/students", "id missing", "", admin, nil, 400)
	adminDenied("DELETE /api/admin/students", "?id=1", nil)

	// ── Leaves ─────────────────────────────────────────────────────────────────
	_, d = r.call("POST /api/admin/leaves", "leave today", "", admin, map[string]any{"student_id": idC, "leave_date": today, "notes": "Family visit"}, 200)
	leaveID := at(d, "data", "leave_id")
	_, d = r.call("POST /api/admin/leaves", "same day again", "", admin, map[string]any{"student_id": idC, "leave_date": today, "notes": "Updated note"}, 200)
	r.expect("re-submitted leave keeps its id", at(d, "data", "leave_id"), leaveID)
	r.call("POST /api/admin/leaves", "leave_date missing", "", admin, map[string]any{"student_id": idC}, 400)
	r.call("POST /api/admin/leaves", "leave_date not YYYY-MM-DD", "", admin, map[string]any{"student_id": idC, "leave_date": "21-09-2026"}, 400)
	r.call("POST /api/admin/leaves", "malformed JSON", "", admin, "{", 400)
	r.call("POST /api/admin/leaves", "unknown student", "", admin, map[string]any{"student_id": 999999, "leave_date": today}, 404)
	r.call("POST /api/admin/leaves", "deactivated student", "", admin, map[string]any{"student_id": idE, "leave_date": today}, 404)
	r.call("POST /api/admin/leaves", "negative student_id", "", admin, map[string]any{"student_id": -1, "leave_date": today}, 400)
	for _, bad := range []string{"0000-01-01", "1999-12-31", "2101-01-01"} {
		r.call("POST /api/admin/leaves", "leave_date year "+bad, "", admin, map[string]any{"student_id": idC, "leave_date": bad}, 400)
	}
	r.call("POST /api/admin/leaves", "oversized body", "", admin, oversized(), 413)
	adminDenied("POST /api/admin/leaves", "", map[string]any{"student_id": idC, "leave_date": today})
	cancelDay := "2026-09-01"
	r.call("POST /api/admin/leaves", "leave to cancel", "", admin, map[string]any{"student_id": idC, "leave_date": cancelDay}, 200)
	cancelQuery := fmt.Sprintf("?student_id=%d&date=%s", idC, cancelDay)
	_, d = r.call("DELETE /api/admin/leaves", "cancel a leave", cancelQuery, admin, nil, 200)
	r.expect("cancel message", at(d, "message"), "No leave remains for this student on this date")
	var cancelled, kept int
	r.db.QueryRow(`SELECT COUNT(*) FILTER (WHERE leave_date = $2), COUNT(*) FILTER (WHERE leave_date = $3) FROM student_leaves WHERE student_id = $1`, idC, cancelDay, today).Scan(&cancelled, &kept)
	r.expect("cancelled leave is gone", cancelled, 0)
	r.expect("the same student's other leave is kept", kept, 1)
	r.call("DELETE /api/admin/leaves", "nothing left to cancel", cancelQuery, admin, nil, 200)
	r.call("DELETE /api/admin/leaves", "unknown student", "?student_id=999999&date="+cancelDay, admin, nil, 200)
	for _, bad := range []string{"", "?date=" + cancelDay, fmt.Sprintf("?student_id=%d", idC), "?student_id=0&date=" + cancelDay, "?student_id=-1&date=" + cancelDay, "?student_id=abc&date=" + cancelDay,
		fmt.Sprintf("?student_id=%d&date=01-09-2026", idC), fmt.Sprintf("?student_id=%d&date=1999-12-31", idC), fmt.Sprintf("?student_id=%d&date=2101-01-01", idC)} {
		r.call("DELETE /api/admin/leaves", "invalid query "+bad, bad, admin, nil, 400)
	}
	adminDenied("DELETE /api/admin/leaves", cancelQuery, nil)

	// ── Settings ───────────────────────────────────────────────────────────────
	r.call("PUT /api/admin/settings", "public setting", "", admin, map[string]string{"key": "whatsapp_number", "value": "+9647000000999"}, 200)
	r.call("PUT /api/admin/settings", "internal setting", "", admin, map[string]string{"key": "internal_note", "value": "staff only"}, 200)
	r.call("PUT /api/admin/settings", "empty key", "", admin, map[string]string{"key": "", "value": "x"}, 400)
	r.call("PUT /api/admin/settings", "key too long", "", admin, map[string]string{"key": strings.Repeat("k", 101), "value": "x"}, 400)
	r.call("PUT /api/admin/settings", "malformed JSON", "", admin, "{", 400)
	r.call("PUT /api/admin/settings", "oversized body", "", admin, oversized(), 413)
	adminDenied("PUT /api/admin/settings", "", map[string]string{"key": "internal_note", "value": "x"})
	_, d = r.call("GET /api/admin/settings", "all settings", "", admin, nil, 200)
	r.expect("admin settings include internal keys", at(d, "data", "internal_note"), "staff only")
	r.expect("admin settings include whatsapp_number", at(d, "data", "whatsapp_number"), "+9647000000999")
	adminDenied("GET /api/admin/settings", "", nil)

	// ── Hardware ───────────────────────────────────────────────────────────────
	x, _ := r.call("GET /iclock/cdata", "handshake", "?SN=TEST-SN-0001&options=all", "", nil, 200)
	r.expect("ADMS handshake body", string(x.body), "OK")
	r.call("GET /iclock/cdata", "handshake without SN", "", "", nil, 200)
	x, _ = r.call("GET /iclock/getrequest", "command poll", "?SN=TEST-SN-0001", "", nil, 200)
	r.expect("ADMS command poll body", string(x.body), "OK")
	batch := strings.Join([]string{
		"9001\t" + today + " 07:15:00\t1\t1",
		"9001\t" + today + " 12:30:00\t1\t1",
		"9004\t" + today + " 07:40:00\t1\t1",
		"8888\t" + today + " 07:41:00\t1\t1",
		"not a punch",
	}, "\n") + "\n"
	x, _ = r.call("POST /iclock/cdata", "ATTLOG batch", "?SN=TEST-SN-0001&table=ATTLOG", "", batch, 200)
	r.expect("ADMS push body", string(x.body), "OK")
	r.call("POST /iclock/cdata", "unregistered device", "?SN=TEST-SN-9999&table=ATTLOG", "", "9001\t"+today+" 08:00:00\t1\t1\n", 200)
	r.call("POST /iclock/cdata", "non-ATTLOG table", "?SN=TEST-SN-0001&table=OPERLOG", "", "OPLOG 4\t0\t"+today+" 08:00:00\n", 200)
	r.call("POST /iclock/cdata", "disabled device", "?SN=TEST-SN-0002&table=ATTLOG", "", "9001\t"+today+" 08:00:00\t1\t1\n", 200)
	r.call("POST /api/attendance/push", "ATTLOG via alias", "?SN=TEST-SN-0001&table=ATTLOG", "", "9004\t"+today+" 12:45:00\t1\t1\n", 200)
	var stored, ignored int
	r.db.QueryRow(`SELECT COUNT(*) FILTER (WHERE student_id IN ($1, $2)), COUNT(*) FILTER (WHERE check_time = $3::date + TIME '08:00') FROM attendance_logs`, idA, idD, today).Scan(&stored, &ignored)
	r.expect("punches stored from ADMS", stored, 4)
	r.expect("punches stored from unregistered or disabled devices", ignored, 0)

	_, d = r.call("POST /api/attendance/push/json", "new punch", "?SN=TEST-SN-0001", "", punch("TEST-SN-0001", "9001", today+" 07:20:00"), 200)
	r.expect("JSON push result", at(d, "message"), "Punched successfully")
	_, d = r.call("POST /api/attendance/push/json", "duplicate punch", "?SN=TEST-SN-0001", "", punch("TEST-SN-0001", "9001", today+" 07:20:00"), 200)
	r.expect("JSON push duplicate result", at(d, "message"), "Ignored or Duplicate")
	r.call("POST /api/attendance/push/json", "unknown rfid_tag", "?SN=TEST-SN-0001", "", punch("TEST-SN-0001", "8888", today+" 07:21:00"), 200)
	r.call("POST /api/attendance/push/json", "push_time not YYYY-MM-DD HH:MM:SS", "?SN=TEST-SN-0001", "", punch("TEST-SN-0001", "9001", "07:20"), 400)
	r.call("POST /api/attendance/push/json", "rfid_tag missing", "?SN=TEST-SN-0001", "", map[string]string{"device_sn": "TEST-SN-0001", "push_time": today + " 07:22:00"}, 400)
	r.call("POST /api/attendance/push/json", "malformed JSON", "?SN=TEST-SN-0001", "", "{", 400)
	r.call("POST /api/attendance/push/json", "oversized body", "?SN=TEST-SN-0001", "", oversized(), 413)
	r.call("POST /api/attendance/push/json", "SN missing", "", "", punch("TEST-SN-0001", "9001", today+" 07:23:00"), 401)
	r.call("POST /api/attendance/push/json", "unregistered device", "?SN=TEST-SN-9999", "", punch("TEST-SN-9999", "9001", today+" 07:23:00"), 401)
	r.call("POST /api/attendance/push/json", "disabled device", "?SN=TEST-SN-0002", "", punch("TEST-SN-0002", "9001", today+" 07:23:00"), 403)
	r.call("POST /api/attendance/push/json", "device_sn differs from SN", "?SN=TEST-SN-0001", "", punch("TEST-SN-0003", "9001", today+" 07:23:00"), 403)

	// ── Admin banners ──────────────────────────────────────────────────────────
	picture := func(filename string, data []byte) formPart { return formPart{"image", filename, "image/jpeg", data} }
	field := func(name, value string) formPart { return formPart{name: name, data: []byte(value)} }
	jpg, pngPic, webp := contractJPEG(t, 1), contractPNG(t, 2), contractWebP()
	_, d = r.call("POST /api/admin/banners", "JPEG picture with every field", "", admin, multipartBody{picture("open-day.jpg", jpg), field("title", "Open day 2026"), field("action_link", "https://example.com/open-day"), field("is_active", "true")}, 200)
	bannerJPEG := toInt(at(d, "data", "id"))
	r.expect("uploaded picture image_url", at(d, "data", "image_url"), fmt.Sprintf("/api/mobile/banners/image?id=%d", bannerJPEG))
	_, d = r.call("POST /api/admin/banners", "PNG picture declared as text, inactive", "", admin, multipartBody{{"image", "notes.txt", "text/plain", pngPic}, field("is_active", "false")}, 200)
	bannerPNG := toInt(at(d, "data", "id"))
	r.expect("picture type comes from its bytes", at(d, "data", "image_content_type"), "image/png")
	_, d = r.call("POST /api/admin/banners", "WebP picture without title", "", admin, multipartBody{picture("banner.webp", webp)}, 200)
	bannerWebP := toInt(at(d, "data", "id"))
	r.expect("omitted title is null", at(d, "data", "title"), nil)
	r.expect("omitted is_active means active", at(d, "data", "is_active"), true)
	notPicture := "image must be a JPEG, PNG or WebP picture"
	for _, tc := range []struct {
		name string
		body any
		msg  string
	}{
		{"SVG", multipartBody{{"image", "banner.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"/>`)}}, notPicture},
		{"GIF", multipartBody{{"image", "banner.gif", "image/gif", contractGIF(t)}}, notPicture},
		{"text named .jpg", multipartBody{picture("banner.jpg", []byte("not a picture"))}, notPicture},
		{"empty picture", multipartBody{picture("banner.jpg", nil)}, "image is empty"},
		{"no picture", multipartBody{field("title", "No picture")}, "image is required"},
		{"javascript: action_link", multipartBody{picture("banner.jpg", jpg), field("action_link", "javascript:alert(1)")}, "action_link must be an absolute http or https URL"},
		{"relative action_link", multipartBody{picture("banner.jpg", jpg), field("action_link", "/news")}, "action_link must be an absolute http or https URL"},
		{"title too long", multipartBody{picture("banner.jpg", jpg), field("title", strings.Repeat("t", 256))}, "title must be at most 255 characters"},
		{"is_active not true or false", multipartBody{picture("banner.jpg", jpg), field("is_active", "yes")}, `is_active must be "true" or "false"`},
		{"not multipart", map[string]any{"title": "JSON"}, "Request must be multipart/form-data"},
	} {
		_, d = r.call("POST /api/admin/banners", tc.name, "", admin, tc.body, 400)
		r.expect("message for "+tc.name, at(d, "message"), tc.msg)
	}
	tooBig := append(append([]byte{}, jpg...), make([]byte, 2<<20)...)
	r.call("POST /api/admin/banners", "picture over 2 MiB", "", admin, multipartBody{picture("big.jpg", tooBig)}, 413)
	r.call("POST /api/admin/banners", "body over the limit", "", admin, multipartBody{picture("banner.jpg", jpg), {name: "padding", data: bytes.Repeat([]byte("p"), 3<<20)}}, 413)
	adminDenied("POST /api/admin/banners", "", multipartBody{picture("banner.jpg", jpg)})

	jpegQuery := fmt.Sprintf("?id=%d", bannerJPEG)
	_, d = r.call("PUT /api/admin/banners", "title only", jpegQuery, admin, multipartBody{field("title", "Open day")}, 200)
	r.expect("title-only update keeps the picture", at(d, "data", "image_content_type"), "image/jpeg")
	r.expect("title-only update keeps action_link", at(d, "data", "action_link"), "https://example.com/open-day")
	jpg2 := contractJPEG(t, 3)
	_, d = r.call("PUT /api/admin/banners", "replace the picture", jpegQuery, admin, multipartBody{picture("new.jpg", jpg2)}, 200)
	r.expect("replaced picture size", toInt(at(d, "data", "image_size_bytes")), len(jpg2))
	r.call("PUT /api/admin/banners", "no field", jpegQuery, admin, multipartBody{}, 400)
	r.call("PUT /api/admin/banners", "id not an integer", "?id=abc", admin, multipartBody{field("title", "x")}, 400)
	r.call("PUT /api/admin/banners", "unknown banner", "?id=999999", admin, multipartBody{field("title", "Ghost")}, 404)
	r.call("PUT /api/admin/banners", "picture over 2 MiB", jpegQuery, admin, multipartBody{picture("big.jpg", tooBig)}, 413)
	adminDenied("PUT /api/admin/banners", jpegQuery, multipartBody{field("title", "x")})

	_, d = r.call("GET /api/admin/banners", "every banner", "", admin, nil, 200)
	r.expect("admin list has active, inactive and legacy banners", len(list(at(d, "data"))), 6)
	r.expect("admin list is newest first", toInt(at(d, "data", 0, "id")), bannerWebP)
	r.expect("legacy banner has no picture type", at(d, "data", 5, "image_content_type"), nil)
	adminDenied("GET /api/admin/banners", "", nil)

	pngQuery := fmt.Sprintf("?id=%d", bannerPNG)
	x, _ = r.call("GET /api/admin/banners/image", "preview of an inactive banner", pngQuery, admin, nil, 200)
	r.expect("preview is the uploaded bytes", bytes.Equal(x.body, pngPic), true)
	r.callWith("GET /api/admin/banners/image", "preview unchanged", pngQuery, map[string]string{"Authorization": "Bearer " + admin, "If-None-Match": x.header.Get("ETag")}, nil, 304)
	r.call("GET /api/admin/banners/image", "unknown banner", "?id=999999", admin, nil, 404)
	r.call("GET /api/admin/banners/image", "id not an integer", "?id=x", admin, nil, 400)
	adminDenied("GET /api/admin/banners/image", pngQuery, nil)

	_, d = r.call("POST /api/admin/banners", "banner to delete", "", admin, multipartBody{picture("banner.jpg", jpg)}, 200)
	doomed := fmt.Sprintf("?id=%d", toInt(at(d, "data", "id")))
	r.call("DELETE /api/admin/banners", "delete", doomed, admin, nil, 200)
	r.call("DELETE /api/admin/banners", "already deleted", doomed, admin, nil, 404)
	r.call("DELETE /api/admin/banners", "id missing", "", admin, nil, 400)
	adminDenied("DELETE /api/admin/banners", doomed, nil)
	var leftovers int
	r.db.QueryRow(`SELECT COUNT(*) FROM banner_images WHERE banner_id NOT IN (SELECT id FROM banners)`).Scan(&leftovers)
	r.expect("no picture outlives its banner", leftovers, 0)

	// ── Parent login ───────────────────────────────────────────────────────────
	r.call("POST /api/mobile/login", "wrong PIN", "", "", map[string]string{"phone": phone1, "pin": "0000"}, 401)
	r.call("POST /api/mobile/login", "unregistered phone", "", "", map[string]string{"phone": unknownPhone, "pin": "482193"}, 401)
	r.call("POST /api/mobile/login", "not an Iraqi mobile number", "", "", map[string]string{"phone": "12345", "pin": "482193"}, 401)
	_, d = r.call("POST /api/mobile/login", "valid PIN with FCM token", "", "", map[string]string{"phone": "0700 000 0101", "pin": "482193", "fcm_token": "fcm-contract-token-0001"}, 200)
	r.parent = str(at(d, "data", "token"))
	r.parentID = toInt(at(d, "data", "parent", "id"))
	if r.parent == "" || r.parentID == 0 {
		t.Fatalf("parent login returned no token or id: %v", d)
	}
	r.expect("login returns the canonical phone", at(d, "data", "parent", "phone"), phone1)
	if _, err := r.db.Exec(`INSERT INTO notifications (parent_id, parent_phone, title, body)
		SELECT $1, $2, 'إشعار تجريبي', 'رسالة رقم ' || g FROM generate_series(1, 150) g`, r.parentID, phone1); err != nil {
		t.Fatalf("notification fixture: %v", err)
	}
	for _, format := range []string{phone1, "009647000000101", "٠٧٠٠٠٠٠٠١٠١", "(0700) 000-0101"} {
		_, d := r.call("POST /api/mobile/login", "phone written as "+format, "", "", map[string]string{"phone": format, "pin": "482193"}, 200)
		r.expect("same parent for "+format, toInt(at(d, "data", "parent", "id")), r.parentID)
	}
	r.expect("login returns the parent name", at(d, "data", "parent", "name"), "Omar Example")
	var tokens int
	r.db.QueryRow(`SELECT COUNT(*) FROM device_tokens WHERE parent_id = $1 AND token = 'fcm-contract-token-0001'`, r.parentID).Scan(&tokens)
	r.expect("login fcm_token registered as a device token", tokens, 1)
	r.call("POST /api/mobile/login", "valid PIN without FCM token", "", "", map[string]string{"phone": phone1, "pin": "482193"}, 200)
	r.db.QueryRow(`SELECT COUNT(*) FROM device_tokens WHERE parent_id = $1`, r.parentID).Scan(&tokens)
	r.expect("login without fcm_token keeps the registered device", tokens, 1)
	r.call("POST /api/mobile/login", "pin missing", "", "", map[string]string{"phone": phone1}, 400)
	r.call("POST /api/mobile/login", "malformed JSON", "", "", "{", 400)
	r.call("POST /api/mobile/login", "oversized body", "", "", oversized(), 413)
	_, d = r.call("POST /api/mobile/login", "parent with no active child", "", "", map[string]string{"phone": phone3, "pin": "604158"}, 200)
	childless := str(at(d, "data", "token"))
	parent := r.parent
	expiredParent := signToken(t, claims("parent", r.parentID, earlier), contractSecret)

	parentDenied := func(op, query string) {
		t.Helper()
		r.call(op, "no token", query, "", nil, 401)
		r.call(op, "expired token", query, expiredParent, nil, 401)
		r.call(op, "admin token", query, admin, nil, 403)
	}

	// ── Public settings ────────────────────────────────────────────────────────
	_, d = r.call("GET /api/mobile/settings", "public settings", "", "", nil, 200)
	r.expect("public whatsapp_number", at(d, "data", "whatsapp_number"), "+9647000000999")
	if _, leaked := obj(at(d, "data"))["internal_note"]; leaked {
		t.Errorf("public settings expose internal_note")
	}

	// ── Children and tokens ────────────────────────────────────────────────────
	_, d = r.call("GET /api/mobile/students", "parent's children", "", parent, nil, 200)
	var ids []int
	for _, s := range list(at(d, "data")) {
		ids = append(ids, toInt(at(s, "id")))
	}
	r.expect("children by ascending id", ids, []int{idA, idB, idC})
	r.expect("unset grade is an empty string", at(d, "data", 1, "grade"), "")
	_, d = r.call("GET /api/mobile/students", "parent with no active child", "", childless, nil, 200)
	r.expect("no active child", len(list(at(d, "data"))), 0)
	r.call("GET /api/mobile/students", "token for a parent that does not exist", "", someParent, nil, 401)
	legacy := claims("parent", r.parentID, later)
	delete(legacy, "sv")
	r.call("GET /api/mobile/students", "token without sv (issued before session versions)", "", signToken(t, legacy, contractSecret), nil, 401)
	r.call("GET /api/mobile/students", "no Authorization header", "", "", nil, 401)
	r.callWith("GET /api/mobile/students", "scheme is not Bearer", "", map[string]string{"Authorization": "Token " + parent}, nil, 401)
	r.callWith("GET /api/mobile/students", "lower-case bearer", "", map[string]string{"Authorization": "bearer " + parent}, nil, 401)
	r.call("GET /api/mobile/students", "malformed token", "", "not-a-jwt", nil, 401)
	r.call("GET /api/mobile/students", "expired token", "", expiredParent, nil, 401)
	r.call("GET /api/mobile/students", "signed with another key", "", signToken(t, claims("parent", r.parentID, later), "another-secret"), nil, 401)
	r.call("GET /api/mobile/students", "unsigned token (alg none)", "", unsignedToken(t, claims("parent", r.parentID, later)), nil, 401)
	r.call("GET /api/mobile/students", "token without role", "", signToken(t, claims("", r.parentID, later), contractSecret), nil, 401)
	r.call("GET /api/mobile/students", "parent_id is a string", "", signToken(t, claims("parent", strconv.Itoa(r.parentID), later), contractSecret), nil, 401)
	r.call("GET /api/mobile/students", "admin token", "", admin, nil, 403)

	// ── Today ──────────────────────────────────────────────────────────────────
	_, d = r.call("GET /api/mobile/attendance/today", "today", "", parent, nil, 200)
	r.expect("today is the Asia/Baghdad date", at(d, "date"), today)
	byID := map[int]any{}
	var todayOrder []int
	for _, rec := range list(at(d, "data")) {
		byID[toInt(at(rec, "student_id"))] = rec
		todayOrder = append(todayOrder, toInt(at(rec, "student_id")))
	}
	r.expect("today ordered by student_id", todayOrder, []int{idA, idB, idC})
	r.expect("today: one record per active child", len(byID), 3)
	r.expect("present child", []any{at(byID[idA], "status"), at(byID[idA], "check_in_time"), at(byID[idA], "check_out_time")}, []any{"Present", "07:15 AM", "12:30 PM"})
	r.expect("absent child", []any{at(byID[idB], "status"), at(byID[idB], "check_in_time"), at(byID[idB], "check_out_time")}, []any{"Absent", nil, nil})
	r.expect("excused child", []any{at(byID[idC], "status"), at(byID[idC], "check_in_time")}, []any{"Excused", nil})
	parentDenied("GET /api/mobile/attendance/today", "")

	// ── Month views ────────────────────────────────────────────────────────────
	days := schoolDays(now)
	todayIsSchoolDay := len(days) > 0 && days[0] == today
	children := []int{idA, idB, idC}
	hideAbsentToday := todayIsSchoolDay && now.Before(time.Date(now.Year(), now.Month(), now.Day(), 9, 31, 0, 0, now.Location()))
	daysOf := func(id int) []string {
		if id == idB && hideAbsentToday {
			return days[1:]
		}
		return days
	}
	for _, q := range []string{"", "?month=" + month} {
		_, d = r.call("GET /api/mobile/attendance/summary", "current month "+q, q, parent, nil, 200)
		r.expect("summary month", at(d, "month"), month)
		rows := list(at(d, "data"))
		if len(days) == 0 {
			r.expect("summary without school days", len(rows), 0)
		} else {
			var got []int
			for i, row := range rows {
				got = append(got, toInt(at(row, "student_id")))
				sum := toInt(at(row, "total_present")) + toInt(at(row, "total_excused")) + toInt(at(row, "total_absent"))
				r.expect(fmt.Sprintf("summary[%d] counts every school day, without an Absent today before 09:31", i), sum, len(daysOf(toInt(at(row, "student_id")))))
			}
			r.expect("summary children by ascending id", got, children)
			if todayIsSchoolDay {
				r.expect("summary present days", at(rows, 0, "total_present"), 1)
				r.expect("summary excused days", at(rows, 2, "total_excused"), 1)
			}
		}

		_, d = r.call("GET /api/mobile/attendance/monthly", "current month "+q, q, parent, nil, 200)
		r.expect("monthly month", at(d, "month"), month)
		reports := list(at(d, "data"))
		if len(days) == 0 {
			r.expect("monthly without school days", len(reports), 0)
			continue
		}
		var got []int
		for _, rep := range reports {
			got = append(got, toInt(at(rep, "student_id")))
			var dates []string
			for _, rec := range list(at(rep, "records")) {
				dates = append(dates, str(at(rec, "date")))
				if _, old := obj(rec)["check_time"]; old {
					t.Errorf("monthly record %v still has the old check_time key", at(rec, "date"))
				}
			}
			r.expect(fmt.Sprintf("student %v: school days newest first, without an Absent today before 09:31", at(rep, "student_id")), dates, daysOf(toInt(at(rep, "student_id"))))
		}
		r.expect("monthly children by ascending id", got, children)
		if todayIsSchoolDay {
			r.expect("monthly today", []any{at(reports, 0, "records", 0, "status"), at(reports, 0, "records", 0, "check_in_time"), at(reports, 0, "records", 0, "check_out_time")}, []any{"Present", "07:15 AM", "12:30 PM"})
		}
	}
	_, d = r.call("POST /api/mobile/login", "parent of a child created today", "", "", map[string]string{"phone": phone2, "pin": "593047"}, 200)
	parentD := str(at(d, "data", "token"))
	previousMonth := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, now.Location()).Format("2006-01")
	for _, op := range []string{"GET /api/mobile/attendance/summary", "GET /api/mobile/attendance/monthly"} {
		_, d = r.call(op, "child created today, previous month", "?month="+previousMonth, parentD, nil, 200)
		r.expect(op+": a month before the child existed has no rows", len(list(at(d, "data"))), 0)
	}
	_, d = r.call("GET /api/mobile/attendance/monthly", "child created today, current month", "", parentD, nil, 200)
	var createdToday []string
	for _, rep := range list(at(d, "data")) {
		for _, rec := range list(at(rep, "records")) {
			createdToday = append(createdToday, str(at(rec, "date")))
		}
	}
	if todayIsSchoolDay {
		r.expect("a child created today has only today", createdToday, []string{today})
	} else {
		r.expect("a child created today has no school day yet", len(createdToday), 0)
	}
	for _, op := range []string{"GET /api/mobile/attendance/summary", "GET /api/mobile/attendance/monthly"} {
		_, d = r.call(op, "future month", "?month=2099-01", parent, nil, 200)
		r.expect("future month has no rows", len(list(at(d, "data"))), 0)
		for _, bad := range []string{"2026-13", "09-2026", "2026-9", "abc", "0000-01", "1999-12", "2101-01"} {
			r.call(op, "invalid month "+bad, "?month="+bad, parent, nil, 400)
		}
		parentDenied(op, "")
	}

	// ── Schedule ───────────────────────────────────────────────────────────────
	_, d = r.call("GET /api/mobile/schedule", "weekly schedule", "", parent, nil, 200)
	var order []string
	for _, e := range list(at(d, "data")) {
		order = append(order, fmt.Sprintf("%v %s %v", at(e, "student_id"), at(e, "day_of_week"), at(e, "period_number")))
	}
	want := []string{
		fmt.Sprintf("%d الاحد 1", idA), fmt.Sprintf("%d الأحد 2", idA), fmt.Sprintf("%d الإثنين 2", idA),
		fmt.Sprintf("%d Tuesday 1", idA), fmt.Sprintf("%d الخميس 1", idA), fmt.Sprintf("%d Holiday 1", idA),
		fmt.Sprintf("%d sunday 1", idC), fmt.Sprintf("%d Wednesday 3", idC), fmt.Sprintf("%d السبت 1", idC),
	}
	r.expect("schedule order: child, school week, period", order, want)
	for i := 1; i < len(order); i++ {
		a, b := list(at(d, "data"))[i-1], list(at(d, "data"))[i]
		if toInt(at(a, "student_id")) == toInt(at(b, "student_id")) && weekdayRank(str(at(a, "day_of_week"))) > weekdayRank(str(at(b, "day_of_week"))) {
			t.Errorf("schedule: %s before %s", order[i-1], order[i])
		}
	}
	r.expect("missing teacher is an empty string", at(d, "data", 1, "teacher_name"), "")
	parentDenied("GET /api/mobile/schedule", "")

	// ── Notifications ──────────────────────────────────────────────────────────
	_, d = r.call("GET /api/mobile/notifications", "first page", "", parent, nil, 200)
	page := list(at(d, "data"))
	r.expect("first page size", len(page), 100)
	r.expect("first page has_more", at(d, "has_more"), true)
	next := toInt(at(d, "next_before"))
	r.expect("next_before is the last id on the page", next, toInt(at(page, 99, "id")))
	for i := 1; i < len(page); i++ {
		if toInt(at(page, i, "id")) >= toInt(at(page, i-1, "id")) {
			t.Errorf("notifications not newest first at %d", i)
			break
		}
	}
	var older int
	r.db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE parent_phone = $1 AND id < $2`, phone1, next).Scan(&older)
	_, d = r.call("GET /api/mobile/notifications", "second page", "?before="+strconv.Itoa(next), parent, nil, 200)
	r.expect("second page holds the rest", len(list(at(d, "data"))), older)
	r.expect("second page has_more", at(d, "has_more"), false)
	r.expect("second page next_before", at(d, "next_before"), nil)
	if first := toInt(at(d, "data", 0, "id")); first >= next {
		t.Errorf("second page starts at id %d, not below the cursor %d", first, next)
	}
	for _, bad := range []string{"abc", "0", "-1", "1.5"} {
		r.call("GET /api/mobile/notifications", "invalid cursor "+bad, "?before="+bad, parent, nil, 400)
	}
	parentDenied("GET /api/mobile/notifications", "")

	// ── Read state ─────────────────────────────────────────────────────────────
	var unread int
	r.db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE parent_id = $1 AND is_read IS NOT TRUE`, r.parentID).Scan(&unread)
	_, d = r.call("GET /api/mobile/notifications", "unread_count", "", parent, nil, 200)
	r.expect("unread_count before marking", toInt(at(d, "unread_count")), unread)
	own := toInt(at(d, "data", 0, "id"))
	for i := 0; i < 2; i++ {
		r.call("PUT /api/mobile/notifications/read", "own notification", fmt.Sprintf("?id=%d", own), parent, nil, 200)
	}
	_, d = r.call("GET /api/mobile/notifications", "after marking one", "", parent, nil, 200)
	r.expect("marked notification is read", at(d, "data", 0, "is_read"), true)
	r.expect("unread_count after marking one", toInt(at(d, "unread_count")), unread-1)
	var foreign int
	r.db.QueryRow(`SELECT id FROM notifications WHERE parent_id <> $1 ORDER BY id LIMIT 1`, r.parentID).Scan(&foreign)
	if foreign == 0 {
		t.Fatalf("no notification of another parent to test with")
	}
	r.call("PUT /api/mobile/notifications/read", "another parent's notification", fmt.Sprintf("?id=%d", foreign), parent, nil, 404)
	r.call("PUT /api/mobile/notifications/read", "missing notification", "?id=999999", parent, nil, 404)
	for _, bad := range []string{"", "?id=abc", "?id=0", "?id=-1"} {
		r.call("PUT /api/mobile/notifications/read", "invalid id "+bad, bad, parent, nil, 400)
	}
	_, d = r.call("PUT /api/mobile/notifications/read-all", "mark all", "", parent, nil, 200)
	r.expect("mark all updated", toInt(at(d, "data", "updated")), unread-1)
	_, d = r.call("PUT /api/mobile/notifications/read-all", "mark all again", "", parent, nil, 200)
	r.expect("mark all again updated", toInt(at(d, "data", "updated")), 0)
	_, d = r.call("GET /api/mobile/notifications", "after marking all", "", parent, nil, 200)
	r.expect("unread_count after marking all", toInt(at(d, "unread_count")), 0)
	parentDenied("PUT /api/mobile/notifications/read", "?id=1")
	parentDenied("PUT /api/mobile/notifications/read-all", "")

	// ── Banners ────────────────────────────────────────────────────────────────
	_, d = r.call("GET /api/mobile/banners", "active banners", "", parent, nil, 200)
	var banners []string
	for _, b := range list(at(d, "data")) {
		banners = append(banners, fmt.Sprintf("%q %q", at(b, "title"), at(b, "action_link")))
	}
	r.expect("active banners newest first", banners, []string{`"" ""`, `"Open day" "https://example.com/open-day"`, `"" "https://example.com/sports"`, `"Open day" ""`})
	r.expect("legacy banner keeps its stored image_url", at(d, "data", 2, "image_url"), "https://example.com/banners/sports.jpg")
	imageURL := str(at(d, "data", 1, "image_url"))
	imagePath, imageQuery, _ := strings.Cut(imageURL, "?")
	r.expect("uploaded banner image_url is the public picture route", imagePath, "/api/mobile/banners/image")
	x, _ = r.call("GET /api/mobile/banners/image", "picture of an active banner, no token", "?"+imageQuery, "", nil, 200)
	r.expect("public picture is the uploaded bytes", bytes.Equal(x.body, jpg2), true)
	r.callWith("GET /api/mobile/banners/image", "picture unchanged", "?"+imageQuery, map[string]string{"If-None-Match": x.header.Get("ETag")}, nil, 304)
	r.call("GET /api/mobile/banners/image", "inactive banner", pngQuery, "", nil, 404)
	r.call("GET /api/mobile/banners/image", "unknown banner", "?id=999999", "", nil, 404)
	r.call("GET /api/mobile/banners/image", "id not an integer", "?id=x", "", nil, 400)
	parentDenied("GET /api/mobile/banners", "")
	parentDenied("GET /api/mobile/students", "")

	// ── Device tokens ──────────────────────────────────────────────────────────
	device := map[string]string{"token": "contract-device-token-0001"}
	for _, op := range []string{"PUT /api/mobile/device-token", "DELETE /api/mobile/device-token"} {
		r.call(op, "valid token", "", parent, device, 200)
		r.call(op, "same token again (idempotent)", "", parent, device, 200)
		r.call(op, "token too short", "", parent, map[string]string{"token": "short"}, 400)
		r.call(op, "token with spaces", "", parent, map[string]string{"token": "contract device token 0001"}, 400)
		r.call(op, "token missing", "", parent, map[string]string{}, 400)
		r.call(op, "malformed JSON", "", parent, "{", 400)
		r.call(op, "oversized body", "", parent, oversized(), 413)
		r.call(op, "no token", "", "", device, 401)
		r.call(op, "expired token", "", expiredParent, device, 401)
		r.call(op, "admin token", "", admin, device, 403)
	}
	r.call("PUT /api/mobile/device-token", "register before the PIN change", "", parent, device, 200)

	// ── PIN change signs the parent out ────────────────────────────────────────
	r.call("PUT /api/admin/students", "change the parent's PIN", "", admin, map[string]any{"id": idA, "name": "Sara Example", "parent_name": "Omar Example", "parent_phone": phone1, "parent_pin": "482204"}, 200)
	r.call("GET /api/mobile/students", "token issued before the PIN change", "", parent, nil, 401)
	r.call("POST /api/mobile/login", "old PIN after the change", "", "", map[string]string{"phone": phone1, "pin": "482193"}, 401)
	_, d = r.call("POST /api/mobile/login", "new PIN", "", "", map[string]string{"phone": phone1, "pin": "482204"}, 200)
	r.parent = str(at(d, "data", "token"))
	r.call("GET /api/mobile/students", "token issued after the PIN change", "", r.parent, nil, 200)
	r.db.QueryRow(`SELECT COUNT(*) FROM device_tokens WHERE parent_id = $1`, r.parentID).Scan(&tokens)
	r.expect("PIN change removed the parent's device tokens", tokens, 0)

	// ── Admin reports ──────────────────────────────────────────────────────────
	_, d = r.call("GET /api/admin/dashboard", "today's totals", "", admin, nil, 200)
	r.expect("dashboard date", at(d, "date"), today)
	r.expect("dashboard totals", []any{at(d, "data", "total_students"), at(d, "data", "total_parents"), at(d, "data", "present_today"), at(d, "data", "excused_today"), at(d, "data", "absent_today")}, []any{4, 3, 2, 1, 1})
	r.call("GET /api/admin/dashboard", "malformed token", "", "not-a-jwt", nil, 401)
	adminDenied("GET /api/admin/dashboard", "", nil)

	_, d = r.call("GET /api/admin/students", "active students", "", admin, nil, 200)
	ids = nil
	for _, s := range list(at(d, "data")) {
		ids = append(ids, toInt(at(s, "id")))
	}
	r.expect("students newest first, deactivated excluded", ids, []int{idD, idC, idB, idA})
	r.expect("PUT kept rfid_tag and grade", []any{at(d, "data", 3, "rfid_tag"), at(d, "data", 3, "grade")}, []any{"9001", "G3"})
	adminDenied("GET /api/admin/students", "", nil)

	for _, q := range []string{"", "?date=" + today} {
		_, d = r.call("GET /api/admin/attendance", "daily report "+q, q, admin, nil, 200)
		r.expect("report date", at(d, "date"), today)
		ids = nil
		for _, rec := range list(at(d, "data")) {
			ids = append(ids, toInt(at(rec, "student_id")))
		}
		r.expect("Present, Excused, Absent, then by name", ids, []int{idD, idA, idC, idB})
	}
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	_, d = r.call("GET /api/admin/attendance", "a day before a student was created", "?date="+yesterday, admin, nil, 200)
	ids = nil
	for _, rec := range list(at(d, "data")) {
		ids = append(ids, toInt(at(rec, "student_id")))
	}
	sort.Ints(ids)
	r.expect("yesterday's report leaves out the student created today", ids, []int{idA, idB, idC})
	r.call("GET /api/admin/export/excel", "a day before a student was created", "?date="+yesterday, admin, nil, 200)
	for _, bad := range []string{"2026-02-30", "21-09-2026", "today", "0000-01-01", "1999-12-31", "2101-01-01"} {
		r.call("GET /api/admin/attendance", "invalid date "+bad, "?date="+bad, admin, nil, 400)
		r.call("GET /api/admin/export/excel", "invalid date "+bad, "?date="+bad, admin, nil, 400)
	}
	adminDenied("GET /api/admin/attendance", "", nil)

	x, _ = r.call("GET /api/admin/export/excel", "Excel report", "", admin, nil, 200)
	if !bytes.HasPrefix(x.body, []byte("PK\x03\x04")) {
		t.Errorf("Excel export is not an xlsx (zip) file")
	}
	r.expect("Excel file name", x.header.Get("Content-Disposition"), "attachment; filename=attendance_"+today+".xlsx")
	adminDenied("GET /api/admin/export/excel", "", nil)

	deadline := time.Now().Add(10 * time.Second)
	for synced := false; !synced && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		r.db.QueryRow(`SELECT last_sync IS NOT NULL FROM devices WHERE serial_number = 'TEST-SN-0001'`).Scan(&synced)
	}
	_, d = r.call("GET /api/admin/devices", "all devices", "", admin, nil, 200)
	var devs []string
	for _, dv := range list(at(d, "data")) {
		_, hasSync := obj(dv)["last_sync"]
		devs = append(devs, fmt.Sprintf("%v %q %v %v", at(dv, "serial_number"), at(dv, "location_name"), at(dv, "is_active"), hasSync))
	}
	r.expect("devices by location, last_sync only once synced", devs, []string{
		`TEST-SN-0002 "" false false`, `TEST-SN-0004 "Annex" true false`, `TEST-SN-0001 "Main Gate" true true`, `TEST-SN-0003 "Side Gate" true false`,
	})
	adminDenied("GET /api/admin/devices", "", nil)

	r.call("PUT /api/admin/devices", "update", "", admin, map[string]any{"serial_number": "TEST-SN-0003", "location_name": "Back Gate", "is_active": true}, 200)
	r.call("PUT /api/admin/devices", "unknown device", "", admin, map[string]any{"serial_number": "TEST-SN-9999", "location_name": "X", "is_active": true}, 404)
	r.call("PUT /api/admin/devices", "missing serial", "", admin, map[string]any{"location_name": "X", "is_active": true}, 400)
	r.call("PUT /api/admin/devices", "malformed JSON", "", admin, "{", 400)
	r.call("PUT /api/admin/devices", "oversized body", "", admin, oversized(), 413)
	adminDenied("PUT /api/admin/devices", "", map[string]any{"serial_number": "TEST-SN-0003", "location_name": "X", "is_active": true})

	r.call("PUT /api/admin/devices", "only serial_number", "", admin, map[string]any{"serial_number": "TEST-SN-0003"}, 400)
	r.call("PUT /api/admin/devices", "location_name too long", "", admin, map[string]any{"serial_number": "TEST-SN-0003", "location_name": strings.Repeat("L", 51)}, 400)
	r.call("PUT /api/admin/devices", "only location_name", "", admin, map[string]any{"serial_number": "TEST-SN-0003", "location_name": "Rear Gate"}, 200)
	var location string
	var active bool
	r.db.QueryRow(`SELECT location_name, is_active FROM devices WHERE serial_number = 'TEST-SN-0003'`).Scan(&location, &active)
	r.expect("PUT with only location_name keeps is_active", fmt.Sprintf("%q %v", location, active), `"Rear Gate" true`)
	r.call("DELETE /api/admin/devices", "disable", "?sn=TEST-SN-0003", admin, nil, 200)
	r.call("DELETE /api/admin/devices", "unknown device still 200", "?sn=TEST-SN-NONE", admin, nil, 200)
	r.call("DELETE /api/admin/devices", "sn missing", "", admin, nil, 400)
	adminDenied("DELETE /api/admin/devices", "?sn=TEST-SN-0003", nil)

	// ── Admin schedule ─────────────────────────────────────────────────────────
	period := func(day string, n int, subject string, teacher any) map[string]any {
		return map[string]any{"day_of_week": day, "period_number": n, "subject_name": subject, "teacher_name": teacher}
	}
	classG4C := func(periods ...map[string]any) map[string]any {
		if periods == nil {
			periods = []map[string]any{}
		}
		return map[string]any{"grade": "G4", "section": "C", "periods": periods}
	}
	scheduleRows := func(d any) []string {
		var rows []string
		for _, e := range list(at(d, "data")) {
			rows = append(rows, fmt.Sprintf("%s %v %s", at(e, "day_of_week"), at(e, "period_number"), at(e, "subject_name")))
		}
		return rows
	}
	wantSchedule := []string{"الأحد 1 Mathematics", "الأحد 2 Science", "الإثنين 1 Reading", "الخميس 2 Art"}
	_, d = r.call("PUT /api/admin/schedule", "scrambled periods with mixed day names", "", admin, map[string]any{"grade": " G4 ", "section": "C ", "periods": []map[string]any{
		period("Thursday", 2, "Art", nil), period("الاحد", 2, "Science", "Teacher Example"), period(" monday ", 1, "Reading", "  "), period("الأحد", 1, "Mathematics", "Teacher Example"),
	}}, 200)
	r.expect("saved schedule: canonical days, school week, period", scheduleRows(d), wantSchedule)
	r.expect("saved class is trimmed", fmt.Sprint(at(d, "grade"), "/", at(d, "section")), "G4/C")
	r.expect("blank teacher_name is stored as null", at(d, "data", 2, "teacher_name"), nil)
	_, d = r.call("GET /api/admin/schedule", "class schedule", "?grade=G4&section=C", admin, nil, 200)
	r.expect("read back the saved schedule", scheduleRows(d), wantSchedule)
	_, d = r.call("GET /api/admin/schedule", "class without a schedule", "?grade=G9&section=Z", admin, nil, 200)
	r.expect("class without a schedule is empty", len(list(at(d, "data"))), 0)
	r.call("GET /api/admin/schedule", "missing grade", "?section=C", admin, nil, 400)
	r.call("GET /api/admin/schedule", "blank section", "?grade=G4&section=%20", admin, nil, 400)
	r.call("GET /api/admin/schedule", "grade too long", "?grade="+strings.Repeat("g", 51)+"&section=C", admin, nil, 400)
	adminDenied("GET /api/admin/schedule", "?grade=G4&section=C", nil)

	ok := period("الأحد", 1, "Mathematics", nil)
	var sixtyOne []map[string]any
	for _, day := range []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday"} {
		for n := 1; n <= 12; n++ {
			sixtyOne = append(sixtyOne, period(day, n, "Mathematics", nil))
		}
	}
	sixtyOne = append(sixtyOne, period("Sunday", 1, "Extra", nil))
	for _, tc := range []struct {
		name string
		body any
		msg  string
	}{
		{"missing grade", map[string]any{"section": "C", "periods": []any{ok}}, "grade is required"},
		{"blank section", map[string]any{"grade": "G4", "section": "  ", "periods": []any{ok}}, "section is required"},
		{"grade too long", map[string]any{"grade": strings.Repeat("g", 51), "section": "C", "periods": []any{ok}}, "grade must be at most 50 characters"},
		{"period_number 0", classG4C(period("الأحد", 0, "Mathematics", nil)), "periods[0].period_number must be from 1 to 12"},
		{"period_number 13", classG4C(ok, period("الخميس", 13, "Mathematics", nil)), "periods[1].period_number must be from 1 to 12"},
		{"blank subject_name", classG4C(period("الأحد", 1, "   ", nil)), "periods[0].subject_name is required"},
		{"subject_name too long", classG4C(period("الأحد", 1, strings.Repeat("م", 101), nil)), "periods[0].subject_name must be at most 100 characters"},
		{"teacher_name too long", classG4C(period("الأحد", 1, "Mathematics", strings.Repeat("م", 101))), "periods[0].teacher_name must be at most 100 characters"},
		{"same day and period twice", classG4C(ok, period("Sunday", 1, "Art", nil)), "periods contain day_of_week الأحد with period_number 1 more than once"},
		{"Friday", classG4C(period("Friday", 1, "Mathematics", nil)), "periods[0].day_of_week must be a school day, Sunday to Thursday (Arabic or English)"},
		{"Saturday in Arabic", classG4C(period("السبت", 1, "Mathematics", nil)), "periods[0].day_of_week must be a school day, Sunday to Thursday (Arabic or English)"},
		{"61 periods", classG4C(sixtyOne...), "periods must contain at most 60 entries"},
		{"periods missing", map[string]any{"grade": "G4", "section": "C"}, "periods is required; send an empty array to clear the schedule"},
		{"period_number not an integer", `{"grade":"G4","section":"C","periods":[{"day_of_week":"Sunday","period_number":1.5,"subject_name":"Art"}]}`, "Invalid request body"},
		{"malformed JSON", "{", "Invalid request body"},
	} {
		_, d = r.call("PUT /api/admin/schedule", tc.name, "", admin, tc.body, 400)
		r.expect("message for "+tc.name, at(d, "message"), tc.msg)
	}
	_, d = r.call("GET /api/admin/schedule", "rejected saves changed nothing", "?grade=G4&section=C", admin, nil, 200)
	r.expect("schedule after rejected saves", scheduleRows(d), wantSchedule)
	r.call("PUT /api/admin/schedule", "oversized body", "", admin, oversized(), 413)
	adminDenied("PUT /api/admin/schedule", "", classG4C())
	_, d = r.call("PUT /api/admin/schedule", "empty periods clears the class", "", admin, classG4C(), 200)
	r.expect("cleared schedule", len(list(at(d, "data"))), 0)
	var g4c, g3a int
	r.db.QueryRow(`SELECT COUNT(*) FILTER (WHERE grade = 'G4' AND section = 'C'), COUNT(*) FILTER (WHERE grade = 'G3' AND section = 'A') FROM weekly_schedules`).Scan(&g4c, &g3a)
	r.expect("rows left for the cleared class", g4c, 0)
	r.expect("rows of another class after clearing", g3a, 6)

	// ── System ─────────────────────────────────────────────────────────────────
	r.call("GET /health", "liveness", "", "", nil, 200)

	// ── Rate limits ────────────────────────────────────────────────────────────
	for i := 1; i <= 5; i++ {
		r.call("POST /api/admin/login", fmt.Sprintf("failed admin attempt %d", i), "", "", map[string]string{"username": "locked-admin", "password": "wrong-password"}, 401)
	}
	x, _ = r.call("POST /api/admin/login", "admin username rate-limited from this IP", "", "", map[string]string{"username": "locked-admin", "password": "wrong-password"}, 429)
	if n, err := strconv.Atoi(x.header.Get("Retry-After")); err != nil || n < 1 || n > 900 {
		t.Errorf("admin Retry-After %q: want whole seconds in 1..900", x.header.Get("Retry-After"))
	}
	for _, tc := range []struct{ phone, pin string }{{phone2, "593047"}, {limitedPhone, "2580"}} {
		for i := 1; i <= 5; i++ {
			r.call("POST /api/mobile/login", fmt.Sprintf("failed attempt %d", i), "", "", map[string]string{"phone": tc.phone, "pin": "0000"}, 401)
		}
		x, _ = r.call("POST /api/mobile/login", "rate-limited, even with the right PIN", "", "", map[string]string{"phone": tc.phone, "pin": tc.pin}, 429)
		if n, err := strconv.Atoi(x.header.Get("Retry-After")); err != nil || n < 1 || n > 900 {
			t.Errorf("Retry-After %q: want whole seconds in 1..900", x.header.Get("Retry-After"))
		}
	}

	// ── Oversized headers (twice the limit), for every operation called above ────
	seen := map[string]bool{}
	var ops []string
	for _, x := range r.xs {
		if !seen[x.op] {
			seen[x.op] = true
			ops = append(ops, x.op)
		}
	}
	for _, op := range ops {
		r.callWith(op, "headers of 128 KiB", "", map[string]string{"X-Pad": strings.Repeat("a", 128<<10)}, nil, 431)
	}
}

// databaseFailures triggers the documented 500 and 503 responses with real database failures,
// then restores the database.
func (r *runner) databaseFailures() {
	t := r.t
	today := tz.Today()
	exec := func(q string) {
		t.Helper()
		if _, err := r.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	admin, parent := r.admin, r.parent

	tables := []string{"students", "parents", "settings", "devices", "notifications", "banners", "admins", "device_tokens"}
	for _, tb := range tables {
		exec("ALTER TABLE " + tb + " RENAME TO " + tb + "_offline")
	}
	for _, op := range []string{
		"GET /api/mobile/students", "GET /api/mobile/attendance/today", "GET /api/mobile/attendance/summary",
		"GET /api/mobile/attendance/monthly", "GET /api/mobile/schedule", "GET /api/mobile/notifications",
		"GET /api/mobile/banners", "PUT /api/mobile/notifications/read-all",
	} {
		r.call(op, "database error", "", parent, nil, 500)
	}
	r.call("PUT /api/mobile/notifications/read", "database error", "?id=1", parent, nil, 500)
	r.call("GET /api/mobile/settings", "database error", "", "", nil, 500)
	r.call("PUT /api/mobile/device-token", "database error", "", parent, map[string]string{"token": "contract-device-token-0002"}, 500)
	r.call("DELETE /api/mobile/device-token", "database error", "", parent, map[string]string{"token": "contract-device-token-0002"}, 500)
	r.call("POST /api/mobile/login", "database error", "", "", map[string]string{"phone": phone1, "pin": "482193"}, 500)
	r.call("POST /api/admin/login", "database error", "", "", map[string]string{"username": "admin", "password": adminPassword}, 500)
	for _, op := range []string{
		"GET /api/admin/dashboard", "GET /api/admin/students", "GET /api/admin/attendance",
		"GET /api/admin/export/excel", "GET /api/admin/settings", "GET /api/admin/devices", "GET /api/admin/banners",
	} {
		r.call(op, "database error", "", admin, nil, 500)
	}
	r.call("POST /api/admin/students", "database error", "", admin, map[string]any{"name": "Down Example", "parent_name": "Omar Example", "parent_phone": phone1}, 500)
	r.call("PUT /api/admin/students", "database error", "", admin, map[string]any{"id": 1, "name": "Down Example", "parent_name": "Omar Example", "parent_phone": phone1}, 500)
	r.call("DELETE /api/admin/students", "database error", "?id=1", admin, nil, 500)
	r.call("PUT /api/admin/settings", "database error", "", admin, map[string]string{"key": "internal_note", "value": "x"}, 500)
	r.call("GET /api/admin/schedule", "database error", "?grade=G4&section=C", admin, nil, 500)
	bannerPicture := multipartBody{{"image", "banner.jpg", "image/jpeg", contractJPEG(t, 4)}}
	r.call("POST /api/admin/banners", "database error", "", admin, bannerPicture, 500)
	r.call("PUT /api/admin/banners", "database error", "?id=1", admin, multipartBody{{name: "title", data: []byte("Down")}}, 500)
	r.call("DELETE /api/admin/banners", "database error", "?id=1", admin, nil, 500)
	r.call("GET /api/admin/banners/image", "database error", "?id=1", admin, nil, 500)
	r.call("GET /api/mobile/banners/image", "database error", "?id=1", "", nil, 500)
	r.call("PUT /api/admin/schedule", "database error", "", admin, map[string]any{"grade": "G4", "section": "C", "periods": []any{}}, 500)
	r.call("POST /api/admin/leaves", "database error", "", admin, map[string]any{"student_id": 1, "leave_date": today}, 500)
	r.call("DELETE /api/admin/leaves", "database error", "?student_id=1&date="+today, admin, nil, 500)
	r.call("POST /api/admin/devices", "database error", "", admin, map[string]any{"serial_number": "TEST-SN-0100"}, 500)
	r.call("PUT /api/admin/devices", "database error", "", admin, map[string]any{"serial_number": "TEST-SN-0001", "location_name": "Main Gate", "is_active": true}, 500)
	r.call("DELETE /api/admin/devices", "database error", "?sn=TEST-SN-0100", admin, nil, 500)
	r.call("POST /api/attendance/push/json", "database error", "?SN=TEST-SN-0001", "", punch("TEST-SN-0001", "9001", today+" 07:25:00"), 500)
	x, _ := r.call("POST /iclock/cdata", "non-transient database error is still ACKed", "?SN=TEST-SN-0001&table=ATTLOG", "", "9001\t"+today+" 07:26:00\t1\t1\n", 200)
	r.expect("ADMS body on a non-transient error", string(x.body), "OK")
	for _, tb := range tables {
		exec("ALTER TABLE " + tb + "_offline RENAME TO " + tb)
	}

	// A lock the server cannot get within lock_timeout (55P03) is a transient database failure.
	var dbName string
	if err := r.db.QueryRow(`SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	r.db.SetMaxIdleConns(0)
	exec("ALTER DATABASE " + dbName + " SET lock_timeout = '300ms'")
	exec("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()")
	tx, err := r.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("LOCK TABLE devices IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"POST /iclock/cdata", "POST /api/attendance/push"} {
		x, _ := r.call(op, "database unavailable", "?SN=TEST-SN-0001&table=ATTLOG", "", "9001\t"+today+" 07:27:00\t1\t1\n", 503)
		r.expect(op+" retry body", string(x.body), "RETRY")
	}
	r.call("POST /api/attendance/push/json", "database unavailable", "?SN=TEST-SN-0001", "", punch("TEST-SN-0001", "9001", today+" 07:27:00"), 503)
	tx.Rollback()
	exec("ALTER DATABASE " + dbName + " RESET lock_timeout")
	var retried int
	r.db.QueryRow(`SELECT COUNT(*) FROM attendance_logs WHERE check_time IN ($1::date + TIME '07:26', $1::date + TIME '07:27')`, today).Scan(&retried)
	r.expect("nothing stored while the database was failing", retried, 0)
}

// transport checks the responses the spec documents once, under x-transport-responses, rather
// than per operation, and the plain-text answers /health and /iclock/* keep (RULES.md §5).
func (r *runner) transport(t *testing.T) {
	record := func(name, method, target string, header map[string]string, wantStatus int, wantAllow string) {
		t.Helper()
		h := http.Header{}
		for k, v := range header {
			h.Set(k, v)
		}
		status, hd, body, err := r.srv.send(method, target, h, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		r.tx = append(r.tx, &exchange{name: name, op: method + " " + target, target: target, status: status, header: hd, body: body})
		if status != wantStatus || hd.Get("Allow") != wantAllow {
			t.Errorf("%s: got %d Allow=%q %q, want %d Allow=%q", name, status, hd.Get("Allow"), body, wantStatus, wantAllow)
		}
	}
	record("unknown path", "GET", "/api/mobile/unknown", nil, 404, "")
	record("unknown root path", "GET", "/", nil, 404, "")
	record("wrong method before authentication", "PUT", "/api/mobile/students", nil, 405, "GET, HEAD")
	record("wrong method with a valid token", "PUT", "/api/mobile/students", authHeader(r.parent), 405, "GET, HEAD")
	record("wrong method on an admin route", "PATCH", "/api/admin/devices", nil, 405, "DELETE, GET, HEAD, POST, PUT")
	record("wrong method on the schedule route", "DELETE", "/api/admin/schedule", nil, 405, "GET, HEAD, PUT")
	record("wrong method on the banner picture route", "DELETE", "/api/mobile/banners/image", nil, 405, "GET, HEAD")
	record("wrong method on a login route", "GET", "/api/admin/login", nil, 405, "POST")

	plain := func(name, method, target string, wantStatus int, wantBody string) {
		t.Helper()
		status, hd, body, err := r.srv.send(method, target, nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		mt, _, _ := mime.ParseMediaType(hd.Get("Content-Type"))
		if status != wantStatus || mt != "text/plain" || string(body) != wantBody {
			t.Errorf("%s: got %d %s %q, want %d text/plain %q", name, status, mt, body, wantStatus, wantBody)
		}
	}
	plain("unsupported method on /health", "POST", "/health", 405, "Method Not Allowed\n")
	plain("unsupported method on /iclock", "DELETE", "/iclock/getrequest", 405, "Method Not Allowed\n")
	plain("unknown /iclock path", "GET", "/iclock/devicecmd", 404, "404 page not found\n")
	plain("ADMS alias ACKs any method", "GET", "/api/attendance/push?SN=TEST-SN-0001", 200, "OK")
}

func transportProblems(s *apiSpec, xs []*exchange) []string {
	var problems []string
	responses := obj(s.root["x-transport-responses"])
	for _, x := range xs {
		where := fmt.Sprintf("%s [%s] → %d", x.op, x.name, x.status)
		resp := obj(responses[strconv.Itoa(x.status)])
		if resp == nil {
			problems = append(problems, fmt.Sprintf("%s: status %d is not in x-transport-responses", where, x.status))
			continue
		}
		problems = append(problems, responseProblems(s, where, resp, x)...)
	}
	return problems
}

type route struct {
	pattern, method, path string
	chain                 []string
}

var knownMiddleware = map[string]bool{
	"AdminMiddleware": true, "AuthMiddleware": true, "DeviceAuthMiddleware": true, "HardwareLoggerMiddleware": true,
}

// mainRoutes reads every mux.HandleFunc/Handle registration in cmd/api/main.go.
func mainRoutes(file string) ([]route, error) {
	fset := gotoken.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		return nil, err
	}
	var routes []route
	var perr error
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") || len(call.Args) != 2 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != gotoken.STRING {
			perr = fmt.Errorf("%s: route pattern is not a string literal", fset.Position(call.Pos()))
			return false
		}
		pattern, _ := strconv.Unquote(lit.Value)
		rt := route{pattern: pattern, path: pattern}
		if m, p, ok := strings.Cut(pattern, " "); ok {
			rt.method, rt.path = m, strings.TrimSpace(p)
		}
		ast.Inspect(call.Args[1], func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok {
				rt.chain = append(rt.chain, s.Sel.Name)
			}
			return true
		})
		routes = append(routes, rt)
		return true
	})
	if perr != nil {
		return nil, perr
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("no routes found in %s", file)
	}
	return routes, nil
}

func (rt route) handler() string {
	if len(rt.chain) == 0 {
		return ""
	}
	return rt.chain[len(rt.chain)-1]
}

func (rt route) has(name string) bool {
	for _, c := range rt.chain {
		if c == name {
			return true
		}
	}
	return false
}

// security is the scheme a route's middleware chain enforces ("" for public).
func (rt route) security() string {
	switch {
	case rt.has("AdminMiddleware"):
		return "AdminJWT"
	case rt.has("AuthMiddleware"):
		return "ParentJWT"
	case rt.has("DeviceAuthMiddleware"), rt.handler() == "ADMSHandler":
		return "DeviceSerial"
	}
	return ""
}

func routeTag(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/admin/"):
		return "Admin Dashboard"
	case strings.HasPrefix(path, "/api/mobile/"):
		return "Parent App"
	case strings.HasPrefix(path, "/iclock/"), strings.HasPrefix(path, "/api/attendance/"):
		return internalTag
	case path == "/health":
		return "System"
	}
	return ""
}

// probe returns the operations the server really serves, keyed "METHOD /path". A route
// registered with a method serves that method. For a route without one, each method is sent
// with valid credentials and counts unless it is answered 404 or 405. ADMSHandler routes ACK
// every method (RULES.md §5), so only POST, the method that stores data, is an operation.
func (r *runner) probe(routes []route) map[string]route {
	served := map[string]route{}
	for _, rt := range routes {
		switch {
		case rt.method != "":
			served[rt.method+" "+rt.path] = rt
			continue
		case rt.handler() == "ADMSHandler":
			served["POST "+rt.path] = rt
			continue
		}
		target, h := rt.path, http.Header{}
		switch rt.security() {
		case "AdminJWT":
			h.Set("Authorization", "Bearer "+r.admin)
		case "ParentJWT":
			h.Set("Authorization", "Bearer "+r.parent)
		case "DeviceSerial":
			target += "?SN=TEST-SN-0001"
		}
		for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
			status, _, _, err := r.srv.send(m, target, h, nil)
			if err != nil {
				r.t.Fatalf("probe %s %s: %v", m, rt.path, err)
			}
			switch {
			case status >= 500:
				r.t.Fatalf("probe %s %s: status %d; the probe needs a healthy server", m, rt.path, status)
			case status != http.StatusNotFound && status != http.StatusMethodNotAllowed:
				served[m+" "+rt.path] = rt
			}
		}
	}
	return served
}

func securityOf(op map[string]any) string {
	sec, ok := op["security"].([]any)
	if !ok {
		return "<missing>"
	}
	if len(sec) == 0 {
		return ""
	}
	if len(sec) == 1 && len(obj(sec[0])) == 1 {
		for name, scopes := range obj(sec[0]) {
			if l, ok := scopes.([]any); ok && len(l) == 0 {
				return name
			}
		}
	}
	return fmt.Sprintf("%v", sec)
}

func routeProblems(s *apiSpec, served map[string]route) []string {
	var problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	specOps := map[string]operation{}
	for _, o := range s.operations() {
		specOps[o.key()] = o
	}
	keys := make([]string, 0, len(served))
	for k := range served {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		rt := served[k]
		if len(rt.chain) == 0 {
			bad("%s (cmd/api/main.go) has no handler the contract test can read", k)
			continue
		}
		for _, c := range rt.chain[:len(rt.chain)-1] {
			if !knownMiddleware[c] {
				bad("%s (cmd/api/main.go) uses middleware %s, which the contract test does not know; add it to knownMiddleware and route.security", k, c)
			}
		}
		if routeTag(rt.path) == "" {
			bad("%s (cmd/api/main.go) has no client class; add its prefix to routeTag", k)
		}
		o, ok := specOps[k]
		if !ok {
			bad("route %s is registered in cmd/api/main.go (%q) but missing from the spec", k, rt.pattern)
			continue
		}
		if got, want := securityOf(o.op), rt.security(); got != want {
			bad("%s: spec security is %q, but the route's middleware (%s) means %q", k, got, strings.Join(rt.chain, " → "), want)
		}
		if tags := list(o.op["tags"]); len(tags) != 1 || str(tags[0]) != routeTag(rt.path) {
			bad("%s: tags are %v, want [%s]", k, tags, routeTag(rt.path))
		}
	}
	for _, o := range s.operations() {
		if _, ok := served[o.key()]; !ok {
			bad("spec operation %s (%s) has no route in cmd/api/main.go", o.key(), o.id)
		}
	}
	return problems
}

func exchangeProblems(s *apiSpec, xs []*exchange) []string {
	var problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	ops := map[string]operation{}
	for _, o := range s.operations() {
		ops[o.key()] = o
	}
	for _, x := range xs {
		where := fmt.Sprintf("%s %s [%s] → %d", strings.SplitN(x.op, " ", 2)[0], x.target, x.name, x.status)
		o, ok := ops[x.op]
		if !ok {
			bad("%s: the operation is not in the spec", where)
			continue
		}
		resp := obj(obj(o.op["responses"])[strconv.Itoa(x.status)])
		if resp == nil {
			bad("%s: status %d is not documented for %s", where, x.status, x.op)
			continue
		}
		problems = append(problems, responseProblems(s, where, resp, x)...)
	}
	return problems
}

// responseProblems checks one recorded exchange against a documented response object.
func responseProblems(s *apiSpec, where string, resp map[string]any, x *exchange) []string {
	var problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	resp, err := s.deref(resp)
	if err != nil {
		return []string{where + ": " + err.Error()}
	}
	content := obj(resp["content"])
	ct := x.header.Get("Content-Type")
	mt, _, _ := mime.ParseMediaType(ct)
	switch media := obj(content[mt]); {
	case len(content) == 0 && len(x.body) == 0:
	case media == nil:
		bad("%s: Content-Type %q is not documented (documented: %v)", where, ct, sortedKeys(content))
	default:
		schema := obj(media["schema"])
		resolved, err := s.deref(schema)
		if err != nil {
			return append(problems, where+": "+err.Error())
		}
		switch {
		case str(resolved["format"]) == "binary":
			if len(x.body) == 0 {
				bad("%s: empty file", where)
			} else if strings.HasPrefix(mt, "image/") {
				if got := http.DetectContentType(x.body); got != mt {
					bad("%s: the body is %s, not the declared %s", where, got, mt)
				}
			}
		case mt == "application/json":
			v, err := decodeJSON(x.body)
			if err != nil {
				bad("%s: body is not JSON: %v", where, err)
				break
			}
			problems = append(problems, s.validate(schema, v, where+" body")...)
		default:
			problems = append(problems, s.validate(schema, string(x.body), where+" body")...)
		}
	}
	headers := obj(resp["headers"])
	for _, name := range sortedKeys(headers) {
		hd, err := s.deref(obj(headers[name]))
		if err != nil {
			bad("%s: header %s: %v", where, name, err)
			continue
		}
		v := x.header.Get(name)
		if v == "" {
			bad("%s: documented header %s is missing", where, name)
			continue
		}
		hs, _ := s.deref(obj(hd["schema"]))
		var hv any = v
		if str(hs["type"]) == "integer" {
			if _, err := strconv.ParseInt(v, 10, 64); err != nil {
				bad("%s: header %s=%q is not an integer", where, name, v)
				continue
			}
			hv = json.Number(v)
		}
		problems = append(problems, s.validate(obj(hd["schema"]), hv, where+" header "+name)...)
	}
	return problems
}

// requestProblems checks every multipart request the server accepted against the operation's
// documented multipart/form-data schema: each part sent is a documented property and every
// required property was sent.
func requestProblems(s *apiSpec, xs []*exchange) []string {
	ops := map[string]operation{}
	for _, o := range s.operations() {
		ops[o.key()] = o
	}
	var problems []string
	for _, x := range xs {
		if x.parts == nil || x.status < 200 || x.status > 299 {
			continue
		}
		where := fmt.Sprintf("%s [%s] request", x.op, x.name)
		o, ok := ops[x.op]
		if !ok {
			problems = append(problems, where+": the operation is not in the spec")
			continue
		}
		rb, err := s.deref(obj(o.op["requestBody"]))
		if err != nil {
			problems = append(problems, where+": "+err.Error())
			continue
		}
		media := obj(obj(rb["content"])["multipart/form-data"])
		if media == nil {
			problems = append(problems, where+": multipart/form-data is not documented")
			continue
		}
		schema, err := s.deref(obj(media["schema"]))
		if err != nil {
			problems = append(problems, where+": "+err.Error())
			continue
		}
		props := obj(schema["properties"])
		sent := map[string]bool{}
		for _, p := range x.parts {
			sent[p] = true
			if props[p] == nil {
				problems = append(problems, fmt.Sprintf("%s: part %q is not a documented field (documented: %v)", where, p, sortedKeys(props)))
			}
		}
		for _, req := range list(schema["required"]) {
			if !sent[str(req)] {
				problems = append(problems, fmt.Sprintf("%s: accepted without the required field %q", where, str(req)))
			}
		}
	}
	return problems
}

func coverageProblems(s *apiSpec, xs []*exchange) []string {
	var problems []string
	for _, o := range s.operations() {
		ok, failed := false, false
		for _, x := range xs {
			if x.op == o.key() {
				ok = ok || (x.status >= 200 && x.status < 300)
				failed = failed || x.status >= 400
			}
		}
		if !ok {
			problems = append(problems, fmt.Sprintf("%s (%s) has no successful case", o.key(), o.id))
		}
		if !failed {
			problems = append(problems, fmt.Sprintf("%s (%s) has no error case", o.key(), o.id))
		}
	}
	return problems
}

func untestedStatuses(s *apiSpec, xs []*exchange) []string {
	hit := map[string]bool{}
	for _, x := range xs {
		hit[x.op+" "+strconv.Itoa(x.status)] = true
	}
	var out []string
	for _, o := range s.operations() {
		for _, code := range sortedKeys(obj(o.op["responses"])) {
			if !hit[o.key()+" "+code] {
				out = append(out, o.key()+" "+code)
			}
		}
	}
	return out
}

func node(root map[string]any, path ...string) map[string]any {
	cur := root
	for _, p := range path {
		cur = obj(cur[p])
		if cur == nil {
			panic("self-test mutation: no " + strings.Join(path, "/"))
		}
	}
	return cur
}

// selfTest breaks copies of the spec in ways a real drift would, and requires the contract
// checks to catch every one against the same recorded exchanges.
func selfTest(t *testing.T, raw []byte, s *apiSpec, served map[string]route, xs, tx []*exchange) {
	checkers := map[string]func(*apiSpec) []string{
		"transport": func(sp *apiSpec) []string { return transportProblems(sp, tx) },
		"hygiene":   hygieneProblems,
		"routes":    func(sp *apiSpec) []string { return routeProblems(sp, served) },
		"responses": func(sp *apiSpec) []string { return exchangeProblems(sp, xs) },
		"requests":  func(sp *apiSpec) []string { return requestProblems(sp, xs) },
		"coverage":  func(sp *apiSpec) []string { return coverageProblems(sp, xs) },
	}
	for name, check := range checkers {
		if p := check(s); len(p) > 0 {
			t.Fatalf("the real spec fails the %s check, so the self-test has no clean baseline; first: %s", name, p[0])
		}
	}
	schemas := func(root map[string]any) map[string]any { return node(root, "components", "schemas") }
	mutations := []struct {
		name     string
		caughtBy []string
		mutate   func(map[string]any)
	}{
		{"an operation is removed", []string{"routes", "responses"}, func(root map[string]any) { delete(node(root, "paths"), "/api/mobile/schedule") }},
		{"an operation has no route", []string{"routes", "coverage"}, func(root map[string]any) {
			op := deepCopy(node(root, "paths", "/api/mobile/students", "get")).(map[string]any)
			op["operationId"] = "patchParentStudents"
			node(root, "paths", "/api/mobile/students")["patch"] = op
		}},
		{"a nullable field is marked non-nullable", []string{"responses"}, func(root map[string]any) {
			delete(node(schemas(root), "DailyAttendanceDTO", "properties", "check_in_time"), "nullable")
		}},
		{"an id becomes a string", []string{"responses"}, func(root map[string]any) {
			node(schemas(root), "Notification", "properties", "id")["type"] = "string"
		}},
		{"a status code is removed", []string{"responses"}, func(root map[string]any) {
			delete(node(root, "paths", "/api/mobile/students", "get", "responses"), "403")
		}},
		{"a returned field is undocumented", []string{"responses"}, func(root map[string]any) {
			ms := node(schemas(root), "MobileStudent")
			delete(obj(ms["properties"]), "avatar_url")
			ms["required"] = []any{"id", "full_name", "grade", "section"}
		}},
		{"a field the server never sends is required", []string{"responses"}, func(root map[string]any) {
			b := node(schemas(root), "Banner")
			node(b, "properties")["priority"] = map[string]any{"type": "integer"}
			b["required"] = append(list(b["required"]), "priority")
		}},
		{"a protected operation is marked public", []string{"routes"}, func(root map[string]any) {
			node(root, "paths", "/api/mobile/banners", "get")["security"] = []any{}
		}},
		{"the punch-time format changes", []string{"responses"}, func(root map[string]any) {
			node(schemas(root), "DailyAttendanceDTO", "properties", "check_in_time")["pattern"] = `^[0-2][0-9]:[0-5][0-9]$`
		}},
		{"an enum loses a value", []string{"responses"}, func(root map[string]any) {
			node(schemas(root), "DailyAttendanceDTO", "properties", "status")["enum"] = []any{"Present", "Absent"}
		}},
		{"a header limit is wrong", []string{"responses"}, func(root map[string]any) {
			node(root, "components", "headers", "RetryAfter", "schema")["maximum"] = json.Number("5")
		}},
		{"a response media type is wrong", []string{"responses"}, func(root map[string]any) {
			hdr := node(root, "components", "responses", "HeaderTooLarge", "content")
			hdr["application/json"] = hdr["text/plain"]
			delete(hdr, "text/plain")
		}},
		{"device-token removal documents 404 instead of 200", []string{"responses"}, func(root map[string]any) {
			responses := node(root, "paths", "/api/mobile/device-token", "delete", "responses")
			responses["404"] = responses["200"]
			delete(responses, "200")
		}},
		{"unread_count is no longer documented", []string{"responses"}, func(root map[string]any) {
			page := node(schemas(root), "NotificationPage")
			delete(obj(page["properties"]), "unread_count")
			page["required"] = []any{"status", "data", "has_more", "next_before"}
		}},
		{"MonthlyRecord documents check_time again", []string{"responses"}, func(root map[string]any) {
			rec := node(schemas(root), "MonthlyRecord")
			props := obj(rec["properties"])
			props["check_time"] = props["check_in_time"]
			delete(props, "check_in_time")
			rec["required"] = []any{"date", "status", "check_time", "check_out_time"}
		}},
		{"createStudent no longer documents 409", []string{"responses"}, func(root map[string]any) {
			delete(node(root, "paths", "/api/admin/students", "post", "responses"), "409")
		}},
		{"admin schedule teacher_name is no longer nullable", []string{"responses"}, func(root map[string]any) {
			delete(node(schemas(root), "AdminScheduleEntry", "properties", "teacher_name"), "nullable")
		}},
		{"createBanner renames its image part", []string{"requests"}, func(root map[string]any) {
			props := node(schemas(root), "BannerCreateRequest", "properties")
			props["file"] = props["image"]
			delete(props, "image")
			node(schemas(root), "BannerCreateRequest")["required"] = []any{"file"}
		}},
		{"the public banner picture no longer documents image/jpeg", []string{"responses"}, func(root map[string]any) {
			delete(node(root, "paths", "/api/mobile/banners/image", "get", "responses", "200", "content"), "image/jpeg")
		}},
		{"cancelLeave documents 404 instead of 200", []string{"responses"}, func(root map[string]any) {
			responses := node(root, "paths", "/api/admin/leaves", "delete", "responses")
			responses["404"] = responses["200"]
			delete(responses, "200")
		}},
		{"production is listed as a server", []string{"hygiene"}, func(root map[string]any) {
			root["servers"] = append(list(root["servers"]), map[string]any{"url": "https://futurekids-production.up.railway.app"})
		}},
		{"the 405 envelope is documented as plain text", []string{"transport"}, func(root map[string]any) {
			content := node(root, "x-transport-responses", "405", "content")
			content["text/plain"] = map[string]any{"schema": map[string]any{"type": "string"}, "example": "Method Not Allowed"}
			delete(content, "application/json")
		}},
		{"an example does not match its schema", []string{"hygiene"}, func(root map[string]any) {
			node(schemas(root), "Student", "example")["id"] = "forty-two"
		}},
	}
	expectCaught := func(name string, sp *apiSpec, caughtBy []string) {
		t.Helper()
		for _, c := range caughtBy {
			if p := checkers[c](sp); len(p) == 0 {
				t.Errorf("broken spec passed the %s check: %s", c, name)
			} else {
				t.Logf("%s → %s check: %d problem(s), e.g. %s", name, c, len(p), p[0])
			}
		}
	}
	for _, m := range mutations {
		root := deepCopy(s.root).(map[string]any)
		m.mutate(root)
		expectCaught(m.name, &apiSpec{root: root}, m.caughtBy)
	}

	broken := strings.Replace(string(raw), "\n  /api/mobile/banners:\n", "\n  /api/mobile/banner:\n", 1)
	if broken == string(raw) {
		t.Fatal("text mutation did not apply")
	}
	bs, err := parseSpec(broken)
	if err != nil {
		t.Fatal(err)
	}
	expectCaught("a path is renamed in the YAML", bs, []string{"routes", "responses"})
}

// TestSpecParsesLikeRedocly compares parseYAML with Redocly's reading of the spec. It runs when
// CONTRACT_SPEC_JSON names the output of `npx @redocly/cli bundle main --ext json`.
func TestSpecParsesLikeRedocly(t *testing.T) {
	ref := os.Getenv("CONTRACT_SPEC_JSON")
	if ref == "" {
		t.Skip("set CONTRACT_SPEC_JSON to a Redocly JSON bundle of the spec to compare parsers")
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", contractSpec))
	if err != nil {
		t.Fatal(err)
	}
	mine, err := parseYAML(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(ref)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := decodeJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(mine, theirs) {
		t.Errorf("parseYAML and Redocly read %s differently", contractSpec)
	}
}
