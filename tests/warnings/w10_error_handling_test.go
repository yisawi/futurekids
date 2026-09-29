package warnings

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"future_kids/internal/auth"
	"future_kids/internal/handlers"
	"future_kids/internal/ratelimit"
)

// ── Fake database/sql driver that injects failures ──────────────────────────
//
// The DSN selects the failure mode:
//   query — every query and exec fails
//   scan  — rows have one column, so any multi-column Scan fails
//   iter  — the row stream fails on the first Next (connection lost mid-iteration)
//   empty — every query returns zero rows

var (
	errW10Query = errors.New("w10-injected: query failed")
	errW10Iter  = errors.New("w10-injected: connection lost mid-iteration")
)

func init() { sql.Register("w10fake", w10Driver{}) }

type w10Driver struct{}

func (w10Driver) Open(mode string) (driver.Conn, error) { return &w10Conn{mode: mode}, nil }

type w10Conn struct{ mode string }

func (c *w10Conn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("w10: prepare unsupported")
}
func (c *w10Conn) Close() error              { return nil }
func (c *w10Conn) Begin() (driver.Tx, error) { return nil, errors.New("w10: tx unsupported") }
func (c *w10Conn) CheckNamedValue(*driver.NamedValue) error {
	return nil
}

func (c *w10Conn) QueryContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	switch c.mode {
	case "query":
		return nil, errW10Query
	case "scan":
		return &w10Rows{cols: []string{"only"}, rows: [][]driver.Value{{"x"}}}, nil
	case "iter":
		return &w10Rows{cols: []string{"a", "b"}, failNext: true}, nil
	default:
		return &w10Rows{cols: []string{"a"}}, nil
	}
}

func (c *w10Conn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	if c.mode == "query" {
		return nil, errW10Query
	}
	return driver.RowsAffected(1), nil
}

type w10Rows struct {
	cols     []string
	rows     [][]driver.Value
	i        int
	failNext bool
}

func (r *w10Rows) Columns() []string { return r.cols }
func (r *w10Rows) Close() error      { return nil }
func (r *w10Rows) Next(dest []driver.Value) error {
	if r.failNext {
		return errW10Iter
	}
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

// ── Log capture ─────────────────────────────────────────────────────────────

type w10Log struct {
	Level slog.Level
	Msg   string
	Attrs map[string]string
}

type w10Capture struct {
	mu   sync.Mutex
	recs []w10Log
}

func (c *w10Capture) Enabled(context.Context, slog.Level) bool { return true }
func (c *w10Capture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *w10Capture) WithGroup(string) slog.Handler            { return c }
func (c *w10Capture) Handle(_ context.Context, r slog.Record) error {
	rec := w10Log{Level: r.Level, Msg: r.Message, Attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value.String()
		return true
	})
	c.mu.Lock()
	c.recs = append(c.recs, rec)
	c.mu.Unlock()
	return nil
}

func (c *w10Capture) snapshot() []w10Log {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]w10Log(nil), c.recs...)
}

// orderRecorder notes how many log records existed when a 5xx status was written.
type orderRecorder struct {
	*httptest.ResponseRecorder
	capture      *w10Capture
	logsAtHeader int
}

func (o *orderRecorder) WriteHeader(code int) {
	if code >= 500 && o.logsAtHeader < 0 {
		o.logsAtHeader = len(o.capture.snapshot())
	}
	o.ResponseRecorder.WriteHeader(code)
}

// ── Endpoint table ──────────────────────────────────────────────────────────

type w10Endpoint struct {
	name   string
	op     string // expected log message prefix
	method string
	target string
	body   string
	h      http.HandlerFunc
	list   bool // returns a collection; covered by scan/iter/empty modes
}

// failStatus is the expected status on a database failure: device endpoints answer 503 so
// the device resends (the fake driver's errors count as transient), everything else 500.
func (ep w10Endpoint) failStatus() int {
	if ep.op == "DeviceAuthMiddleware:" {
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

func w10Endpoints(app *handlers.AppEnv) []w10Endpoint {
	admin, parent := handlers.AdminMiddleware, handlers.AuthMiddleware
	return []w10Endpoint{
		{"admin students list", "AdminStudentsHandler:", "GET", "/api/admin/students", "", admin(app.AdminStudentsHandler), true},
		{"admin daily attendance", "AdminDailyAttendanceHandler:", "GET", "/api/admin/attendance?date=2026-09-24", "", admin(app.AdminDailyAttendanceHandler), true},
		{"admin excel export", "AdminExportExcelHandler:", "GET", "/api/admin/export/excel?date=2026-09-24", "", admin(app.AdminExportExcelHandler), true},
		{"admin settings list", "AdminSettingsHandler:", "GET", "/api/admin/settings", "", admin(app.AdminSettingsHandler), true},
		{"admin devices list", "AdminDevicesHandler:", "GET", "/api/admin/devices", "", admin(app.AdminDevicesHandler), true},
		{"mobile today", "MobileTodayAttendanceHandler:", "GET", "/api/mobile/attendance/today", "", parent(app.MobileTodayAttendanceHandler), true},
		{"mobile banners", "GetActiveBannersHandler:", "GET", "/api/mobile/banners", "", parent(app.GetActiveBannersHandler), true},
		{"mobile summary", "MobileAttendanceSummaryHandler:", "GET", "/api/mobile/attendance/summary", "", parent(app.MobileAttendanceSummaryHandler), true},
		{"mobile monthly", "MobileMonthlyAttendanceHandler:", "GET", "/api/mobile/attendance/monthly", "", parent(app.MobileMonthlyAttendanceHandler), true},
		{"mobile schedule", "MobileScheduleHandler:", "GET", "/api/mobile/schedule", "", parent(app.MobileScheduleHandler), true},
		{"mobile students", "MobileStudentsHandler:", "GET", "/api/mobile/students", "", parent(app.MobileStudentsHandler), true},
		{"mobile notifications", "MobileNotificationsHandler:", "GET", "/api/mobile/notifications", "", parent(app.MobileNotificationsHandler), true},
		{"mobile settings", "MobileSettingsHandler:", "GET", "/api/mobile/settings", "", app.MobileSettingsHandler, true},

		{"admin dashboard", "AdminDashboardHandler:", "GET", "/api/admin/dashboard", "", admin(app.AdminDashboardHandler), false},
		{"admin login", "AdminLoginHandler:", "POST", "/api/admin/login", `{"username":"admin","password":"x"}`, app.AdminLoginHandler, false},
		{"mobile login", "MobileLoginHandler:", "POST", "/api/mobile/login", `{"phone":"+9647700000501","pin":"1234"}`, app.MobileLoginHandler, false},
		{"admin student create", "AdminStudentsHandler:", "POST", "/api/admin/students", `{"name":"K","parent_name":"P","parent_phone":"+9647700000502","parent_pin":"1234","rfid_tag":"T"}`, admin(app.AdminStudentsHandler), false},
		{"admin student update", "AdminStudentsHandler:", "PUT", "/api/admin/students", `{"id":1,"name":"K","parent_name":"P","parent_phone":"+9647700000502"}`, admin(app.AdminStudentsHandler), false},
		{"admin student delete", "AdminStudentsHandler:", "DELETE", "/api/admin/students?id=1", "", admin(app.AdminStudentsHandler), false},
		{"admin leave create", "AdminCreateLeaveHandler:", "POST", "/api/admin/leaves", `{"student_id":1,"leave_date":"2026-09-24"}`, admin(app.AdminCreateLeaveHandler), false},
		{"admin setting save", "AdminSettingsHandler:", "PUT", "/api/admin/settings", `{"key":"k","value":"v"}`, admin(app.AdminSettingsHandler), false},
		{"admin device create", "AdminDevicesHandler:", "POST", "/api/admin/devices", `{"serial_number":"SN1","location_name":"Gate","is_active":true}`, admin(app.AdminDevicesHandler), false},
		{"admin device update", "AdminDevicesHandler:", "PUT", "/api/admin/devices", `{"serial_number":"SN1","location_name":"Gate","is_active":true}`, admin(app.AdminDevicesHandler), false},
		{"admin device disable", "AdminDevicesHandler:", "DELETE", "/api/admin/devices?sn=SN1", "", admin(app.AdminDevicesHandler), false},
		{"device auth middleware", "DeviceAuthMiddleware:", "POST", "/api/attendance/push/json?SN=SN1", `{}`, app.DeviceAuthMiddleware(app.HardwareAttendancePushHandler), false},
	}
}

// TestW10ErrorHandling verifies audit warning W10: every database failure on every
// endpoint returns a generic 500, is logged with slog.Error before the 500 is written,
// and never leaks the internal error; iteration errors are detected; empty results are
// 200; auth failures are logged at warn level.
func TestW10ErrorHandling(t *testing.T) {
	capture := &w10Capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	auth.InitAuth("w10-test-secret")
	parentToken, _ := auth.GenerateParentToken(1, "+9647700000500")
	adminToken, _ := auth.GenerateAdminToken("admin")

	appFor := func(mode string) *handlers.AppEnv {
		db, err := sql.Open("w10fake", mode)
		if err != nil {
			t.Fatalf("open fake db: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		return &handlers.AppEnv{DB: db, LoginLimiter: ratelimit.NewLoginLimiter(1000, time.Minute)}
	}

	call := func(ep w10Endpoint) (*orderRecorder, []w10Log) {
		req := httptest.NewRequest(ep.method, ep.target, strings.NewReader(ep.body))
		if strings.HasPrefix(ep.target, "/api/admin/") && ep.target != "/api/admin/login" {
			req.Header.Set("Authorization", "Bearer "+adminToken)
		} else if strings.HasPrefix(ep.target, "/api/mobile/") && ep.target != "/api/mobile/login" && ep.target != "/api/mobile/settings" {
			req.Header.Set("Authorization", "Bearer "+parentToken)
		}
		before := len(capture.snapshot())
		rec := &orderRecorder{ResponseRecorder: httptest.NewRecorder(), capture: capture, logsAtHeader: -1}
		ep.h(rec, req)
		after := capture.snapshot()
		return rec, after[before:]
	}

	check500 := func(t *testing.T, ep w10Endpoint, wantErr error) {
		t.Helper()
		before := len(capture.snapshot())
		rec, logs := call(ep)
		if rec.Code != ep.failStatus() {
			t.Errorf("%s: HTTP %d, want %d (content-type %q)", ep.name, rec.Code, ep.failStatus(), rec.Header().Get("Content-Type"))
			return
		}
		var found *w10Log
		for i := range logs {
			if logs[i].Level == slog.LevelError && strings.HasPrefix(logs[i].Msg, ep.op) {
				found = &logs[i]
			}
		}
		if found == nil {
			t.Errorf("%s: %d returned without an ERROR log starting with %q (logs: %v)", ep.name, rec.Code, ep.op, logs)
			return
		}
		if wantErr != nil && !strings.Contains(found.Attrs["error"], wantErr.Error()) {
			t.Errorf("%s: log %q error attr = %q, want it to contain %q", ep.name, found.Msg, found.Attrs["error"], wantErr)
		}
		if rec.logsAtHeader <= before {
			t.Errorf("%s: 500 status written before the error was logged", ep.name)
		}
		body := rec.Body.String()
		if strings.Contains(body, "w10-injected") || strings.Contains(body, "sql:") {
			t.Errorf("%s: internal error leaked to client: %s", ep.name, body)
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp["status"] != "error" {
			t.Errorf("%s: 500 body is not the standard error JSON: %s", ep.name, body)
		}
	}

	t.Run("query errors on every endpoint", func(t *testing.T) {
		for _, ep := range w10Endpoints(appFor("query")) {
			check500(t, ep, errW10Query)
		}
	})

	t.Run("scan errors on list endpoints and dashboard", func(t *testing.T) {
		for _, ep := range w10Endpoints(appFor("scan")) {
			if ep.list || ep.name == "admin dashboard" {
				check500(t, ep, nil)
			}
		}
	})

	t.Run("rows.Err detected on list endpoints", func(t *testing.T) {
		for _, ep := range w10Endpoints(appFor("iter")) {
			if ep.list {
				check500(t, ep, errW10Iter)
			}
		}
	})

	t.Run("no rows is 200 with empty data", func(t *testing.T) {
		for _, ep := range w10Endpoints(appFor("empty")) {
			if !ep.list {
				continue
			}
			rec, logs := call(ep)
			if rec.Code != http.StatusOK {
				t.Errorf("%s: HTTP %d, want 200", ep.name, rec.Code)
				continue
			}
			for _, l := range logs {
				if l.Level >= slog.LevelError {
					t.Errorf("%s: unexpected error log on empty result: %s", ep.name, l.Msg)
				}
			}
			if ep.name == "admin excel export" {
				continue
			}
			var resp struct {
				Status string          `json:"status"`
				Data   json.RawMessage `json:"data"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			if d := string(resp.Data); resp.Status != "success" || (d != "[]" && d != "{}") {
				t.Errorf("%s: got status=%q data=%s, want success with empty collection", ep.name, resp.Status, d)
			}
		}
	})

	t.Run("auth failures return 401/403 with a warn log", func(t *testing.T) {
		app := appFor("empty")
		cases := []struct {
			name, op, header string
			h                http.HandlerFunc
			want             int
		}{
			{"parent route without token", "AuthMiddleware:", "", handlers.AuthMiddleware(app.MobileStudentsHandler), 401},
			{"parent route with bad token", "AuthMiddleware:", "Bearer not.a.token", handlers.AuthMiddleware(app.MobileStudentsHandler), 401},
			{"parent route with admin token", "AuthMiddleware:", "Bearer " + adminToken, handlers.AuthMiddleware(app.MobileStudentsHandler), 403},
			{"admin route without token", "AdminMiddleware:", "", handlers.AdminMiddleware(app.AdminStudentsHandler), 401},
			{"admin route with bad token", "AdminMiddleware:", "Bearer not.a.token", handlers.AdminMiddleware(app.AdminStudentsHandler), 401},
			{"admin route with parent token", "AdminMiddleware:", "Bearer " + parentToken, handlers.AdminMiddleware(app.AdminStudentsHandler), 403},
			{"device route without SN", "DeviceAuthMiddleware:", "", app.DeviceAuthMiddleware(app.HardwareAttendancePushHandler), 401},
		}
		for _, c := range cases {
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			before := len(capture.snapshot())
			rec := httptest.NewRecorder()
			c.h(rec, req)
			logs := capture.snapshot()[before:]
			if rec.Code != c.want {
				t.Errorf("%s: HTTP %d, want %d", c.name, rec.Code, c.want)
			}
			if len(logs) != 1 || logs[0].Level != slog.LevelWarn || !strings.HasPrefix(logs[0].Msg, c.op) {
				t.Errorf("%s: want one WARN log starting with %q, got %v", c.name, c.op, logs)
			}
			if strings.Contains(fmt.Sprint(logs), "not.a.token") {
				t.Errorf("%s: token value written to the log", c.name)
			}
		}
	})

	t.Run("concurrent failures are all logged without races", func(t *testing.T) {
		before := len(capture.snapshot())
		var wg sync.WaitGroup
		var mu sync.Mutex
		bad := 0
		requests := 0
		for _, mode := range []string{"query", "scan", "iter"} {
			app := appFor(mode)
			for _, ep := range w10Endpoints(app) {
				if mode != "query" && !ep.list {
					continue
				}
				for i := 0; i < 5; i++ {
					requests++
					wg.Add(1)
					go func(ep w10Endpoint) {
						defer wg.Done()
						req := httptest.NewRequest(ep.method, ep.target, strings.NewReader(ep.body))
						if strings.HasPrefix(ep.target, "/api/admin/") && ep.target != "/api/admin/login" {
							req.Header.Set("Authorization", "Bearer "+adminToken)
						} else if strings.HasPrefix(ep.target, "/api/mobile/") && ep.target != "/api/mobile/login" && ep.target != "/api/mobile/settings" {
							req.Header.Set("Authorization", "Bearer "+parentToken)
						}
						rec := httptest.NewRecorder()
						ep.h(rec, req)
						if rec.Code != ep.failStatus() {
							mu.Lock()
							bad++
							mu.Unlock()
						}
					}(ep)
				}
			}
		}
		wg.Wait()
		errorLogs := 0
		for _, l := range capture.snapshot()[before:] {
			if l.Level == slog.LevelError {
				errorLogs++
			}
		}
		if bad != 0 || errorLogs != requests {
			t.Errorf("%d concurrent failing requests: %d non-500 responses, %d ERROR logs; want 0 and %d", requests, bad, errorLogs, requests)
		}
	})

	t.Run("source: every 500 is logged and every rows loop checks rows.Err", func(t *testing.T) {
		files, _ := filepath.Glob(filepath.Join("..", "..", "internal", "handlers", "*.go"))
		bare := regexp.MustCompile(`respondError\(w, http\.StatusInternalServerError`)
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			lines := strings.Split(string(src), "\n")
			for i, l := range lines {
				if bare.MatchString(l) && (i == 0 || !strings.HasPrefix(strings.TrimSpace(lines[i-1]), "slog.Error(")) {
					t.Errorf("%s:%d: 500 without slog.Error on the line before (use respondInternalError)", filepath.Base(f), i+1)
				}
				if strings.Contains(l, "rows.Scan(") && strings.HasSuffix(strings.TrimSpace(l), "err != nil {") {
					indent := l[:len(l)-len(strings.TrimLeft(l, "\t"))]
					for j := i + 1; j < len(lines) && lines[j] != indent+"}"; j++ {
						if strings.TrimSpace(lines[j]) == "continue" {
							t.Errorf("%s:%d: scan error silently skipped", filepath.Base(f), i+1)
						}
					}
				}
			}
			loops := strings.Count(string(src), "for rows.Next()")
			checks := strings.Count(string(src), "rows.Err()")
			if checks < loops {
				t.Errorf("%s: %d rows.Next loops but only %d rows.Err checks", filepath.Base(f), loops, checks)
			}
		}
	})

	if t.Failed() {
		t.Log("FAIL: Error handling is inconsistent (see subtest errors above)")
	} else {
		t.Log("PASS: Every error path is logged and returns a consistent response")
	}
}
