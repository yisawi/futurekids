package warnings

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"future_kids/internal/auth"
	"future_kids/internal/handlers"
	"future_kids/internal/tz"

	"github.com/jackc/pgx/v5/stdlib"
)

// ── Counting driver: wraps pgx and records every round trip ─────────────────

type g1QueryLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *g1QueryLog) add(q string) {
	l.mu.Lock()
	l.entries = append(l.entries, strings.Join(strings.Fields(q), " "))
	l.mu.Unlock()
}

func (l *g1QueryLog) reset() { l.mu.Lock(); l.entries = nil; l.mu.Unlock() }

// sync returns the recorded statements, excluding the fire-and-forget writes that run in
// background goroutines (device last_sync and notification history).
func (l *g1QueryLog) sync() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, q := range l.entries {
		if strings.Contains(q, "SET last_sync") || strings.HasPrefix(q, "INSERT INTO notifications") {
			continue
		}
		out = append(out, q)
	}
	return out
}

var g1Log = &g1QueryLog{}

func init() { sql.Register("g1count", g1Driver{inner: stdlib.GetDefaultDriver()}) }

type g1Driver struct{ inner driver.Driver }

func (d g1Driver) Open(dsn string) (driver.Conn, error) {
	c, err := d.inner.Open(dsn)
	if err != nil {
		return nil, err
	}
	return &g1Conn{c}, nil
}

type g1Conn struct{ driver.Conn }

func (c *g1Conn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	g1Log.add(q)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}
func (c *g1Conn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	g1Log.add(q)
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}
func (c *g1Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	g1Log.add("BEGIN")
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return g1Tx{tx}, nil
}
func (c *g1Conn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, q)
}
func (c *g1Conn) CheckNamedValue(nv *driver.NamedValue) error {
	return c.Conn.(driver.NamedValueChecker).CheckNamedValue(nv)
}
func (c *g1Conn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}
func (c *g1Conn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

type g1Tx struct{ driver.Tx }

func (t g1Tx) Commit() error   { g1Log.add("COMMIT"); return t.Tx.Commit() }
func (t g1Tx) Rollback() error { g1Log.add("ROLLBACK"); return t.Tx.Rollback() }

// TestGroup1Optimizations verifies notices N1, N6, N7 and N12.
func TestGroup1Optimizations(t *testing.T) {
	_, dsn := setupThrowawayDB(t, "g1")
	db, err := sql.Open("g1count", dsn)
	if err != nil {
		t.Fatalf("open counting pool: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	seed := []string{
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('N6-DEV', 'Gate', true)`,
		`INSERT INTO students (full_name, rfid_tag) SELECT 'N6 Student ' || i, 'N6-TAG-' || i FROM generate_series(1, 60) AS i`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed failed: %v\nquery: %s", err, q)
		}
	}
	app := &handlers.AppEnv{DB: db}

	push := func(t *testing.T, body string) []string {
		t.Helper()
		g1Log.reset()
		req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=N6-DEV&table=ATTLOG", strings.NewReader(body))
		rec := httptest.NewRecorder()
		app.ADMSHandler(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != "OK" {
			t.Fatalf("ADMS push: HTTP %d %q", rec.Code, rec.Body.String())
		}
		return g1Log.sync()
	}
	batch := func(day string, from, to int) string {
		var b strings.Builder
		for i := from; i <= to; i++ {
			fmt.Fprintf(&b, "N6-TAG-%d\t%s 07:15:00\t1\t1\n", i, day)
		}
		return b.String()
	}
	countMatching := func(stmts []string, substr string) int {
		n := 0
		for _, s := range stmts {
			if strings.Contains(s, substr) {
				n++
			}
		}
		return n
	}

	t.Run("N6/round trips per ADMS batch are constant", func(t *testing.T) {
		one := push(t, batch("2026-03-01", 1, 1))
		fifty := push(t, batch("2026-03-02", 2, 51))
		t.Logf("database round trips: 1-punch batch = %d, 50-punch batch = %d", len(one), len(fifty))
		t.Logf("1-punch batch statements: %q", one)
		if len(fifty) != len(one) {
			t.Errorf("a 50-punch batch took %d round trips vs %d for 1 punch; the cost must not grow per punch", len(fifty), len(one))
		}
		if len(one) > 7 {
			t.Errorf("a 1-punch batch took %d round trips, want at most 7", len(one))
		}
		for name, stmts := range map[string][]string{"1-punch": one, "50-punch": fifty} {
			if n := countMatching(stmts, "get_student_status"); n != 1 {
				t.Errorf("%s batch ran the notification query %d times, want once per batch", name, n)
			}
		}
	})

	notes := func(t *testing.T, phones ...string) []string {
		t.Helper()
		last, stableSince, deadline := -1, time.Now(), time.Now().Add(3*time.Second)
		var bodies []string
		for time.Since(stableSince) < 300*time.Millisecond && time.Now().Before(deadline) {
			bodies = nil
			rows, err := db.Query(`SELECT body FROM notifications WHERE parent_phone = ANY($1) ORDER BY body`, phones)
			if err != nil {
				t.Fatalf("list notifications: %v", err)
			}
			for rows.Next() {
				var b string
				rows.Scan(&b)
				bodies = append(bodies, b)
			}
			rows.Close()
			if len(bodies) != last {
				last, stableSince = len(bodies), time.Now()
			}
			time.Sleep(25 * time.Millisecond)
		}
		return bodies
	}

	t.Run("N6/notifications unchanged: first check-in and check-out only", func(t *testing.T) {
		for _, q := range []string{
			`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (601, 'PA', '+9647700000601', 'h'), (602, 'PB', '+9647700000602', 'h'), (603, 'PC', '+9647700000603', 'h'), (604, 'PD', '+9647700000604', 'h')`,
			`INSERT INTO students (full_name, rfid_tag, parent_id) VALUES ('Kid A', 'PAR-A', 601), ('Kid B', 'PAR-B', 602), ('Kid C', 'PAR-C', 603), ('Kid D', 'PAR-D', 604)`,
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		push(t, "PAR-D\t2026-03-05 07:05:00\t1\t1\n")
		push(t, strings.Join([]string{
			"PAR-A\t2026-03-05 07:15:00", "PAR-A\t2026-03-05 07:18:00", "PAR-A\t2026-03-05 10:30:00",
			"PAR-A\t2026-03-05 12:05:00", "PAR-A\t2026-03-05 12:15:00",
			"PAR-B\t2026-03-05 07:03:00", "PAR-B\t2026-03-05 07:03:40",
			"PAR-C\t2026-03-05 07:20:00", "PAR-C\t2026-03-05 07:10:00",
			"PAR-D\t2026-03-05 07:25:00",
		}, "\t1\t1\n")+"\t1\t1\n")
		got := notes(t, "+9647700000601", "+9647700000602", "+9647700000603", "+9647700000604")
		sort.Strings(got)
		want := []string{
			"تم تسجيل خروج الطالب Kid A الساعة 12:05",
			"تم تسجيل دخول الطالب Kid A الساعة 07:15",
			"تم تسجيل دخول الطالب Kid B الساعة 07:03",
			"تم تسجيل دخول الطالب Kid C الساعة 07:10",
			"تم تسجيل دخول الطالب Kid D الساعة 07:05",
		}
		sort.Strings(want)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("notifications\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("N6/device PIN stays a string (alphanumeric tags)", func(t *testing.T) {
		if got := push(t, "N6-TAG-55\t2026-03-06 07:15:00\t1\t1\n"); len(got) == 0 {
			t.Fatal("no statements recorded")
		}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM attendance_logs a JOIN students s ON s.id = a.student_id WHERE s.rfid_tag = 'N6-TAG-55' AND a.check_time = '2026-03-06 07:15:00'`).Scan(&n); err != nil || n != 1 {
			t.Errorf("punch for alphanumeric PIN N6-TAG-55 stored %d times (err %v), want 1", n, err)
		}
	})

	t.Run("N1/health returns standard JSON", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handlers.HealthHandler(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		var body map[string]string
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" ||
			json.Unmarshal(rec.Body.Bytes(), &body) != nil || body["status"] != "success" || body["message"] == "" {
			t.Errorf("GET /health = %d %q %s, want 200 application/json with status success", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
		src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "api", "main.go"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), `mux.HandleFunc("GET /health", handlers.HealthHandler)`) || strings.Contains(string(src), "w.Write(") {
			t.Error("main.go must route GET /health to handlers.HealthHandler and write no raw responses")
		}
	})

	t.Run("N7/Baghdad timezone is loaded once and used everywhere", func(t *testing.T) {
		if tz.Baghdad.String() != "Asia/Baghdad" {
			t.Errorf("tz.Baghdad = %q", tz.Baghdad)
		}
		for _, d := range []time.Time{time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC), time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)} {
			if _, off := d.In(tz.Baghdad).Zone(); off != 3*60*60 {
				t.Errorf("offset on %s = %ds, want +3h (Iraq has no DST)", d.Format("2006-01-02"), off)
			}
		}
		if tz.Now().Location() != tz.Baghdad || tz.Today() != time.Now().In(tz.Baghdad).Format("2006-01-02") {
			t.Error("tz.Now/tz.Today are not in Baghdad time")
		}

		sites := 0
		for _, glob := range []string{"internal/*/*.go", "cmd/*/*.go"} {
			files, _ := filepath.Glob(filepath.Join("..", "..", glob))
			for _, f := range files {
				if strings.HasSuffix(f, "_test.go") {
					continue
				}
				src, _ := os.ReadFile(f)
				if n := strings.Count(string(src), "time.LoadLocation("); n > 0 {
					sites += n
					if !strings.HasSuffix(filepath.ToSlash(f), "internal/tz/tz.go") {
						t.Errorf("%s calls time.LoadLocation; use tz.Baghdad", f)
					}
				}
			}
		}
		if sites != 1 {
			t.Errorf("time.LoadLocation appears %d times in non-test code, want exactly 1 (internal/tz)", sites)
		}

		auth.InitAuth("g1-test-secret")
		adminToken, _ := auth.GenerateAdminToken("admin")
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/admin/dashboard", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		handlers.AdminMiddleware(app.AdminDashboardHandler)(rec, req)
		var dash map[string]any
		json.Unmarshal(rec.Body.Bytes(), &dash)
		if rec.Code != http.StatusOK || dash["date"] != tz.Today() {
			t.Errorf("dashboard date = %v (HTTP %d), want Baghdad today %s", dash["date"], rec.Code, tz.Today())
		}
	})

	t.Run("N12/notifications are paginated 100 at a time with no gaps", func(t *testing.T) {
		for _, q := range []string{
			`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1201, 'Busy', '+9647700001201', 'h'), (1202, 'Other', '+9647700001202', 'h'), (1203, 'Quiet', '+9647700001203', 'h')`,
			`INSERT INTO notifications (parent_phone, title, body, created_at) SELECT '+9647700001201', 't', 'busy ' || i, TIMESTAMP '2026-01-01' + i * INTERVAL '1 minute' FROM generate_series(1, 150) AS i`,
			`INSERT INTO notifications (parent_phone, title, body) SELECT '+9647700001202', 't', 'other ' || i FROM generate_series(1, 5) AS i`,
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		auth.InitAuth("g1-test-secret")
		get := func(t *testing.T, parentID int, query string) (int, map[string]any) {
			t.Helper()
			token, _ := auth.GenerateParentToken(parentID, "x")
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/mobile/notifications"+query, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			handlers.AuthMiddleware(app.MobileNotificationsHandler)(rec, req)
			var body map[string]any
			json.Unmarshal(rec.Body.Bytes(), &body)
			return rec.Code, body
		}
		ids := func(body map[string]any) []int {
			var out []int
			for _, n := range body["data"].([]any) {
				out = append(out, int(n.(map[string]any)["id"].(float64)))
			}
			return out
		}

		g1Log.reset()
		code, page1 := get(t, 1201, "")
		if code != http.StatusOK {
			t.Fatalf("page 1: HTTP %d", code)
		}
		if countMatching(g1Log.sync(), "LIMIT") != 1 {
			t.Error("notifications query has no LIMIT")
		}
		p1 := ids(page1)
		if len(p1) != handlers.NotificationsPageSize || page1["has_more"] != true || page1["next_before"] != float64(p1[len(p1)-1]) {
			t.Fatalf("page 1: %d items, has_more=%v, next_before=%v; want 100, true, last id", len(p1), page1["has_more"], page1["next_before"])
		}
		for i := 1; i < len(p1); i++ {
			if p1[i] >= p1[i-1] {
				t.Fatalf("page 1 not newest first at %d: %v", i, p1[i-1:i+1])
			}
		}
		code, page2 := get(t, 1201, fmt.Sprintf("?before=%d", p1[len(p1)-1]))
		p2 := ids(page2)
		if code != http.StatusOK || len(p2) != 50 || page2["has_more"] != false || page2["next_before"] != nil {
			t.Fatalf("page 2: HTTP %d, %d items, has_more=%v, next_before=%v; want 50, false, null", code, len(p2), page2["has_more"], page2["next_before"])
		}
		seen := map[int]bool{}
		for _, id := range append(p1, p2...) {
			if seen[id] {
				t.Errorf("notification %d returned twice", id)
			}
			seen[id] = true
		}
		if n := countRows(t, db, `SELECT COUNT(*) FROM notifications WHERE parent_phone = '+9647700001201'`); len(seen) != n {
			t.Errorf("pages returned %d distinct notifications, parent has %d", len(seen), n)
		}
		if _, other := get(t, 1202, ""); len(ids(other)) != 5 {
			t.Errorf("other parent sees %d notifications, want only its own 5", len(ids(other)))
		}
		if code, quiet := get(t, 1203, ""); code != http.StatusOK || len(ids(quiet)) != 0 || quiet["has_more"] != false || quiet["next_before"] != nil {
			t.Errorf("parent without notifications: HTTP %d %v", code, quiet)
		}
		for _, bad := range []string{"abc", "0", "-5", "1.5"} {
			if code, _ := get(t, 1201, "?before="+bad); code != http.StatusBadRequest {
				t.Errorf("before=%s: HTTP %d, want 400", bad, code)
			}
		}
	})

	if t.Failed() {
		t.Log("FAIL: Group 1 optimizations not in effect (see subtest errors above)")
	} else {
		t.Log("PASS: health is JSON, ADMS round trips are constant per batch, timezone loaded once, notifications paginated")
	}
}

// BenchmarkLoadLocationPerCall measures the old per-request pattern.
func BenchmarkLoadLocationPerCall(b *testing.B) {
	for i := 0; i < b.N; i++ {
		loc, _ := time.LoadLocation("Asia/Baghdad")
		_ = time.Now().In(loc).Format("2006-01-02")
	}
}

// BenchmarkCachedLocation measures the current pattern (tz.Today).
func BenchmarkCachedLocation(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = tz.Today()
	}
}
