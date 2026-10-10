package warnings

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"future_kids/internal/auth"
	"future_kids/internal/background"
	"future_kids/internal/config"
	"future_kids/internal/cron"
	"future_kids/internal/handlers"
	"future_kids/internal/testdb"

	"github.com/golang-jwt/jwt/v5"
	excelize "github.com/xuri/excelize/v2"
	"golang.org/x/crypto/bcrypt"
)

// k1Today is the server's today in the K1 tests, a Thursday.
const k1Today = "2026-09-24"

// k1Server starts the real API with FAKE_TODAY=today and returns it, its database and an admin
// token.
func k1Server(t *testing.T, label, today string) (*g3Server, *sql.DB, string) {
	t.Helper()
	db, dsn := setupThrowawayDB(t, label)
	hash, _ := bcrypt.GenerateFromPassword([]byte(a14AdminPassword), bcrypt.DefaultCost)
	if _, err := db.Exec(`UPDATE admins SET password_hash = $1 WHERE username = 'admin'`, string(hash)); err != nil {
		t.Fatal(err)
	}
	srv := g3Start(t, dsn, g3FreePort(t), true, "TRUSTED_PROXY_CIDRS=127.0.0.1/32", "ABSENCE_CRON_SCHEDULE=0 0 1 1 *", "FAKE_TODAY="+today)
	return srv, db, a14AdminToken(t, srv)
}

func k1Closure(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func k1Exec(t *testing.T, db *sql.DB, queries ...string) {
	t.Helper()
	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
}

func k1Count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%v\n%s", err, query)
	}
	return n
}

// k1Snapshot lists every closure, broadcast and notification row.
func k1Snapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT id || '|' || kind || '|' || start_date || '|' || COALESCE(end_date::text, '-') || '|' || title || '|' || COALESCE(notes, '-') || '|' || COALESCE(broadcast_id::text, '-') FROM school_closures ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for rows.Next() {
		var s string
		rows.Scan(&s)
		b.WriteString(s + "\n")
	}
	rows.Close()
	return b.String() + "--\n" + j1Snapshot(t, db)
}

type k1Result struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Data    struct {
		ID             *int    `json:"id"`
		Kind           string  `json:"kind"`
		Action         string  `json:"action"`
		StartDate      string  `json:"start_date"`
		EndDate        *string `json:"end_date"`
		SchoolDays     *int    `json:"school_days"`
		Notified       bool    `json:"notified"`
		RecipientCount int     `json:"recipient_count"`
		DryRun         bool    `json:"dry_run"`
	} `json:"data"`
}

func k1Decode(t *testing.T, r a14Response) k1Result {
	t.Helper()
	var res k1Result
	if err := json.Unmarshal(r.body, &res); err != nil {
		t.Fatalf("decode %d %s: %v", r.status, r.body, err)
	}
	return res
}

func k1Str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// k1Weekdays lists the Sunday-to-Thursday dates from first to last inclusive, skipping the
// dates in skip.
func k1Weekdays(first, last string, skip ...string) []string {
	from, _ := time.Parse("2006-01-02", first)
	to, _ := time.Parse("2006-01-02", last)
	skipped := map[string]bool{}
	for _, s := range skip {
		skipped[s] = true
	}
	var out []string
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		s := d.Format("2006-01-02")
		if d.Weekday() != time.Friday && d.Weekday() != time.Saturday && !skipped[s] {
			out = append(out, s)
		}
	}
	return out
}

// k1Broadcast returns the title and body of the newest broadcast and how many notifications
// link to it.
func k1Broadcast(t *testing.T, db *sql.DB) (int, string, string, int) {
	t.Helper()
	var id, n int
	var title, body string
	if err := db.QueryRow(`SELECT b.id, b.title, b.body, (SELECT COUNT(*) FROM notifications n WHERE n.broadcast_id = b.id) FROM broadcasts b ORDER BY b.id DESC LIMIT 1`).Scan(&id, &title, &body, &n); err != nil {
		t.Fatalf("newest broadcast: %v", err)
	}
	return id, title, body, n
}

// TestK1DayTypes verifies the day rule: Friday and Saturday are the weekend whatever covers
// them, then a pause, then a holiday, otherwise a school day; a closure covers its first and
// last day, a NULL end_date covers every later date, and a one-day holiday covers one date.
func TestK1DayTypes(t *testing.T) {
	db, _ := setupThrowawayDB(t, "k1days")
	k1Exec(t, db, `INSERT INTO school_closures (kind, start_date, end_date, title, notes) VALUES
		('holiday', '2026-03-22', '2026-03-26', 'عطلة الربيع', 'ملاحظة'),
		('holiday', '2026-04-05', '2026-04-05', 'عطلة يوم واحد', NULL),
		('pause', '2026-06-14', NULL, 'العطلة الصيفية', NULL),
		('holiday', '2026-01-04', '2026-01-08', 'عطلة متداخلة', NULL),
		('pause', '2026-01-06', '2026-01-06', 'توقف متداخل', NULL)`)
	for _, tc := range []struct{ date, typ, title, notes string }{
		{"2026-03-19", handlers.DaySchool, "<nil>", "<nil>"},
		{"2026-03-20", handlers.DayWeekend, "<nil>", "<nil>"},
		{"2026-03-21", handlers.DayWeekend, "<nil>", "<nil>"},
		{"2026-03-22", handlers.DayHoliday, "عطلة الربيع", "ملاحظة"},
		{"2026-03-26", handlers.DayHoliday, "عطلة الربيع", "ملاحظة"},
		{"2026-03-29", handlers.DaySchool, "<nil>", "<nil>"},
		{"2026-04-05", handlers.DayHoliday, "عطلة يوم واحد", "<nil>"},
		{"2026-04-06", handlers.DaySchool, "<nil>", "<nil>"},
		{"2026-06-11", handlers.DaySchool, "<nil>", "<nil>"},
		{"2026-06-14", handlers.DayPause, "العطلة الصيفية", "<nil>"},
		{"2026-06-19", handlers.DayWeekend, "<nil>", "<nil>"},
		{"2027-03-02", handlers.DayPause, "العطلة الصيفية", "<nil>"},
		{"2026-01-05", handlers.DayHoliday, "عطلة متداخلة", "<nil>"},
		{"2026-01-06", handlers.DayPause, "توقف متداخل", "<nil>"},
	} {
		day, err := handlers.DayOn(context.Background(), db, tc.date)
		if err != nil {
			t.Fatalf("%s: %v", tc.date, err)
		}
		if got := fmt.Sprint(day.Type, " ", k1Str(day.Title), " ", k1Str(day.Notes)); got != tc.typ+" "+tc.title+" "+tc.notes {
			t.Errorf("%s: %s, want %s %s %s", tc.date, got, tc.typ, tc.title, tc.notes)
		}
		if day.School() != (tc.typ == handlers.DaySchool) {
			t.Errorf("%s: School() = %v", tc.date, day.School())
		}
	}
	if _, err := handlers.DayOn(context.Background(), db, "2026-02-30"); err == nil {
		t.Errorf("an invalid date was accepted")
	}
}

// TestK1CreateHoliday verifies POST /api/admin/holidays: every validation is a 400 naming the
// field and writes nothing; the limits are inclusive; a holiday may start in the past and one
// entirely in the past sends nothing; a pause needs confirm and may not start in the past; every
// overlap is a 409 naming the other closure; concurrent identical requests store one closure;
// and dry_run counts without writing.
func TestK1CreateHoliday(t *testing.T) {
	srv, db, admin := k1Server(t, "k1create", k1Today)
	j1Seed(t, db)
	bearer := a14Bearer(admin)
	create := func(body any) a14Response {
		t.Helper()
		return a14Do(t, srv, "POST", "/api/admin/holidays", bearer, body)
	}
	ar := func(n int) string { return strings.Repeat("ع", n) }
	h := func(kv ...any) map[string]any {
		return k1Closure(append([]any{"kind", "holiday", "start_date", "2026-11-01", "title", "عطلة", "notify", false}, kv...)...)
	}

	t.Run("invalid requests are 400 naming the problem and write nothing", func(t *testing.T) {
		before := k1Snapshot(t, db)
		for _, tc := range []struct {
			name string
			body any
			msg  string
		}{
			{"kind missing", map[string]any{"start_date": "2026-11-01", "title": "x"}, "kind is required"},
			{"kind null", h("kind", nil), "kind is required"},
			{"kind in another case", h("kind", "Holiday"), "kind must be holiday or pause"},
			{"kind a number", h("kind", 1), "kind must be holiday or pause"},
			{"start_date missing", map[string]any{"kind": "holiday", "title": "x"}, "start_date is required"},
			{"start_date null", h("start_date", nil), "start_date is required"},
			{"start_date not zero-padded", h("start_date", "2026-11-1"), "start_date must be formatted as YYYY-MM-DD"},
			{"start_date not a real date", h("start_date", "2026-02-30"), "start_date must be formatted as YYYY-MM-DD"},
			{"start_date with a time", h("start_date", "2026-11-01T00:00:00Z"), "start_date must be formatted as YYYY-MM-DD"},
			{"start_date a number", h("start_date", 20261101), "start_date must be formatted as YYYY-MM-DD"},
			{"start_date in 1999", h("start_date", "1999-12-31"), "start_date must have a year from 2000 to 2100"},
			{"end_date in 2101", h("end_date", "2101-01-01"), "end_date must have a year from 2000 to 2100"},
			{"end_date not a date", h("end_date", "soon"), "end_date must be formatted as YYYY-MM-DD"},
			{"end_date before start_date", h("end_date", "2026-10-31"), "end_date must not be before start_date"},
			{"holiday of 61 days", h("start_date", "2099-01-01", "end_date", "2099-03-02"), "a holiday can be at most 60 days; use a pause for a longer closure"},
			{"title missing", map[string]any{"kind": "holiday", "start_date": "2026-11-01"}, "title is required"},
			{"title blank", h("title", " \t "), "title is required"},
			{"title over 80", h("title", ar(81)), "title must be at most 80 characters"},
			{"title a number", h("title", 5), "title must be a string"},
			{"NUL in title", h("title", "a\x00b"), "title must be text"},
			{"invalid UTF-8 in title", `{"kind":"holiday","start_date":"2026-11-01","title":"` + "\xff\xfe" + `"}`, "title must be text"},
			{"notes over 500", h("notes", ar(501)), "notes must be at most 500 characters"},
			{"notes a number", h("notes", 5), "notes must be a string"},
			{"NUL in notes", h("notes", "a\x00b"), "notes must be text"},
			{"invalid UTF-8 in notes", `{"kind":"holiday","start_date":"2026-11-01","title":"x","notes":"` + "\xc3\x28" + `"}`, "notes must be text"},
			{"notify a string", h("notify", "false"), "notify must be true or false"},
			{"confirm a number", h("confirm", 1), "confirm must be true or false"},
			{"dry_run a string", h("dry_run", "yes"), "dry_run must be true or false"},
			{"pause without confirm", h("kind", "pause"), "confirm must be true for a pause"},
			{"pause with confirm false", h("kind", "pause", "confirm", false), "confirm must be true for a pause"},
			{"pause starting yesterday", h("kind", "pause", "confirm", true, "start_date", "2026-09-23"), "a pause cannot start in the past"},
			{"malformed JSON", "{", "Invalid request body"},
			{"a JSON array", "[]", "Invalid request body"},
		} {
			r := create(tc.body)
			if r.status != 400 || r.json(t)["message"] != tc.msg {
				t.Errorf("%s: %d %s, want 400 %q", tc.name, r.status, r.body, tc.msg)
			}
		}
		e1Error(t, create(`{"title":"`+strings.Repeat("a", int(handlers.MaxJSONBodyBytes))+`"}`), 413, "Request body too large")
		if after := k1Snapshot(t, db); after != before {
			t.Errorf("rejected requests wrote rows\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	t.Run("limits are inclusive and a dry run writes nothing", func(t *testing.T) {
		before := k1Snapshot(t, db)
		for _, tc := range []struct {
			name string
			body map[string]any
			days int
		}{
			{"exactly 60 days", h("start_date", "2099-01-01", "end_date", "2099-03-01", "dry_run", true), len(k1Weekdays("2099-01-01", "2099-03-01"))},
			{"title of 80 Arabic characters", h("title", ar(80), "dry_run", true), 1},
			{"padded title of 80", h("title", "  "+ar(80)+"  ", "dry_run", true), 1},
			{"notes of 500 Arabic characters", h("notes", ar(500), "dry_run", true), 1},
			{"a pause dry run needs no confirm", h("kind", "pause", "start_date", "2027-06-01", "end_date", nil, "dry_run", true), -1},
		} {
			r := create(tc.body)
			if r.status != 200 {
				t.Errorf("%s: %d %s", tc.name, r.status, r.body)
				continue
			}
			res := k1Decode(t, r)
			days := -1
			if res.Data.SchoolDays != nil {
				days = *res.Data.SchoolDays
			}
			if res.Message != "Preview only" || res.Data.ID != nil || !res.Data.DryRun || days != tc.days {
				t.Errorf("%s: %s, want a preview with %d school days", tc.name, r.body, tc.days)
			}
		}
		r := create(h("start_date", "2028-03-05", "end_date", "2028-03-16", "notify", nil, "dry_run", true))
		res := k1Decode(t, r)
		if r.status != 200 || !res.Data.Notified || res.Data.RecipientCount != 6 || *res.Data.SchoolDays != 10 || *res.Data.EndDate != "2028-03-16" {
			t.Errorf("preview with a notification: %d %s; want 6 recipients and 10 school days", r.status, r.body)
		}
		if after := k1Snapshot(t, db); after != before {
			t.Errorf("dry runs wrote rows")
		}
	})

	t.Run("a one-day holiday stores end_date = start_date", func(t *testing.T) {
		r := create(h())
		res := k1Decode(t, r)
		if r.status != 200 || res.Message != "Closure registered" || res.Data.ID == nil || res.Data.Kind != "holiday" || k1Str(res.Data.EndDate) != "2026-11-01" || *res.Data.SchoolDays != 1 || res.Data.Notified || res.Data.RecipientCount != 0 || res.Data.DryRun {
			t.Fatalf("one-day holiday: %d %s", r.status, r.body)
		}
		var stored string
		db.QueryRow(`SELECT kind || ' ' || start_date || ' ' || end_date || ' ' || title || ' ' || (notes IS NULL) || ' ' || (broadcast_id IS NULL) FROM school_closures WHERE id = $1`, *res.Data.ID).Scan(&stored)
		if stored != "holiday 2026-11-01 2026-11-01 عطلة true true" {
			t.Errorf("stored %q", stored)
		}
	})

	t.Run("a holiday in the past corrects reports and sends nothing", func(t *testing.T) {
		broadcasts := k1Count(t, db, `SELECT COUNT(*) FROM broadcasts`)
		r := create(k1Closure("kind", "holiday", "start_date", "2026-09-01", "end_date", "2026-09-03", "title", "عطلة سابقة"))
		res := k1Decode(t, r)
		if r.status != 200 || res.Data.ID == nil || res.Data.Notified || res.Data.RecipientCount != 0 {
			t.Fatalf("past holiday: %d %s", r.status, r.body)
		}
		if n := k1Count(t, db, `SELECT COUNT(*) FROM broadcasts`); n != broadcasts {
			t.Errorf("a holiday entirely in the past sent a notification")
		}
		day := a14Do(t, srv, "GET", "/api/admin/attendance?date=2026-09-02", bearer, nil).json(t)
		if day["day_type"] != "holiday" || day["day_title"] != "عطلة سابقة" {
			t.Errorf("report for a past holiday date: %v", day)
		}
		r = create(k1Closure("kind", "holiday", "start_date", "2026-09-20", "end_date", k1Today, "title", "عطلة حتى اليوم"))
		if res := k1Decode(t, r); r.status != 200 || !res.Data.Notified || res.Data.RecipientCount != 6 {
			t.Errorf("a holiday that started in the past and covers today is notified: %d %s", r.status, r.body)
		}
	})

	t.Run("every overlap is 409 naming the other closure and writes nothing", func(t *testing.T) {
		r := create(h("start_date", "2026-12-06", "end_date", "2026-12-10", "title", "عطلة الشتاء"))
		winter := k1Decode(t, r).Data.ID
		r = create(h("kind", "pause", "confirm", true, "start_date", "2027-06-01", "title", "العطلة الصيفية"))
		summer := k1Decode(t, r).Data.ID
		if winter == nil || summer == nil {
			t.Fatalf("setup: %s", r.body)
		}
		winterMsg := fmt.Sprintf("Overlaps closure %d %q (2026-12-06 to 2026-12-10); cancel it first or choose other dates", *winter, "عطلة الشتاء")
		summerMsg := fmt.Sprintf("Overlaps closure %d %q (from 2027-06-01, open-ended); cancel it first or choose other dates", *summer, "العطلة الصيفية")
		before := k1Snapshot(t, db)
		for _, tc := range []struct {
			name string
			body map[string]any
			msg  string
		}{
			{"inside", h("start_date", "2026-12-07", "end_date", "2026-12-08"), winterMsg},
			{"around", h("start_date", "2026-12-01", "end_date", "2026-12-20"), winterMsg},
			{"over its first day", h("start_date", "2026-12-03", "end_date", "2026-12-06"), winterMsg},
			{"over its last day", h("start_date", "2026-12-10", "end_date", "2026-12-12"), winterMsg},
			{"the same dates", h("start_date", "2026-12-06", "end_date", "2026-12-10"), winterMsg},
			{"one day inside", h("start_date", "2026-12-08"), winterMsg},
			{"a pause around it", h("kind", "pause", "confirm", true, "start_date", "2026-12-01", "end_date", "2027-01-31"), winterMsg},
			{"an open-ended pause before it", h("kind", "pause", "confirm", true, "start_date", "2026-11-15"), winterMsg},
			{"years after an open-ended pause", h("start_date", "2030-01-06"), summerMsg},
			{"on the open-ended pause's first day", h("start_date", "2027-06-01"), summerMsg},
			{"a pause starting before the open-ended one", h("kind", "pause", "confirm", true, "start_date", "2027-01-03"), summerMsg},
			{"a dry run that overlaps", h("start_date", "2026-12-07", "dry_run", true), winterMsg},
		} {
			r := create(tc.body)
			if r.status != 409 || r.json(t)["message"] != tc.msg {
				t.Errorf("%s: %d %s, want 409 %q", tc.name, r.status, r.body, tc.msg)
			}
		}
		if after := k1Snapshot(t, db); after != before {
			t.Errorf("an overlap wrote rows")
		}
		for _, tc := range []struct {
			name string
			body map[string]any
		}{
			{"the Friday after it", h("start_date", "2026-12-11", "dry_run", true)},
			{"the Saturday before it", h("start_date", "2026-12-05", "dry_run", true)},
			{"a pause ending the day before the open-ended one", h("kind", "pause", "start_date", "2027-01-03", "end_date", "2027-05-31", "dry_run", true)},
		} {
			if r := create(tc.body); r.status != 200 {
				t.Errorf("%s: %d %s", tc.name, r.status, r.body)
			}
		}
	})

	t.Run("20 concurrent identical creates store one closure", func(t *testing.T) {
		body := h("start_date", "2027-03-07", "end_date", "2027-03-11", "title", "متزامن")
		var wg sync.WaitGroup
		var mu sync.Mutex
		codes := map[int]int{}
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, _, err := e1Send(srv, "POST", "/api/admin/holidays", admin, body)
				if err != nil {
					code = -1
				}
				mu.Lock()
				codes[code]++
				mu.Unlock()
			}()
		}
		wg.Wait()
		if codes[200] != 1 || codes[409] != 19 {
			t.Errorf("status counts %v, want one 200 and nineteen 409", codes)
		}
		if n := k1Count(t, db, `SELECT COUNT(*) FROM school_closures WHERE title = 'متزامن'`); n != 1 {
			t.Errorf("%d closures stored, want 1", n)
		}
	})
}

// TestK1HolidayNotifications verifies the notification a new closure sends: one broadcast to
// every parent in the closure's transaction, linked through broadcast_id, with the notes or the
// fixed Arabic text; nothing with notify false or without parents; and a failure rolls back the
// closure, the broadcast and every notification together.
func TestK1HolidayNotifications(t *testing.T) {
	srv, db, admin := k1Server(t, "k1notify", k1Today)
	j1Seed(t, db)
	bearer := a14Bearer(admin)
	create := func(body any) a14Response {
		t.Helper()
		return a14Do(t, srv, "POST", "/api/admin/holidays", bearer, body)
	}

	t.Run("every parent gets one unread notification linked to the closure", func(t *testing.T) {
		r := create(k1Closure("kind", "holiday", "start_date", "2026-10-11", "end_date", "2026-10-15", "title", " عطلة الخريف ", "notes", "  نتمنى لكم عطلة سعيدة  "))
		res := k1Decode(t, r)
		if r.status != 200 || !res.Data.Notified || res.Data.RecipientCount != 6 {
			t.Fatalf("create: %d %s", r.status, r.body)
		}
		var linked int
		db.QueryRow(`SELECT broadcast_id FROM school_closures WHERE id = $1`, *res.Data.ID).Scan(&linked)
		var row string
		db.QueryRow(`SELECT title || '|' || body || '|' || audience_type || '|' || recipient_count FROM broadcasts WHERE id = $1`, linked).Scan(&row)
		if row != "عطلة الخريف|نتمنى لكم عطلة سعيدة|all|6" {
			t.Errorf("broadcast %d: %q", linked, row)
		}
		rows, _ := db.Query(`SELECT parent_id, title, body, COALESCE(is_read, false) FROM notifications WHERE broadcast_id = $1 ORDER BY parent_id`, linked)
		var got []string
		for rows.Next() {
			var pid int
			var title, body string
			var read bool
			rows.Scan(&pid, &title, &body, &read)
			got = append(got, fmt.Sprintf("%d %s %s %v", pid, title, body, read))
		}
		rows.Close()
		var want []string
		for _, p := range []int{3001, 3002, 3003, 3004, 3005, 3006} {
			want = append(want, fmt.Sprintf("%d عطلة الخريف نتمنى لكم عطلة سعيدة false", p))
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("notifications\n got %v\nwant %v", got, want)
		}
		log := a14Do(t, srv, "GET", "/api/admin/broadcasts", bearer, nil).json(t)
		if first := log["data"].([]any)[0].(map[string]any); first["id"] != float64(linked) || first["recipient_count"] != float64(6) {
			t.Errorf("sent log: %v", first)
		}
		list := a14Do(t, srv, "GET", "/api/mobile/notifications", j1ParentToken(t, 3005, "+9647000003005"), nil).json(t)
		if first := list["data"].([]any)[0].(map[string]any); first["title"] != "عطلة الخريف" || first["body"] != "نتمنى لكم عطلة سعيدة" || first["is_read"] != false || list["unread_count"] != float64(1) {
			t.Errorf("parent without children: %v", list)
		}
	})

	t.Run("without notes the body is a fixed Arabic sentence", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			body map[string]any
			want string
		}{
			{"one-day holiday", k1Closure("kind", "holiday", "start_date", "2026-11-01", "title", "يوم وطني"), "عطلة رسمية يوم 2026-11-01"},
			{"holiday range", k1Closure("kind", "holiday", "start_date", "2026-11-08", "end_date", "2026-11-12", "title", "عطلة", "notes", "   "), "عطلة رسمية من 2026-11-08 إلى 2026-11-12"},
			{"open-ended pause", k1Closure("kind", "pause", "start_date", "2027-06-01", "title", "العطلة الصيفية", "confirm", true), "تعطيل الدوام اعتباراً من 2027-06-01"},
			{"pause with an end", k1Closure("kind", "pause", "start_date", "2027-01-03", "end_date", "2027-02-28", "title", "توقف", "confirm", true, "notify", true), "تعطيل الدوام اعتباراً من 2027-01-03 حتى 2027-02-28"},
		} {
			r := create(tc.body)
			res := k1Decode(t, r)
			if r.status != 200 || !res.Data.Notified {
				t.Errorf("%s: %d %s", tc.name, r.status, r.body)
				continue
			}
			var body string
			var n int
			db.QueryRow(`SELECT b.body, b.recipient_count FROM school_closures c JOIN broadcasts b ON b.id = c.broadcast_id WHERE c.id = $1`, *res.Data.ID).Scan(&body, &n)
			if body != tc.want || n != 6 {
				t.Errorf("%s: body %q to %d parents, want %q to 6", tc.name, body, n, tc.want)
			}
		}
	})

	t.Run("notify false sends nothing", func(t *testing.T) {
		broadcasts := k1Count(t, db, `SELECT COUNT(*) FROM broadcasts`)
		r := create(k1Closure("kind", "holiday", "start_date", "2026-11-15", "title", "صامت", "notify", false))
		res := k1Decode(t, r)
		if r.status != 200 || res.Data.Notified || res.Data.RecipientCount != 0 || k1Count(t, db, `SELECT COUNT(*) FROM broadcasts`) != broadcasts {
			t.Errorf("notify false: %d %s", r.status, r.body)
		}
	})

	t.Run("a failed notification rolls back the closure too", func(t *testing.T) {
		before := k1Snapshot(t, db)
		k1Exec(t, db, `ALTER TABLE notifications ADD CONSTRAINT k1_refuse_poison CHECK (body <> 'Poison')`)
		defer db.Exec(`ALTER TABLE notifications DROP CONSTRAINT k1_refuse_poison`)
		e1Error(t, create(k1Closure("kind", "holiday", "start_date", "2026-11-22", "title", "Poison test", "notes", "Poison")), 500, "Failed to register closure")
		if after := k1Snapshot(t, db); after != before {
			t.Errorf("a failed create changed the tables\nbefore:\n%s\nafter:\n%s", before, after)
		}
		if srv.out.index("AdminHolidaysHandler: create failed") < 0 {
			t.Errorf("the 500 was not logged")
		}
	})

	t.Run("each create is logged without its text", func(t *testing.T) {
		out := srv.out.String()
		if !strings.Contains(out, `"msg":"Closure registered"`) || strings.Contains(out, "عطلة الخريف") || strings.Contains(out, "نتمنى لكم") {
			t.Errorf("create logs must exist and must not contain the text")
		}
	})

	t.Run("without parents the closure is still created", func(t *testing.T) {
		empty, emptyDB, emptyAdmin := k1Server(t, "k1empty", k1Today)
		for _, body := range []map[string]any{
			k1Closure("kind", "holiday", "start_date", "2026-10-11", "title", "عطلة", "dry_run", true),
			k1Closure("kind", "holiday", "start_date", "2026-10-11", "title", "عطلة"),
		} {
			r := a14Do(t, empty, "POST", "/api/admin/holidays", a14Bearer(emptyAdmin), body)
			if res := k1Decode(t, r); r.status != 200 || res.Data.Notified || res.Data.RecipientCount != 0 {
				t.Errorf("no parents: %d %s", r.status, r.body)
			}
		}
		if n := k1Count(t, emptyDB, `SELECT COUNT(*) FROM school_closures WHERE broadcast_id IS NULL`); n != 1 || k1Count(t, emptyDB, `SELECT COUNT(*) FROM broadcasts`) != 0 {
			t.Errorf("closures %d, broadcasts %d; want 1 and 0", n, k1Count(t, emptyDB, `SELECT COUNT(*) FROM broadcasts`))
		}
	})
}

// TestK1Cancel verifies POST /api/admin/holidays/cancel: "cancel from today onward" deletes a
// closure that has not started or started today, shortens a running one to yesterday without
// rewriting past days, refuses one already over; the notification texts (resuming on the next
// Sunday-to-Thursday, also when cancelled on a Saturday); notify false; dry_run; validation.
func TestK1Cancel(t *testing.T) {
	srv, db, admin := k1Server(t, "k1cancel", k1Today)
	j1Seed(t, db)
	bearer := a14Bearer(admin)
	create := func(kv ...any) int {
		t.Helper()
		r := a14Do(t, srv, "POST", "/api/admin/holidays", bearer, k1Closure(append([]any{"notify", false}, kv...)...))
		res := k1Decode(t, r)
		if r.status != 200 || res.Data.ID == nil {
			t.Fatalf("create %v: %d %s", kv, r.status, r.body)
		}
		return *res.Data.ID
	}
	cancel := func(body any) a14Response {
		t.Helper()
		return a14Do(t, srv, "POST", "/api/admin/holidays/cancel", bearer, body)
	}
	ar := func(n int) string { return strings.Repeat("ع", n) }
	future := create("kind", "holiday", "start_date", "2026-10-11", "end_date", "2026-10-15", "title", "عطلة الخريف")
	running := create("kind", "holiday", "start_date", "2026-09-21", "end_date", "2026-09-27", "title", "عطلة طارئة")
	past := create("kind", "holiday", "start_date", "2026-09-01", "end_date", "2026-09-03", "title", "عطلة سابقة")

	t.Run("invalid requests are 400 or 404 and change nothing", func(t *testing.T) {
		before := k1Snapshot(t, db)
		for _, tc := range []struct {
			name   string
			body   any
			status int
			msg    string
		}{
			{"id missing", map[string]any{}, 400, "id is required"},
			{"id null", map[string]any{"id": nil}, 400, "id is required"},
			{"id zero", map[string]any{"id": 0}, 400, "id must be a positive closure id"},
			{"id negative", map[string]any{"id": -1}, 400, "id must be a positive closure id"},
			{"id a string", map[string]any{"id": fmt.Sprint(future)}, 400, "id must be a positive closure id"},
			{"id a fraction", map[string]any{"id": 1.5}, 400, "id must be a positive closure id"},
			{"notify a string", map[string]any{"id": future, "notify": "no"}, 400, "notify must be true or false"},
			{"notes over 500", map[string]any{"id": future, "notes": ar(501)}, 400, "notes must be at most 500 characters"},
			{"NUL in notes", map[string]any{"id": future, "notes": "a\x00b"}, 400, "notes must be text"},
			{"dry_run a number", map[string]any{"id": future, "dry_run": 1}, 400, "dry_run must be true or false"},
			{"malformed JSON", "{", 400, "Invalid request body"},
			{"unknown id", map[string]any{"id": 999999}, 404, "Closure not found"},
			{"unknown id in a dry run", map[string]any{"id": 999999, "dry_run": true}, 404, "Closure not found"},
			{"already over", map[string]any{"id": past}, 409, "This closure is already over"},
			{"already over in a dry run", map[string]any{"id": past, "dry_run": true}, 409, "This closure is already over"},
		} {
			e1Error(t, cancel(tc.body), tc.status, tc.msg)
		}
		e1Error(t, cancel(`{"notes":"`+strings.Repeat("a", int(handlers.MaxJSONBodyBytes))+`"}`), 413, "Request body too large")
		if after := k1Snapshot(t, db); after != before {
			t.Errorf("rejected cancels changed rows")
		}
	})

	t.Run("a dry run reports the action and writes nothing", func(t *testing.T) {
		before := k1Snapshot(t, db)
		r := cancel(map[string]any{"id": running, "dry_run": true})
		res := k1Decode(t, r)
		if r.status != 200 || res.Message != "Preview only" || res.Data.Action != "shortened" || k1Str(res.Data.EndDate) != "2026-09-23" || !res.Data.Notified || res.Data.RecipientCount != 6 || !res.Data.DryRun {
			t.Errorf("dry run: %d %s", r.status, r.body)
		}
		if after := k1Snapshot(t, db); after != before {
			t.Errorf("a dry run changed rows")
		}
	})

	t.Run("a running closure ends yesterday and past days stay closed", func(t *testing.T) {
		reports := func() []string {
			var out []string
			for _, d := range []string{"2026-09-21", "2026-09-22", "2026-09-23"} {
				out = append(out, string(a14Do(t, srv, "GET", "/api/admin/attendance?date="+d, bearer, nil).body))
			}
			return out
		}
		before := reports()
		if !strings.Contains(before[0], `"day_type":"holiday"`) {
			t.Fatalf("setup: %s", before[0])
		}
		r := cancel(map[string]any{"id": running})
		res := k1Decode(t, r)
		if r.status != 200 || res.Message != "Closure cancelled" || res.Data.Action != "shortened" || k1Str(res.Data.EndDate) != "2026-09-23" || !res.Data.Notified || res.Data.RecipientCount != 6 || res.Data.DryRun || res.Data.ID == nil || *res.Data.ID != running {
			t.Fatalf("cancel: %d %s", r.status, r.body)
		}
		var end string
		db.QueryRow(`SELECT end_date::text FROM school_closures WHERE id = $1`, running).Scan(&end)
		if end != "2026-09-23" {
			t.Errorf("stored end_date %s", end)
		}
		if after := reports(); strings.Join(after, "\n") != strings.Join(before, "\n") {
			t.Errorf("past reports changed\nbefore %v\nafter  %v", before, after)
		}
		if today := a14Do(t, srv, "GET", "/api/admin/attendance", bearer, nil).json(t); today["day_type"] != "school" {
			t.Errorf("today after the cancel: %v", today)
		}
		_, title, body, n := k1Broadcast(t, db)
		if title != "تم إنهاء العطلة واستئناف الدوام" || body != "عاد الدوام اعتباراً من 2026-09-24: عطلة طارئة" || n != 6 {
			t.Errorf("notification %q %q to %d", title, body, n)
		}
		e1Error(t, cancel(map[string]any{"id": running}), 409, "This closure is already over")
	})

	t.Run("a closure that started today is deleted; notes follow a blank line", func(t *testing.T) {
		pause := create("kind", "pause", "start_date", k1Today, "end_date", "2026-09-30", "title", "توقف", "confirm", true)
		r := cancel(map[string]any{"id": pause, "notes": "  نلتقي غداً  "})
		res := k1Decode(t, r)
		if r.status != 200 || res.Data.Action != "deleted" || res.Data.EndDate != nil || !res.Data.Notified {
			t.Fatalf("cancel: %d %s", r.status, r.body)
		}
		if k1Count(t, db, `SELECT COUNT(*) FROM school_closures WHERE id = $1`, pause) != 0 {
			t.Errorf("the closure is still stored")
		}
		_, title, body, _ := k1Broadcast(t, db)
		if title != "تم إنهاء العطلة واستئناف الدوام" || body != "عاد الدوام اعتباراً من 2026-09-24: توقف\n\nنلتقي غداً" {
			t.Errorf("notification %q %q", title, body)
		}
	})

	t.Run("a closure that has not started is deleted", func(t *testing.T) {
		r := cancel(map[string]any{"id": future})
		res := k1Decode(t, r)
		if r.status != 200 || res.Data.Action != "deleted" || res.Data.EndDate != nil || k1Count(t, db, `SELECT COUNT(*) FROM school_closures WHERE id = $1`, future) != 0 {
			t.Fatalf("cancel: %d %s", r.status, r.body)
		}
		_, title, body, n := k1Broadcast(t, db)
		if title != "تم إلغاء العطلة" || body != "تم إلغاء: عطلة الخريف" || n != 6 {
			t.Errorf("notification %q %q to %d", title, body, n)
		}
		e1Error(t, cancel(map[string]any{"id": future}), 404, "Closure not found")
	})

	t.Run("notify false sends nothing", func(t *testing.T) {
		id := create("kind", "holiday", "start_date", "2026-12-06", "end_date", "2026-12-10", "title", "عطلة الشتاء")
		broadcasts := k1Count(t, db, `SELECT COUNT(*) FROM broadcasts`)
		r := cancel(map[string]any{"id": id, "notify": false})
		if res := k1Decode(t, r); r.status != 200 || res.Data.Notified || res.Data.RecipientCount != 0 || k1Count(t, db, `SELECT COUNT(*) FROM broadcasts`) != broadcasts {
			t.Errorf("notify false: %d %s", r.status, r.body)
		}
	})

	t.Run("a long title is shortened inside the body, then the notes", func(t *testing.T) {
		id := create("kind", "holiday", "start_date", "2027-01-10", "title", ar(80))
		if r := cancel(map[string]any{"id": id, "notes": ar(450)}); r.status != 200 {
			t.Fatalf("cancel: %d %s", r.status, r.body)
		}
		_, _, body, _ := k1Broadcast(t, db)
		if want := "تم إلغاء: " + ar(37) + "…" + "\n\n" + ar(450); body != want || utf8.RuneCountInString(body) != 500 {
			t.Errorf("body of %d characters %q", utf8.RuneCountInString(body), body)
		}
		id = create("kind", "holiday", "start_date", "2027-01-11", "title", ar(80))
		if r := cancel(map[string]any{"id": id, "notes": ar(500)}); r.status != 200 {
			t.Fatalf("cancel: %d %s", r.status, r.body)
		}
		_, _, body, _ = k1Broadcast(t, db)
		if !strings.HasPrefix(body, "تم إلغاء: …\n\n"+ar(10)) || !strings.HasSuffix(body, "ع…") || utf8.RuneCountInString(body) != 500 {
			t.Errorf("body of %d characters %q", utf8.RuneCountInString(body), body)
		}
	})

	t.Run("cancelled on a Saturday, school resumes on Sunday", func(t *testing.T) {
		sat, satDB, satAdmin := k1Server(t, "k1sat", "2026-09-26")
		j1Seed(t, satDB)
		r := a14Do(t, sat, "POST", "/api/admin/holidays", a14Bearer(satAdmin), k1Closure("kind", "holiday", "start_date", "2026-09-20", "end_date", "2026-09-30", "title", "عطلة", "notify", false))
		id := k1Decode(t, r).Data.ID
		if id == nil {
			t.Fatalf("create: %d %s", r.status, r.body)
		}
		r = a14Do(t, sat, "POST", "/api/admin/holidays/cancel", a14Bearer(satAdmin), map[string]any{"id": *id})
		if res := k1Decode(t, r); r.status != 200 || res.Data.Action != "shortened" || k1Str(res.Data.EndDate) != "2026-09-25" {
			t.Fatalf("cancel: %d %s", r.status, r.body)
		}
		_, title, body, n := k1Broadcast(t, satDB)
		if title != "تم إنهاء العطلة واستئناف الدوام" || body != "عاد الدوام اعتباراً من 2026-09-27: عطلة" || n != 6 {
			t.Errorf("notification %q %q to %d", title, body, n)
		}
	})
}

// TestK1Push verifies the push after a closure's commit reaches exactly the devices of the
// parents, none for a holiday entirely in the past or a dry run, and no log line holds a token.
func TestK1Push(t *testing.T) {
	capture := &w10Capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	db, _ := setupThrowawayDB(t, "k1push")
	pinToday(t, k1Today+" 10:00")
	fcm := newB1FakeFCM(t)
	app := &handlers.AppEnv{DB: db, FCMClient: fcm.client(t), Background: &background.Group{}}
	auth.InitAuth("k1-test-secret")
	token, _ := auth.GenerateAdminToken("admin", 0)
	a1, a2, b := b1Token("k1-a1"), b1Token("k1-a2"), b1Token("k1-b")
	k1Exec(t, db,
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (3201, 'K1 A', '+9647000003201', 'x'), (3202, 'K1 B', '+9647000003202', 'x'), (3203, 'K1 C', '+9647000003203', 'x')`,
		`INSERT INTO device_tokens (parent_id, token) VALUES (3201, '`+a1+`'), (3201, '`+a2+`'), (3202, '`+b+`')`)
	post := func(t *testing.T, h http.HandlerFunc, path string, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := serve(t, app.AdminMiddleware(h), "POST", path, token, string(raw))
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if pending, err := app.Background.Wait(ctx); err != nil {
			t.Fatalf("background work still running: %d", pending)
		}
		var m map[string]any
		json.Unmarshal(rec.Body.Bytes(), &m)
		return m["data"].(map[string]any)
	}
	every := []string{a1, a2, b}
	sort.Strings(every)

	var id float64
	t.Run("a new closure pushes to every device of every parent", func(t *testing.T) {
		d := post(t, app.AdminHolidaysHandler, "/api/admin/holidays", k1Closure("kind", "holiday", "start_date", "2026-10-11", "title", "عطلة"))
		id = d["id"].(float64)
		if got := fcm.take(); strings.Join(got, ",") != strings.Join(every, ",") {
			t.Errorf("pushed to %v, want %v", got, every)
		}
	})

	t.Run("nothing for a holiday entirely in the past or a dry run", func(t *testing.T) {
		post(t, app.AdminHolidaysHandler, "/api/admin/holidays", k1Closure("kind", "holiday", "start_date", "2026-09-01", "title", "سابقة"))
		post(t, app.AdminHolidaysHandler, "/api/admin/holidays", k1Closure("kind", "holiday", "start_date", "2026-12-06", "title", "معاينة", "dry_run", true))
		post(t, app.AdminCancelHolidayHandler, "/api/admin/holidays/cancel", map[string]any{"id": id, "dry_run": true})
		if got := fcm.take(); len(got) != 0 {
			t.Errorf("pushed to %v", got)
		}
	})

	t.Run("a cancel pushes to every device", func(t *testing.T) {
		post(t, app.AdminCancelHolidayHandler, "/api/admin/holidays/cancel", map[string]any{"id": id})
		if got := fcm.take(); strings.Join(got, ",") != strings.Join(every, ",") {
			t.Errorf("pushed to %v, want %v", got, every)
		}
	})

	t.Run("no log line holds a full token", func(t *testing.T) {
		for _, l := range capture.snapshot() {
			line := l.Msg + fmt.Sprint(l.Attrs)
			for _, tok := range every {
				if strings.Contains(line, tok) {
					t.Fatalf("log %q contains a token", line)
				}
			}
		}
	})
}

// TestK1Effects verifies what closures change: the parent month views skip closed days (H1 still
// applies), the admin report, the dashboard and the Excel export are empty with zero counts and
// the day's type on a weekend, holiday or pause day and unchanged on a school day, the parent's
// today is empty on a closed day, and GET /api/mobile/holidays lists the closed school weekdays.
func TestK1Effects(t *testing.T) {
	db, _ := setupThrowawayDB(t, "k1effects")
	k1Exec(t, db,
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1, 'K1 Parent', '+9647000004001', 'x'), (2, 'K1 Late Parent', '+9647000004002', 'x')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id, grade, section, created_at) VALUES
			(1, 'K1 Kid One', 'K1-1', 1, 'G1', 'A', '2026-01-01 08:00'), (2, 'K1 Kid Two', 'K1-2', 1, 'G1', 'A', '2026-01-01 08:00'),
			(3, 'K1 Late', 'K1-3', 2, 'G1', 'A', '2026-03-24 10:00')`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('K1-DEV', 'Gate', true)`,
		`INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (1, 'K1-DEV', '2026-03-18 07:15'), (1, 'K1-DEV', '2026-03-18 12:30'),
			(1, 'K1-DEV', '2026-03-23 07:15'), (1, 'K1-DEV', '2026-03-21 07:15'), (1, 'K1-DEV', '2026-06-15 07:15')`,
		`INSERT INTO student_leaves (student_id, leave_date) VALUES (2, '2026-03-24')`,
		`INSERT INTO school_closures (kind, start_date, end_date, title, notes) VALUES
			('holiday', '2026-03-22', '2026-03-26', 'عطلة الربيع', 'ملاحظة'),
			('holiday', '2026-04-29', '2026-05-03', 'عطلة مايو', NULL),
			('pause', '2026-06-14', NULL, 'العطلة الصيفية', NULL),
			('holiday', '2026-01-04', '2026-01-08', 'عطلة متداخلة', NULL),
			('pause', '2026-01-06', '2026-01-06', 'توقف متداخل', NULL)`)
	auth.InitAuth("k1-effects-secret")
	parent, _ := auth.GenerateParentToken(1, "+9647000004001", 0)
	late, _ := auth.GenerateParentToken(2, "+9647000004002", 0)
	admin, _ := auth.GenerateAdminToken("admin", 0)
	app := &handlers.AppEnv{DB: db}
	get := func(t *testing.T, h http.HandlerFunc, target, token string) map[string]any {
		t.Helper()
		rec := serve(t, h, "GET", target, token, "")
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || rec.Code != 200 {
			t.Fatalf("%s: %d %s", target, rec.Code, rec.Body.String())
		}
		return m
	}
	dayOf := func(m map[string]any) string {
		return fmt.Sprint(m["day_type"], " ", m["day_title"], " ", m["day_notes"])
	}

	t.Run("the month views skip closed days and keep the start-date rule", func(t *testing.T) {
		pinToday(t, "2026-03-31 13:00")
		march := k1Weekdays("2026-03-01", "2026-03-31", "2026-03-22", "2026-03-23", "2026-03-24", "2026-03-25", "2026-03-26")
		monthly := get(t, app.AuthMiddleware(app.MobileMonthlyAttendanceHandler), "/api/mobile/attendance/monthly?month=2026-03", parent)
		for _, s := range monthly["data"].([]any) {
			var dates []string
			statuses := map[string]string{}
			for _, r := range s.(map[string]any)["records"].([]any) {
				rec := r.(map[string]any)
				dates = append(dates, rec["date"].(string))
				statuses[rec["date"].(string)] = rec["status"].(string)
			}
			sort.Strings(dates)
			if strings.Join(dates, ",") != strings.Join(march, ",") {
				t.Errorf("student %v dates %v, want %v", s.(map[string]any)["student_id"], dates, march)
			}
			if s.(map[string]any)["student_id"] == float64(1) && statuses["2026-03-18"] != "Present" {
				t.Errorf("the school-day punch is not Present")
			}
		}
		summary := get(t, app.AuthMiddleware(app.MobileAttendanceSummaryHandler), "/api/mobile/attendance/summary?month=2026-03", parent)
		var got []string
		for _, s := range summary["data"].([]any) {
			m := s.(map[string]any)
			got = append(got, fmt.Sprintf("%v %v/%v/%v", m["student_id"], m["total_present"], m["total_excused"], m["total_absent"]))
		}
		want := []string{fmt.Sprintf("1 1/0/%d", len(march)-1), fmt.Sprintf("2 0/0/%d", len(march))}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("summary %v, want %v (the leave and punch on holiday dates are not counted)", got, want)
		}
		lateMonthly := get(t, app.AuthMiddleware(app.MobileMonthlyAttendanceHandler), "/api/mobile/attendance/monthly?month=2026-03", late)
		var lateDates []string
		for _, r := range lateMonthly["data"].([]any)[0].(map[string]any)["records"].([]any) {
			lateDates = append(lateDates, r.(map[string]any)["date"].(string))
		}
		if strings.Join(lateDates, ",") != "2026-03-31,2026-03-30,2026-03-29" {
			t.Errorf("a child created during a holiday starts on the next school day: %v", lateDates)
		}
	})

	t.Run("on a weekend, holiday or pause day the daily views are empty and say why", func(t *testing.T) {
		for _, tc := range []struct{ date, day, label string }{
			{"2026-03-21", "weekend <nil> <nil>", handlers.WeekendLabel},
			{"2026-03-23", "holiday عطلة الربيع ملاحظة", "عطلة الربيع"},
			{"2026-06-15", "pause العطلة الصيفية <nil>", "العطلة الصيفية"},
		} {
			report := get(t, app.AdminMiddleware(app.AdminDailyAttendanceHandler), "/api/admin/attendance?date="+tc.date, admin)
			if dayOf(report) != tc.day || len(report["data"].([]any)) != 0 || report["date"] != tc.date {
				t.Errorf("report %s: %v", tc.date, report)
			}
			rec := serve(t, app.AdminMiddleware(app.AdminExportExcelHandler), "GET", "/api/admin/export/excel?date="+tc.date, admin, "")
			f, err := excelize.OpenReader(bytes.NewReader(rec.Body.Bytes()))
			if err != nil || rec.Code != 200 {
				t.Fatalf("excel %s: %d %v", tc.date, rec.Code, err)
			}
			rows, _ := f.GetRows("Sheet1")
			label, _ := f.GetCellValue("Sheet1", "A4")
			header, _ := f.GetCellValue("Sheet1", "A5")
			if len(rows) != 5 || label != tc.label || header != "رقم الطالب" {
				t.Errorf("excel %s: %d rows, A4 %q, A5 %q", tc.date, len(rows), label, header)
			}
			f.Close()
			pinToday(t, tc.date+" 13:00")
			dash := get(t, app.AdminMiddleware(app.AdminDashboardHandler), "/api/admin/dashboard", admin)
			if dayOf(dash) != tc.day || fmt.Sprint(dash["data"]) != "map[absent_today:0 excused_today:0 present_today:0 total_parents:2 total_students:3]" {
				t.Errorf("dashboard %s: %v", tc.date, dash)
			}
			today := get(t, app.AuthMiddleware(app.MobileTodayAttendanceHandler), "/api/mobile/attendance/today", parent)
			if dayOf(today) != tc.day || len(today["data"].([]any)) != 0 || today["date"] != tc.date {
				t.Errorf("parent today %s: %v", tc.date, today)
			}
		}
	})

	t.Run("on a school day the daily views are as before, with day_type school", func(t *testing.T) {
		report := get(t, app.AdminMiddleware(app.AdminDailyAttendanceHandler), "/api/admin/attendance?date=2026-03-18", admin)
		if dayOf(report) != "school <nil> <nil>" || len(report["data"].([]any)) != 2 {
			t.Errorf("report: %v", report)
		}
		for _, k := range []string{"day_title", "day_notes"} {
			if v, ok := report[k]; !ok || v != nil {
				t.Errorf("%s must be present and null on a school day", k)
			}
		}
		rec := serve(t, app.AdminMiddleware(app.AdminExportExcelHandler), "GET", "/api/admin/export/excel?date=2026-03-18", admin, "")
		f, err := excelize.OpenReader(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		rows, _ := f.GetRows("Sheet1")
		label, _ := f.GetCellValue("Sheet1", "A4")
		f.Close()
		if len(rows) != 7 || label != "" {
			t.Errorf("excel on a school day: %d rows, A4 %q", len(rows), label)
		}
		pinToday(t, "2026-03-18 13:00")
		dash := get(t, app.AdminMiddleware(app.AdminDashboardHandler), "/api/admin/dashboard", admin)
		if dayOf(dash) != "school <nil> <nil>" || fmt.Sprint(dash["data"]) != "map[absent_today:2 excused_today:0 present_today:1 total_parents:2 total_students:3]" {
			t.Errorf("dashboard: %v", dash)
		}
		today := get(t, app.AuthMiddleware(app.MobileTodayAttendanceHandler), "/api/mobile/attendance/today", parent)
		if dayOf(today) != "school <nil> <nil>" || len(today["data"].([]any)) != 2 {
			t.Errorf("parent today: %v", today)
		}
	})

	t.Run("GET /api/mobile/holidays lists the closed school weekdays of a month", func(t *testing.T) {
		pinToday(t, "2026-03-31 13:00")
		list := func(t *testing.T, query string) (string, []string) {
			t.Helper()
			m := get(t, app.AuthMiddleware(app.MobileHolidaysHandler), "/api/mobile/holidays"+query, parent)
			var out []string
			for _, d := range m["data"].([]any) {
				it := d.(map[string]any)
				out = append(out, fmt.Sprint(it["date"], " ", it["kind"], " ", it["title"], " ", it["notes"]))
			}
			return fmt.Sprint(m["month"]), out
		}
		prefixed := func(dates []string, suffix string) []string {
			var out []string
			for _, d := range dates {
				out = append(out, d+" "+suffix)
			}
			return out
		}
		for _, tc := range []struct {
			query, month string
			want         []string
		}{
			{"", "2026-03", prefixed(k1Weekdays("2026-03-22", "2026-03-26"), "holiday عطلة الربيع ملاحظة")},
			{"?month=2026-03", "2026-03", prefixed(k1Weekdays("2026-03-22", "2026-03-26"), "holiday عطلة الربيع ملاحظة")},
			{"?month=2026-04", "2026-04", prefixed([]string{"2026-04-29", "2026-04-30"}, "holiday عطلة مايو <nil>")},
			{"?month=2026-05", "2026-05", prefixed([]string{"2026-05-03"}, "holiday عطلة مايو <nil>")},
			{"?month=2026-06", "2026-06", prefixed(k1Weekdays("2026-06-14", "2026-06-30"), "pause العطلة الصيفية <nil>")},
			{"?month=2026-07", "2026-07", prefixed(k1Weekdays("2026-07-01", "2026-07-31"), "pause العطلة الصيفية <nil>")},
			{"?month=2026-02", "2026-02", nil},
			{"?month=2026-01", "2026-01", []string{"2026-01-04 holiday عطلة متداخلة <nil>", "2026-01-05 holiday عطلة متداخلة <nil>", "2026-01-06 pause توقف متداخل <nil>", "2026-01-07 holiday عطلة متداخلة <nil>", "2026-01-08 holiday عطلة متداخلة <nil>"}},
		} {
			month, got := list(t, tc.query)
			if month != tc.month || strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("%q: month %s\n got %v\nwant %v", tc.query, month, got, tc.want)
			}
		}
		for _, tc := range []struct{ query, msg string }{
			{"?month=2026-13", "month must be formatted as YYYY-MM"},
			{"?month=march", "month must be formatted as YYYY-MM"},
			{"?month=1999-12", "month must have a year from 2000 to 2100"},
		} {
			rec := serve(t, app.AuthMiddleware(app.MobileHolidaysHandler), "GET", "/api/mobile/holidays"+tc.query, parent, "")
			if rec.Code != 400 || !strings.Contains(rec.Body.String(), tc.msg) {
				t.Errorf("%s: %d %s", tc.query, rec.Code, rec.Body.String())
			}
		}
	})
}

// TestK1NoonJob verifies the noon absence job does nothing on a holiday, a pause day or a
// Saturday, logging one INFO line with the reason, and runs as before on a school day.
func TestK1NoonJob(t *testing.T) {
	capture := &w10Capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	db, _ := setupThrowawayDB(t, "k1noon")
	k1Exec(t, db,
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1, 'K1 Noon Parent', '+9647000004101', 'x')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES (1, 'K1 Absent Kid', 'K1N-1', 1), (2, 'K1 Present Kid', 'K1N-2', 1)`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('K1N-DEV', 'Gate', true)`,
		`INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (2, 'K1N-DEV', '2026-03-18 07:15'), (2, 'K1N-DEV', '2026-03-23 07:15')`,
		`INSERT INTO school_closures (kind, start_date, end_date, title) VALUES ('holiday', '2026-03-23', '2026-03-23', 'عطلة'), ('pause', '2026-06-14', NULL, 'توقف')`)
	bg := &background.Group{}
	run := func(t *testing.T, at string) []w10Log {
		t.Helper()
		pinToday(t, at)
		marker := len(capture.snapshot())
		cron.ProcessDailyAbsences(db, nil, bg)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bg.Wait(ctx)
		return capture.snapshot()[marker:]
	}
	for _, tc := range []struct{ name, at, date, typ string }{
		{"holiday", "2026-03-23 12:00", "2026-03-23", "holiday"},
		{"pause", "2026-06-15 12:00", "2026-06-15", "pause"},
		{"Saturday", "2026-03-21 12:00", "2026-03-21", "weekend"},
		{"Friday", "2026-03-20 12:00", "2026-03-20", "weekend"},
	} {
		t.Run("skipped on a "+tc.name, func(t *testing.T) {
			logs := run(t, tc.at)
			if len(logs) != 1 || logs[0].Level != slog.LevelInfo || logs[0].Msg != "ProcessDailyAbsences: skipped, not a school day" || logs[0].Attrs["date"] != tc.date || logs[0].Attrs["day_type"] != tc.typ {
				t.Errorf("logs %+v, want one INFO skip line for %s %s", logs, tc.date, tc.typ)
			}
			if n := k1Count(t, db, `SELECT COUNT(*) FROM notifications`); n != 0 {
				t.Errorf("%d notifications on a %s", n, tc.name)
			}
		})
	}
	t.Run("runs on a school day", func(t *testing.T) {
		logs := run(t, "2026-03-18 12:00")
		var msgs []string
		for _, l := range logs {
			msgs = append(msgs, l.Msg)
		}
		if strings.Join(msgs, "|") != "ProcessDailyAbsences: starting|ProcessDailyAbsences: processed absence|ProcessDailyAbsences: finished" {
			t.Errorf("logs %v", msgs)
		}
		var body string
		db.QueryRow(`SELECT title || ' ' || body FROM notifications`).Scan(&body)
		if n := k1Count(t, db, `SELECT COUNT(*) FROM notifications`); n != 1 || body != "إشعار غياب الطالب K1 Absent Kid غائب اليوم" {
			t.Errorf("%d notifications, %q", n, body)
		}
	})
}

// TestK1PunchNotifications verifies a punch on a holiday, a pause day or a Saturday is stored
// and answered exactly like one on a school day, without a notification, while school-day punches
// notify as before (first check-in and check-out only).
func TestK1PunchNotifications(t *testing.T) {
	db, _ := setupThrowawayDB(t, "k1punch")
	k1Exec(t, db,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('K1P-DEV', 'Gate', true)`,
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (4101, 'K1 Punch Parent', '+9647000004201', 'x')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES (4101, 'K1 Punch Kid', 'K1P-1', 4101)`,
		`INSERT INTO school_closures (kind, start_date, end_date, title) VALUES ('holiday', '2026-03-23', '2026-03-23', 'عطلة'), ('pause', '2026-06-14', NULL, 'توقف')`)
	app := &handlers.AppEnv{DB: db, Background: &background.Group{}}
	push := func(t *testing.T, date string) (*httptest.ResponseRecorder, []string) {
		t.Helper()
		before := k1Count(t, db, `SELECT COUNT(*) FROM notifications`)
		body := fmt.Sprintf("K1P-1\t%[1]s 07:15:00\t1\t1\nK1P-1\t%[1]s 07:20:00\t1\t1\nK1P-1\t%[1]s 10:30:00\t1\t1\nK1P-1\t%[1]s 12:30:00\t1\t1\n", date)
		rec := httptest.NewRecorder()
		app.ADMSHandler(rec, httptest.NewRequest("POST", "/iclock/cdata?SN=K1P-DEV&table=ATTLOG", strings.NewReader(body)))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		app.Background.Wait(ctx)
		if n := k1Count(t, db, `SELECT COUNT(*) FROM attendance_logs WHERE check_time::date = $1::date`, date); n != 4 {
			t.Errorf("%s: %d punches stored, want 4", date, n)
		}
		rows, _ := db.Query(`SELECT body FROM notifications ORDER BY id OFFSET $1`, before)
		var notes []string
		for rows.Next() {
			var s string
			rows.Scan(&s)
			notes = append(notes, s)
		}
		rows.Close()
		sort.Strings(notes)
		return rec, notes
	}
	school, notes := push(t, "2026-03-18")
	if strings.Join(notes, "|") != "تم تسجيل خروج الطالب K1 Punch Kid الساعة 12:30|تم تسجيل دخول الطالب K1 Punch Kid الساعة 07:15" {
		t.Errorf("school day notifications %v", notes)
	}
	for _, date := range []string{"2026-03-23", "2026-06-15", "2026-03-21"} {
		rec, notes := push(t, date)
		if rec.Code != school.Code || rec.Body.String() != school.Body.String() || rec.Header().Get("Content-Type") != school.Header().Get("Content-Type") || rec.Body.String() != "OK" {
			t.Errorf("%s: device response %d %q %q differs from a school day's", date, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
		if len(notes) != 0 {
			t.Errorf("%s: notifications %v", date, notes)
		}
	}
}

// TestK1AdminList verifies GET /api/admin/holidays: without month the running and upcoming
// closures, with month those covering it, with active, notified and recipient_count.
func TestK1AdminList(t *testing.T) {
	db, _ := setupThrowawayDB(t, "k1list")
	pinToday(t, k1Today+" 13:00")
	var b int
	if err := db.QueryRow(`INSERT INTO broadcasts (title, body, audience_type, recipient_count) VALUES ('x', 'y', 'all', 6) RETURNING id`).Scan(&b); err != nil {
		t.Fatal(err)
	}
	k1Exec(t, db, fmt.Sprintf(`INSERT INTO school_closures (id, kind, start_date, end_date, title, notes, broadcast_id) VALUES
		(1, 'holiday', '2026-09-01', '2026-09-03', 'سابقة', 'ملاحظة', %d),
		(2, 'holiday', '2026-09-21', '2026-09-27', 'جارية', NULL, NULL),
		(3, 'holiday', '2026-10-11', '2026-10-15', 'قادمة', NULL, NULL),
		(4, 'pause', '2026-12-01', NULL, 'توقف', NULL, NULL)`, b))
	auth.InitAuth("k1-list-secret")
	admin, _ := auth.GenerateAdminToken("admin", 0)
	app := &handlers.AppEnv{DB: db}
	list := func(t *testing.T, query string) []string {
		t.Helper()
		rec := serve(t, app.AdminMiddleware(app.AdminHolidaysHandler), "GET", "/api/admin/holidays"+query, admin, "")
		var m struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || rec.Code != 200 || m.Data == nil {
			t.Fatalf("%s: %d %s", query, rec.Code, rec.Body.String())
		}
		var out []string
		for _, it := range m.Data {
			created, _ := it["created_at"].(string)
			if _, err := time.Parse(time.RFC3339, created); err != nil || !strings.HasSuffix(created, "+03:00") {
				t.Errorf("created_at %q", created)
			}
			out = append(out, fmt.Sprint(it["id"], " ", it["kind"], " ", it["start_date"], " ", it["end_date"], " ", it["title"], " ", it["notes"], " ", it["active"], " ", it["notified"], " ", it["recipient_count"]))
		}
		return out
	}
	past := "1 holiday 2026-09-01 2026-09-03 سابقة ملاحظة false true 6"
	running := "2 holiday 2026-09-21 2026-09-27 جارية <nil> true false <nil>"
	upcoming := "3 holiday 2026-10-11 2026-10-15 قادمة <nil> false false <nil>"
	pause := "4 pause 2026-12-01 <nil> توقف <nil> false false <nil>"
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"", []string{running, upcoming, pause}},
		{"?month=2026-09", []string{past, running}},
		{"?month=2026-11", nil},
		{"?month=2027-03", []string{pause}},
	} {
		if got := list(t, tc.query); strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("%q\n got %v\nwant %v", tc.query, got, tc.want)
		}
	}
	for _, tc := range []struct{ query, msg string }{
		{"?month=2026-9", "month must be formatted as YYYY-MM"},
		{"?month=x", "month must be formatted as YYYY-MM"},
		{"?month=2101-01", "month must have a year from 2000 to 2100"},
	} {
		rec := serve(t, app.AdminMiddleware(app.AdminHolidaysHandler), "GET", "/api/admin/holidays"+tc.query, admin, "")
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), tc.msg) {
			t.Errorf("%s: %d %s", tc.query, rec.Code, rec.Body.String())
		}
	}
	k1Exec(t, db, `DELETE FROM broadcasts`)
	if got := list(t, "?month=2026-09"); len(got) == 0 || got[0] != "1 holiday 2026-09-01 2026-09-03 سابقة ملاحظة false false <nil>" {
		t.Errorf("after its broadcast is deleted: %v", got)
	}
}

// TestK1Auth verifies every new route rejects missing, malformed and expired tokens (401), the
// other app's token (403) and tokens from before a password rotation or PIN change (401),
// answers 405 for other methods, and writes nothing.
func TestK1Auth(t *testing.T) {
	srv, db, admin := k1Server(t, "k1auth", k1Today)
	j1Seed(t, db)
	later, earlier := time.Now().Add(time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
	expiredAdmin := e1Sign(t, jwt.MapClaims{"username": "admin", "role": "admin", "sv": 0, "exp": earlier})
	expiredParent := e1Sign(t, jwt.MapClaims{"parent_id": 3001, "phone": "+9647000003001", "role": "parent", "sv": 0, "exp": earlier})
	parent := e1Sign(t, jwt.MapClaims{"parent_id": 3001, "phone": "+9647000003001", "role": "parent", "sv": 0, "exp": later})
	body := k1Closure("kind", "holiday", "start_date", "2026-10-11", "title", "x", "notify", false)
	adminRoutes := []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/admin/holidays", nil},
		{"POST", "/api/admin/holidays", body},
		{"POST", "/api/admin/holidays/cancel", map[string]any{"id": 1}},
	}
	before := k1Snapshot(t, db)
	for _, rt := range adminRoutes {
		for _, tc := range []struct {
			name   string
			header map[string]string
			status int
		}{
			{"no token", nil, 401},
			{"malformed token", a14Bearer("not-a-jwt"), 401},
			{"wrong scheme", map[string]string{"Authorization": "Basic x"}, 401},
			{"expired token", a14Bearer(expiredAdmin), 401},
			{"parent token", a14Bearer(parent), 403},
		} {
			if r := a14Do(t, srv, rt.method, rt.path, tc.header, rt.body); r.status != tc.status || r.json(t)["status"] != "error" {
				t.Errorf("%s %s with %s: %d %s", rt.method, rt.path, tc.name, r.status, r.body)
			}
		}
	}
	for _, tc := range []struct {
		name   string
		header map[string]string
		status int
	}{
		{"no token", nil, 401},
		{"malformed token", a14Bearer("not-a-jwt"), 401},
		{"expired token", a14Bearer(expiredParent), 401},
		{"admin token", a14Bearer(admin), 403},
		{"parent token", a14Bearer(parent), 200},
	} {
		if r := a14Do(t, srv, "GET", "/api/mobile/holidays", tc.header, nil); r.status != tc.status {
			t.Errorf("GET /api/mobile/holidays with %s: %d %s", tc.name, r.status, r.body)
		}
	}
	script, err := os.ReadFile(filepath.Join("..", "..", "db", "scripts", "rotate_admin_password.sql"))
	if err != nil {
		t.Fatal(err)
	}
	k1Exec(t, db, strings.ReplaceAll(string(script), "NEW_PASSWORD_HERE", "k1-rotated-password"), `UPDATE parents SET session_version = session_version + 1 WHERE id = 3001`)
	for _, rt := range adminRoutes {
		if r := a14Do(t, srv, rt.method, rt.path, a14Bearer(admin), rt.body); r.status != 401 {
			t.Errorf("%s %s with a token from before the rotation: %d %s", rt.method, rt.path, r.status, r.body)
		}
	}
	if r := a14Do(t, srv, "GET", "/api/mobile/holidays", a14Bearer(parent), nil); r.status != 401 {
		t.Errorf("parent token from before the PIN change: %d %s", r.status, r.body)
	}
	if after := k1Snapshot(t, db); after != before {
		t.Errorf("rejected tokens wrote rows")
	}
	for _, tc := range []struct{ method, path, allow string }{
		{"DELETE", "/api/admin/holidays", "GET, HEAD, POST"},
		{"GET", "/api/admin/holidays/cancel", "POST"},
		{"POST", "/api/mobile/holidays", "GET, HEAD"},
	} {
		if r := a14Do(t, srv, tc.method, tc.path, nil, nil); r.status != 405 || r.header.Get("Allow") != tc.allow {
			t.Errorf("%s %s: %d Allow=%q", tc.method, tc.path, r.status, r.header.Get("Allow"))
		}
	}
}

// TestK1FakeToday verifies FAKE_TODAY: a valid date pins the server's today with a WARN, an
// invalid one or any value on Railway is a configuration error, and the server refuses to start.
func TestK1FakeToday(t *testing.T) {
	t.Run("configuration", func(t *testing.T) {
		for _, tc := range []struct{ value, railway, want, err string }{
			{"", "", "", ""},
			{"2026-09-24", "", "2026-09-24", ""},
			{" 2026-09-24 ", "", "2026-09-24", ""},
			{"2026-02-30", "", "", "invalid FAKE_TODAY"},
			{"26-09-24", "", "", "invalid FAKE_TODAY"},
			{"1999-12-31", "", "", "invalid FAKE_TODAY"},
			{"2026-09-24", "production", "", "FAKE_TODAY is for tests only and must not be set on Railway"},
		} {
			t.Setenv("FAKE_TODAY", tc.value)
			t.Setenv("RAILWAY_ENVIRONMENT_NAME", tc.railway)
			cfg, err := config.LoadConfig()
			switch {
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Errorf("FAKE_TODAY=%q railway=%q: err %v, want %q", tc.value, tc.railway, err, tc.err)
			case tc.err == "" && (err != nil || cfg.FakeToday != tc.want):
				t.Errorf("FAKE_TODAY=%q: %v %v", tc.value, cfg, err)
			}
		}
	})

	t.Run("the server takes it as today", func(t *testing.T) {
		srv, _, admin := k1Server(t, "k1fake", "2026-03-18")
		if dash := a14Do(t, srv, "GET", "/api/admin/dashboard", a14Bearer(admin), nil).json(t); dash["date"] != "2026-03-18" || dash["day_type"] != "school" {
			t.Errorf("dashboard %v", dash)
		}
		if srv.out.index("FAKE_TODAY is set: today is pinned for tests") < 0 {
			t.Errorf("no startup WARN")
		}
	})

	t.Run("the server refuses it on Railway", func(t *testing.T) {
		_, dsn := setupThrowawayDB(t, "k1fakerail")
		srv := g3Start(t, dsn, g3FreePort(t), false, "FAKE_TODAY=2026-09-24", "RAILWAY_ENVIRONMENT_NAME=staging")
		if code, _ := srv.wait(t, 15*time.Second); code != 1 || !strings.Contains(srv.out.String(), "FAKE_TODAY is for tests only") {
			t.Errorf("exit %d\n%s", code, srv.out)
		}
	})
}

// TestK1BeforePhoneNormalisation runs the new code on a database at 000027 without 000028, the
// state Staging is in between the steps of the README deploy notes.
func TestK1BeforePhoneNormalisation(t *testing.T) {
	db, dsn := setupThrowawayDB(t, "k1v27")
	if _, err := db.Exec(a14Migration(t, "000028_normalize_phone_numbers.down.sql")); err != nil {
		t.Fatal(err)
	}
	j1Seed(t, db)
	hash, _ := bcrypt.GenerateFromPassword([]byte(a14AdminPassword), bcrypt.DefaultCost)
	k1Exec(t, db, fmt.Sprintf(`UPDATE admins SET password_hash = '%s' WHERE username = 'admin'`, hash))
	srv := g3Start(t, dsn, g3FreePort(t), true, "TRUSTED_PROXY_CIDRS=127.0.0.1/32", "ABSENCE_CRON_SCHEDULE=0 0 1 1 *", "FAKE_TODAY="+k1Today)
	admin := a14Bearer(a14AdminToken(t, srv))
	r := a14Do(t, srv, "POST", "/api/admin/holidays", admin, k1Closure("kind", "holiday", "start_date", "2026-10-11", "end_date", "2026-10-15", "title", "عطلة"))
	if res := k1Decode(t, r); r.status != 200 || res.Data.RecipientCount != 6 {
		t.Errorf("create on 000027: %d %s", r.status, r.body)
	}
	for _, path := range []string{"/api/admin/holidays", "/api/admin/attendance?date=2026-10-11", "/api/admin/dashboard"} {
		if r := a14Do(t, srv, "GET", path, admin, nil); r.status != 200 {
			t.Errorf("%s on 000027: %d %s", path, r.status, r.body)
		}
	}
	if r := a14Do(t, srv, "GET", "/api/mobile/holidays?month=2026-10", j1ParentToken(t, 3001, "+9647000003001"), nil); r.status != 200 || strings.Count(string(r.body), `"date"`) != 5 {
		t.Errorf("parent holidays on 000027: %d %s", r.status, r.body)
	}
}

// TestK1ClosuresMigration verifies migration 000027: it adds only school_closures (with its
// checks and the link to broadcasts) to a database at 000026, rolls back to the exact earlier
// schema keeping every broadcast, and applies again; golang-migrate reaches it from versions 20
// to 26; and phone normalisation (now 000028) still applies last, collision abort included.
func TestK1ClosuresMigration(t *testing.T) {
	migrate, err := exec.LookPath("migrate")
	if err != nil {
		t.Skip("golang-migrate CLI not installed")
	}
	fresh := func(t *testing.T, label string) (*sql.DB, func(args ...string) (string, error)) {
		t.Helper()
		cli, cliDSN := setupThrowawayDB(t, label)
		for _, tbl := range []string{"school_closures", "broadcasts", "announcements", "banner_images", "device_tokens", "settings", "notifications", "weekly_schedules", "student_leaves", "banners", "admins", "attendance_logs", "devices", "students", "parents"} {
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
	state := func(db *sql.DB) (closures, phone bool) {
		db.QueryRow(`SELECT to_regclass('school_closures') IS NOT NULL,
			EXISTS (SELECT 1 FROM information_schema.columns WHERE column_name = 'phone_number_original')`).Scan(&closures, &phone)
		return
	}

	t.Run("000027 adds school_closures alone, rolls back exactly and applies again", func(t *testing.T) {
		db, run := fresh(t, "k1mig")
		if out, err := run("goto", "26"); err != nil {
			t.Fatalf("goto 26: %v\n%s", err, out)
		}
		before := g3Schema(t, db)
		var b int
		db.QueryRow(`INSERT INTO broadcasts (title, body, audience_type, recipient_count) VALUES ('t', 'b', 'all', 0) RETURNING id`).Scan(&b)
		if out, err := run("goto", "27"); err != nil {
			t.Fatalf("goto 27: %v\n%s", err, out)
		}
		if closures, phone := state(db); !closures || phone {
			t.Fatalf("at 27: school_closures %v, phone columns %v", closures, phone)
		}
		k1Exec(t, db, fmt.Sprintf(`INSERT INTO school_closures (kind, start_date, end_date, title, broadcast_id) VALUES ('holiday', '2099-01-01', '2099-03-01', 'ستون يوماً', %d), ('pause', '2099-06-01', NULL, 'توقف', NULL)`, b))
		for _, bad := range []string{
			`INSERT INTO school_closures (kind, start_date, end_date, title) VALUES ('holiday', '2098-01-01', '2098-03-02', 'x')`,
			`INSERT INTO school_closures (kind, start_date, end_date, title) VALUES ('holiday', '2098-01-01', NULL, 'x')`,
			`INSERT INTO school_closures (kind, start_date, end_date, title) VALUES ('pause', '2098-01-02', '2098-01-01', 'x')`,
			`INSERT INTO school_closures (kind, start_date, title) VALUES ('strike', '2098-01-01', 'x')`,
			`INSERT INTO school_closures (kind, start_date, title) VALUES ('pause', '2098-01-01', '')`,
			`INSERT INTO school_closures (kind, start_date, title) VALUES ('pause', '2098-01-01', repeat('ع', 81))`,
			`INSERT INTO school_closures (kind, start_date, title, notes) VALUES ('pause', '2098-01-01', 'x', repeat('ع', 501))`,
			`INSERT INTO school_closures (kind, start_date, title, broadcast_id) VALUES ('pause', '2098-01-01', 'x', 999999)`,
		} {
			if _, err := db.Exec(bad); err == nil {
				t.Errorf("accepted: %s", bad)
			}
		}
		var idx string
		db.QueryRow(`SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_school_closures_dates'`).Scan(&idx)
		if idx != "CREATE INDEX idx_school_closures_dates ON public.school_closures USING btree (start_date, end_date)" {
			t.Errorf("index %q", idx)
		}
		k1Exec(t, db, fmt.Sprintf(`DELETE FROM broadcasts WHERE id = %d`, b))
		if n := k1Count(t, db, `SELECT COUNT(*) FROM school_closures WHERE broadcast_id IS NULL`); n != 2 {
			t.Errorf("deleting the broadcast left %d unlinked closures, want 2", n)
		}
		db.QueryRow(`INSERT INTO broadcasts (title, body, audience_type, recipient_count) VALUES ('kept', 'b', 'all', 0) RETURNING id`).Scan(&b)
		if out, err := run("goto", "26"); err != nil {
			t.Fatalf("down to 26: %v\n%s", err, out)
		}
		if closures, _ := state(db); closures {
			t.Errorf("school_closures left after the rollback")
		}
		if n := k1Count(t, db, `SELECT COUNT(*) FROM broadcasts WHERE title = 'kept'`); n != 1 {
			t.Errorf("the rollback lost a broadcast")
		}
		k1Exec(t, db, `DELETE FROM broadcasts`)
		db.Exec(`SELECT setval('broadcasts_id_seq', 1, false)`)
		if after := g3Schema(t, db); after != before {
			t.Errorf("the schema after the rollback differs from 000026's")
		}
		if out, err := run("goto", "27"); err != nil {
			t.Fatalf("up again: %v\n%s", err, out)
		}
		if v, _ := run("version"); v != "27" {
			t.Errorf("version %q, want 27", v)
		}
	})

	for _, from := range []string{"20", "21", "22", "23", "24", "25", "26"} {
		t.Run("golang-migrate from version "+from+" through 27 to 28", func(t *testing.T) {
			db, run := fresh(t, "k1v"+from)
			if out, err := run("goto", from); err != nil {
				t.Fatalf("goto %s: %v\n%s", from, err, out)
			}
			if out, err := run("goto", "27"); err != nil {
				t.Fatalf("goto 27: %v\n%s", err, out)
			}
			if closures, phone := state(db); !closures || phone {
				t.Errorf("at 27: school_closures %v, phone columns %v", closures, phone)
			}
			if out, err := run("up"); err != nil {
				t.Fatalf("up: %v\n%s", err, out)
			}
			if v, _ := run("version"); v != "28" {
				t.Errorf("version %q, want 28", v)
			}
			if closures, phone := state(db); !closures || !phone {
				t.Errorf("at 28: school_closures %v, phone columns %v", closures, phone)
			}
		})
	}

	t.Run("000028 still aborts on collisions after 000027", func(t *testing.T) {
		db, run := fresh(t, "k1coll")
		if out, err := run("goto", "27"); err != nil {
			t.Fatalf("goto 27: %v\n%s", err, out)
		}
		k1Exec(t, db, `INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (991, 'A', '07000000991', 'x'), (992, 'B', '+9647000000991', 'x')`)
		out, err := run("up")
		if err == nil || !strings.Contains(out, "[991, 992]") {
			t.Fatalf("000028 should abort listing [991, 992]: %v\n%s", err, out)
		}
		if closures, phone := state(db); !closures || phone {
			t.Errorf("after the aborted 000028: school_closures %v, phone columns %v; want true, false", closures, phone)
		}
	})
}
