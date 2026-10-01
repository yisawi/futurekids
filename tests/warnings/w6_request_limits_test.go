package warnings

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"future_kids/internal/auth"
	"future_kids/internal/handlers"
	"future_kids/internal/ratelimit"
	"future_kids/internal/server"

	"golang.org/x/crypto/bcrypt"
)

type w6Endpoint struct {
	name   string
	method string
	target string
	body   func(variant int) string // valid JSON; variant keeps create requests unique
	badMsg string                   // 400 message for malformed JSON
	h      http.HandlerFunc
}

// padTo pads a JSON body with trailing spaces to exactly n bytes.
func padTo(body string, n int64) string {
	return body + strings.Repeat(" ", int(n)-len(body))
}

// oversized returns a syntactically valid JSON object of exactly n bytes.
func oversized(n int64) string {
	prefix, suffix := `{"pad":"`, `"}`
	return prefix + strings.Repeat("a", int(n)-len(prefix)-len(suffix)) + suffix
}

// TestW6RequestLimits verifies audit warning W6: JSON bodies are capped with 413 + WARN,
// ADMS bodies are capped but always ACKed with 200 OK (RULES.md §5), and the server
// enforces header/read/write/idle timeouts without harming legitimate requests.
func TestW6RequestLimits(t *testing.T) {
	capture := &w10Capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	db, _ := setupThrowawayDB(t, "w6")
	pinHash, _ := bcrypt.GenerateFromPassword([]byte("4321"), bcrypt.MinCost)
	seed := []string{
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1, 'W6 Parent', '+9647000000601', '` + string(pinHash) + `')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES (1, 'W6 Student', 'W6-RFID-1', 1)`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES (2, 'W6 Orphan', 'W6-RFID-2', NULL)`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('W6-DEVICE', 'Gate', true)`,
		`SELECT setval('students_id_seq', 100)`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed failed: %v\nquery: %s", err, q)
		}
	}

	auth.InitAuth("w6-test-secret")
	adminToken, _ := auth.GenerateAdminToken("admin", 0)
	app := &handlers.AppEnv{DB: db, LoginLimiter: ratelimit.NewLoginLimiter(1000, time.Minute), AdminUserLimiter: ratelimit.NewLoginLimiter(1000, time.Minute), AdminIPLimiter: ratelimit.NewLoginLimiter(1000, time.Minute)}
	admin := app.AdminMiddleware

	endpoints := []w6Endpoint{
		{"admin login", "POST", "/api/admin/login", func(int) string { return `{"username":"admin","password":"admin123"}` }, "Invalid request", app.AdminLoginHandler},
		{"mobile login", "POST", "/api/mobile/login", func(int) string { return `{"phone":"+9647000000601","pin":"4321"}` }, "Invalid request", app.MobileLoginHandler},
		{"student create", "POST", "/api/admin/students", func(v int) string {
			return fmt.Sprintf(`{"name":"K%d","parent_name":"W6 Parent","parent_phone":"+9647000000601","rfid_tag":"W6-NEW-%d"}`, v, v)
		}, "Invalid request body", admin(app.AdminStudentsHandler)},
		{"student update", "PUT", "/api/admin/students", func(int) string {
			return `{"id":1,"name":"W6 Student","parent_name":"W6 Parent","parent_phone":"+9647000000601"}`
		}, "Invalid request body or missing ID", admin(app.AdminStudentsHandler)},
		{"leave create", "POST", "/api/admin/leaves", func(int) string { return `{"student_id":1,"leave_date":"2026-09-24"}` }, "Invalid request payload", admin(app.AdminCreateLeaveHandler)},
		{"setting save", "PUT", "/api/admin/settings", func(int) string { return `{"key":"w6","value":"v"}` }, "Invalid payload", admin(app.AdminSettingsHandler)},
		{"device create", "POST", "/api/admin/devices", func(v int) string {
			return fmt.Sprintf(`{"serial_number":"W6-NEW-%d","location_name":"Gate","is_active":true}`, v)
		}, "Invalid payload or missing SN", admin(app.AdminDevicesHandler)},
		{"device update", "PUT", "/api/admin/devices", func(int) string { return `{"serial_number":"W6-DEVICE","location_name":"Gate","is_active":true}` }, "Invalid payload", admin(app.AdminDevicesHandler)},
		{"hardware JSON push", "POST", "/api/attendance/push/json?SN=W6-DEVICE", func(int) string {
			return `{"device_sn":"W6-DEVICE","rfid_tag":"W6-RFID-1","push_time":"2026-09-24 07:15:00"}`
		}, "Invalid payload", app.DeviceAuthMiddleware(app.HardwareAttendancePushHandler)},
	}

	send := func(ep w6Endpoint, body string) (*httptest.ResponseRecorder, []w10Log) {
		req := httptest.NewRequest(ep.method, ep.target, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		before := len(capture.snapshot())
		rec := httptest.NewRecorder()
		ep.h(rec, req)
		return rec, capture.snapshot()[before:]
	}

	t.Run("JSON/normal bodies pass", func(t *testing.T) {
		for _, ep := range endpoints {
			if rec, _ := send(ep, ep.body(1)); rec.Code != http.StatusOK {
				t.Errorf("%s: HTTP %d, want 200: %s", ep.name, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("JSON/body of exactly the limit passes", func(t *testing.T) {
		for _, ep := range endpoints {
			if rec, _ := send(ep, padTo(ep.body(2), handlers.MaxJSONBodyBytes)); rec.Code != http.StatusOK {
				t.Errorf("%s: %d-byte body got HTTP %d, want 200: %s", ep.name, handlers.MaxJSONBodyBytes, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("JSON/oversized bodies get 413 with a WARN log", func(t *testing.T) {
		for _, size := range []int64{handlers.MaxJSONBodyBytes + 1, 5 << 20} {
			for _, ep := range endpoints {
				rec, logs := send(ep, oversized(size))
				if rec.Code != http.StatusRequestEntityTooLarge {
					t.Errorf("%s: %d-byte body got HTTP %d, want 413", ep.name, size, rec.Code)
					continue
				}
				var resp map[string]string
				if json.Unmarshal(rec.Body.Bytes(), &resp) != nil || resp["status"] != "error" || resp["message"] != "Request body too large" {
					t.Errorf("%s: 413 body is not the standard error JSON: %s", ep.name, rec.Body.String())
				}
				warned := false
				for _, l := range logs {
					if l.Level == slog.LevelWarn && l.Msg == "Request body too large" && strings.HasPrefix(ep.target, l.Attrs["path"]) {
						warned = true
					}
				}
				if !warned {
					t.Errorf("%s: 413 without a WARN log (logs: %v)", ep.name, logs)
				}
			}
		}
	})

	t.Run("JSON/malformed bodies keep their 400 message", func(t *testing.T) {
		for _, ep := range endpoints {
			rec, _ := send(ep, `{"broken"`)
			var resp map[string]string
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			if rec.Code != http.StatusBadRequest || resp["message"] != ep.badMsg {
				t.Errorf("%s: got HTTP %d %q, want 400 %q", ep.name, rec.Code, resp["message"], ep.badMsg)
			}
		}
	})

	punches := func(t *testing.T) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM attendance_logs WHERE student_id = 2`).Scan(&n); err != nil {
			t.Fatalf("count punches: %v", err)
		}
		return n
	}
	adms := func(h http.HandlerFunc, body string) (*httptest.ResponseRecorder, []w10Log) {
		req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=W6-DEVICE&table=ATTLOG", strings.NewReader(body))
		before := len(capture.snapshot())
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec, capture.snapshot()[before:]
	}

	t.Run("ADMS/batch of exactly the limit is processed", func(t *testing.T) {
		before := punches(t)
		rec, _ := adms(app.ADMSHandler, padTo("W6-RFID-2\t2026-09-24 07:10:00\t1\t1\n", handlers.MaxADMSBodyBytes))
		if rec.Code != http.StatusOK || rec.Body.String() != "OK" {
			t.Fatalf("HTTP %d %q, want 200 OK", rec.Code, rec.Body.String())
		}
		if after := punches(t); after != before+1 {
			t.Errorf("punches %d -> %d, want the punch stored", before, after)
		}
	})

	for _, route := range []struct {
		name string
		h    http.HandlerFunc
	}{{"/iclock/cdata", app.ADMSHandler}, {"/api/attendance/push alias", handlers.HardwareLoggerMiddleware(app.ADMSHandler)}} {
		t.Run("ADMS/oversized batch is dropped but ACKed with 200 OK/"+route.name, func(t *testing.T) {
			before := punches(t)
			rec, logs := adms(route.h, padTo("W6-RFID-2\t2026-09-24 12:10:00\t1\t1\n", handlers.MaxADMSBodyBytes+1))
			if rec.Code != http.StatusOK || rec.Body.String() != "OK" {
				t.Errorf("HTTP %d %q, want 200 OK (RULES.md §5)", rec.Code, rec.Body.String())
			}
			if after := punches(t); after != before {
				t.Errorf("punches %d -> %d, oversized batch must not be processed", before, after)
			}
			warned := false
			for _, l := range logs {
				if l.Level == slog.LevelWarn && strings.HasPrefix(l.Msg, "ADMSHandler: body exceeds limit") && l.Attrs["device_sn"] == "W6-DEVICE" {
					warned = true
				}
			}
			if !warned {
				t.Errorf("oversized ADMS batch without a WARN log (logs: %v)", logs)
			}
		})
	}

	t.Run("server/default timeouts are configured", func(t *testing.T) {
		d := server.DefaultTimeouts
		if d.ReadHeader <= 0 || d.Read <= 0 || d.Write <= 0 || d.Idle <= 0 {
			t.Fatalf("every timeout must be set, got %+v", d)
		}
		if d.ReadHeader > d.Read || d.Read > 2*time.Minute || d.Write > 2*time.Minute || d.Idle < d.Read {
			t.Errorf("unreasonable timeouts %+v", d)
		}
		srv := server.New(":0", http.NotFoundHandler(), d)
		if srv.ReadHeaderTimeout != d.ReadHeader || srv.ReadTimeout != d.Read || srv.WriteTimeout != d.Write ||
			srv.IdleTimeout != d.Idle || srv.MaxHeaderBytes != server.MaxHeaderBytes {
			t.Errorf("server.New did not apply limits: %+v", srv)
		}
	})

	// Live server with short timeouts so the behavior is observable quickly.
	short := server.Timeouts{ReadHeader: 200 * time.Millisecond, Read: 500 * time.Millisecond, Write: 500 * time.Millisecond, Idle: 500 * time.Millisecond}
	bodyErr := make(chan error, 64)
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("/read-body", func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		bodyErr <- err
		io.WriteString(w, "read")
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
		io.WriteString(w, "late")
	})
	mux.HandleFunc("/api/admin/login", app.AdminLoginHandler)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := server.New("", mux, short)
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	addr := ln.Addr().String()
	base := "http://" + addr
	client := func() *http.Client { return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}} }

	// waitClosed reads until the server closes conn; it returns how long that took.
	waitClosed := func(t *testing.T, conn net.Conn) time.Duration {
		t.Helper()
		start := time.Now()
		conn.SetReadDeadline(start.Add(5 * time.Second))
		data, err := io.ReadAll(conn)
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("server kept the connection open for 5s (got %q)", data)
		}
		if strings.Contains(string(data), " 200 ") {
			t.Errorf("slow client received a success response: %q", data)
		}
		return time.Since(start)
	}

	t.Run("server/slow headers are cut off (ReadHeaderTimeout)", func(t *testing.T) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		io.WriteString(conn, "GET /ok HTTP/1.1\r\nHost: x\r\n")
		if d := waitClosed(t, conn); d > 2*time.Second {
			t.Errorf("connection closed after %v, want about %v", d, short.ReadHeader)
		}
	})

	t.Run("server/slow body is cut off (ReadTimeout)", func(t *testing.T) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		io.WriteString(conn, "POST /read-body HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\npartial")
		select {
		case err := <-bodyErr:
			var ne net.Error
			if !errors.As(err, &ne) || !ne.Timeout() {
				t.Errorf("handler body read error = %v, want a timeout", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("handler still reading a stalled body after 3s")
		}
	})

	t.Run("server/slow handler cannot hold the response (WriteTimeout)", func(t *testing.T) {
		start := time.Now()
		resp, err := client().Get(base + "/slow")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Errorf("response delivered after WriteTimeout: HTTP %d %q", resp.StatusCode, body)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("client waited %v", d)
		}
	})

	t.Run("server/idle keep-alive connection is closed (IdleTimeout)", func(t *testing.T) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		io.WriteString(conn, "GET /ok HTTP/1.1\r\nHost: x\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("first request failed: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if d := waitClosed(t, conn); d < short.Idle/2 || d > 3*time.Second {
			t.Errorf("idle connection closed after %v, want about %v", d, short.Idle)
		}
	})

	t.Run("server/oversized header gets 431", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, base+"/ok", nil)
		req.Header.Set("X-Big", strings.Repeat("a", 2*server.MaxHeaderBytes))
		resp, err := client().Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
			t.Errorf("HTTP %d, want 431", resp.StatusCode)
		}
	})

	t.Run("server/oversized JSON gets 413 over a real connection", func(t *testing.T) {
		resp, err := client().Post(base+"/api/admin/login", "application/json", strings.NewReader(oversized(1<<20)))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("HTTP %d, want 413", resp.StatusCode)
		}
	})

	t.Run("server/legitimate requests succeed while slow clients hang", func(t *testing.T) {
		var slow []net.Conn
		for i := 0; i < 20; i++ {
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			io.WriteString(c, "GET /ok HTTP/1.1\r\nHost: x\r\n")
			slow = append(slow, c)
		}
		defer func() {
			for _, c := range slow {
				c.Close()
			}
		}()

		var wg sync.WaitGroup
		var mu sync.Mutex
		var failures []string
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				start := time.Now()
				var resp *http.Response
				var err error
				if i%2 == 0 {
					resp, err = client().Get(base + "/ok")
				} else {
					resp, err = client().Post(base+"/read-body", "text/plain", strings.NewReader(strings.Repeat("x", 1<<20)))
				}
				d := time.Since(start)
				if err == nil {
					resp.Body.Close()
				}
				if err != nil || resp.StatusCode != http.StatusOK || d > time.Second {
					mu.Lock()
					failures = append(failures, fmt.Sprintf("request %d: err=%v dur=%v", i, err, d))
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()
		for len(bodyErr) > 0 {
			if err := <-bodyErr; err != nil {
				failures = append(failures, fmt.Sprintf("legitimate 1 MB upload failed: %v", err))
			}
		}
		if len(failures) > 0 {
			t.Errorf("legitimate requests harmed by slow clients:\n%s", strings.Join(failures, "\n"))
		}
		for _, c := range slow {
			waitClosed(t, c)
		}
	})

	if t.Failed() {
		t.Log("FAIL: Request limits or server timeouts not enforced (see subtest errors above)")
	} else {
		t.Log("PASS: Request limits and server timeouts enforced")
	}
}
