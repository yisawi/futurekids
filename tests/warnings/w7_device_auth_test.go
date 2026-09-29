package warnings

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"future_kids/internal/handlers"
)

// TestW7DeviceAuth verifies audit warning W7: the JSON punch endpoint stores a punch only for
// the device authenticated by ?SN= (registered and active), stores it under that serial, and
// rejects any body device_sn that differs as a spoofing attempt. Unknown or disabled devices,
// bad input and database failures never store anything and never loop the device on a 500.
func TestW7DeviceAuth(t *testing.T) {
	capture := &w10Capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	db, _ := setupThrowawayDB(t, "w7")
	seed := []string{
		`INSERT INTO students (id, full_name, rfid_tag) VALUES (1, 'W7 S1', 'W7-T1'), (2, 'W7 S2', 'W7-T2')`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('DEV-A', 'Gate A', true), ('DEV-B', 'Gate B', true), ('DEV-OFF', 'Old gate', false)`,
		`CREATE FUNCTION w7_fail() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
			IF NEW.check_time = '2026-02-02 07:00:00' THEN
				RAISE EXCEPTION USING ERRCODE = 'connection_failure', MESSAGE = 'w7 injected connection failure';
			END IF;
			RETURN NEW;
		 END $$`,
		`CREATE TRIGGER w7_fail BEFORE INSERT ON attendance_logs FOR EACH ROW EXECUTE FUNCTION w7_fail()`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed failed: %v\nquery: %s", err, q)
		}
	}
	app := &handlers.AppEnv{DB: db}
	route := handlers.HardwareLoggerMiddleware(app.DeviceAuthMiddleware(app.HardwareAttendancePushHandler))

	type result struct {
		code       int
		message    string
		retryAfter string
		logs       []w10Log
	}
	push := func(t *testing.T, h http.HandlerFunc, sn string, body map[string]any) result {
		t.Helper()
		target := "/api/attendance/push/json"
		if sn != "" {
			target += "?SN=" + sn
		}
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(string(b)))
		req.RemoteAddr = "203.0.113.7:4242"
		before := len(capture.snapshot())
		rec := httptest.NewRecorder()
		h(rec, req)
		var resp map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return result{rec.Code, resp["message"], rec.Header().Get("Retry-After"), capture.snapshot()[before:]}
	}
	punch := func(sn, tag, at string) map[string]any {
		return map[string]any{"device_sn": sn, "rfid_tag": tag, "push_time": at}
	}
	storedAt := func(t *testing.T, at string) (int, string) {
		t.Helper()
		var n int
		var sn sql.NullString
		if err := db.QueryRow(`SELECT COUNT(*), MAX(device_sn) FROM attendance_logs WHERE check_time = $1::timestamp`, at).Scan(&n, &sn); err != nil {
			t.Fatalf("query punches: %v", err)
		}
		return n, sn.String
	}
	warned := func(logs []w10Log, prefix string, attrs map[string]string) bool {
		for _, l := range logs {
			if l.Level != slog.LevelWarn || !strings.HasPrefix(l.Msg, prefix) {
				continue
			}
			ok := true
			for k, v := range attrs {
				if l.Attrs[k] != v {
					ok = false
				}
			}
			if ok {
				return true
			}
		}
		return false
	}

	t.Run("verified device stores the punch under its own serial", func(t *testing.T) {
		r := push(t, route, "DEV-A", punch("DEV-A", "W7-T1", "2026-02-01 07:10:00"))
		if r.code != http.StatusOK || r.message != "Punched successfully" {
			t.Fatalf("HTTP %d %q, want 200 Punched successfully", r.code, r.message)
		}
		if n, sn := storedAt(t, "2026-02-01 07:10:00"); n != 1 || sn != "DEV-A" {
			t.Errorf("stored %d rows with device_sn %q, want 1 under DEV-A", n, sn)
		}
	})

	rejections := []struct {
		name, sn  string
		body      map[string]any
		code      int
		message   string
		logPrefix string
		logAttrs  map[string]string
	}{
		{"missing SN", "", punch("DEV-A", "W7-T1", "2026-02-01 08:00:00"), 401, "Device SN is required",
			"DeviceAuthMiddleware: missing device SN", map[string]string{"remote_addr": "203.0.113.7:4242"}},
		{"unknown device", "DEV-GHOST", punch("DEV-GHOST", "W7-T1", "2026-02-01 08:01:00"), 401, "Unauthorized Device",
			"DeviceAuthMiddleware: unregistered device", map[string]string{"device_sn": "DEV-GHOST", "remote_addr": "203.0.113.7:4242"}},
		{"disabled device", "DEV-OFF", punch("DEV-OFF", "W7-T1", "2026-02-01 08:02:00"), 403, "Device is disabled",
			"DeviceAuthMiddleware: disabled device", map[string]string{"device_sn": "DEV-OFF", "remote_addr": "203.0.113.7:4242"}},
		{"spoof another active device", "DEV-A", punch("DEV-B", "W7-T1", "2026-02-01 08:03:00"), 403, "device_sn does not match the authenticated device",
			"HardwareAttendancePushHandler: device_sn does not match", map[string]string{"authenticated_sn": "DEV-A", "claimed_sn": "DEV-B", "remote_addr": "203.0.113.7:4242"}},
		{"spoof an unknown device", "DEV-A", punch("DEV-GHOST", "W7-T1", "2026-02-01 08:04:00"), 403, "device_sn does not match the authenticated device",
			"HardwareAttendancePushHandler: device_sn does not match", map[string]string{"authenticated_sn": "DEV-A", "claimed_sn": "DEV-GHOST"}},
		{"spoof a disabled device", "DEV-A", punch("DEV-OFF", "W7-T1", "2026-02-01 08:05:00"), 403, "device_sn does not match the authenticated device",
			"HardwareAttendancePushHandler: device_sn does not match", map[string]string{"claimed_sn": "DEV-OFF"}},
		{"missing body device_sn", "DEV-A", map[string]any{"rfid_tag": "W7-T1", "push_time": "2026-02-01 08:06:00"}, 400, "device_sn is required", "", nil},
		{"missing rfid_tag", "DEV-A", map[string]any{"device_sn": "DEV-A", "push_time": "2026-02-01 08:07:00"}, 400, "rfid_tag is required", "", nil},
		{"malformed push_time", "DEV-A", punch("DEV-A", "W7-T1", "01/02/2026 08:08"), 400, "push_time must be formatted as YYYY-MM-DD HH:MM:SS", "", nil},
	}
	for _, c := range rejections {
		t.Run("rejected/"+c.name, func(t *testing.T) {
			r := push(t, route, c.sn, c.body)
			if r.code != c.code || r.message != c.message {
				t.Errorf("HTTP %d %q, want %d %q", r.code, r.message, c.code, c.message)
			}
			if c.logPrefix != "" && !warned(r.logs, c.logPrefix, c.logAttrs) {
				t.Errorf("no WARN %q with %v (logs %v)", c.logPrefix, c.logAttrs, r.logs)
			}
			if at, _ := c.body["push_time"].(string); at != "" {
				if n, _ := storedAt(t, at); n != 0 {
					t.Errorf("rejected request stored %d punches", n)
				}
			}
		})
	}

	t.Run("handler without DeviceAuthMiddleware fails closed", func(t *testing.T) {
		r := push(t, app.HardwareAttendancePushHandler, "DEV-A", punch("DEV-A", "W7-T1", "2026-02-01 09:00:00"))
		if r.code != http.StatusInternalServerError {
			t.Errorf("HTTP %d, want 500", r.code)
		}
		if n, _ := storedAt(t, "2026-02-01 09:00:00"); n != 0 {
			t.Error("unauthenticated request stored a punch")
		}
	})

	t.Run("transient insert failure replies 503 and stores nothing", func(t *testing.T) {
		r := push(t, route, "DEV-A", punch("DEV-A", "W7-T1", "2026-02-02 07:00:00"))
		if r.code != http.StatusServiceUnavailable || r.retryAfter == "" {
			t.Errorf("HTTP %d Retry-After %q, want 503 with Retry-After", r.code, r.retryAfter)
		}
		if n, _ := storedAt(t, "2026-02-02 07:00:00"); n != 0 {
			t.Error("punch stored despite the failure")
		}
	})

	t.Run("database unavailable during device check replies 503", func(t *testing.T) {
		down, err := sql.Open("pgx", "postgres://localhost:1/none")
		if err != nil {
			t.Fatal(err)
		}
		down.Close()
		downApp := &handlers.AppEnv{DB: down}
		r := push(t, downApp.DeviceAuthMiddleware(downApp.HardwareAttendancePushHandler), "DEV-A", punch("DEV-A", "W7-T1", "2026-02-01 10:00:00"))
		if r.code != http.StatusServiceUnavailable || r.retryAfter == "" {
			t.Errorf("HTTP %d Retry-After %q, want 503 with Retry-After", r.code, r.retryAfter)
		}
	})

	t.Run("concurrent devices never cross-contaminate", func(t *testing.T) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		var bad []string
		for i := 0; i < 60; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sn, claimed := "DEV-A", "DEV-A"
				if i%2 == 1 {
					sn, claimed = "DEV-B", "DEV-B"
				}
				want := http.StatusOK
				if i%3 == 0 {
					claimed = map[string]string{"DEV-A": "DEV-B", "DEV-B": "DEV-A"}[sn]
					want = http.StatusForbidden
				}
				at := fmt.Sprintf("2026-02-03 07:%02d:%02d", i/60, i%60)
				r := push(t, route, sn, punch(claimed, fmt.Sprintf("W7-T%d", i%2+1), at))
				if r.code != want {
					mu.Lock()
					bad = append(bad, fmt.Sprintf("request %d (SN %s, claimed %s): HTTP %d, want %d", i, sn, claimed, r.code, want))
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()
		for _, b := range bad {
			t.Error(b)
		}
		rows, err := db.Query(`SELECT to_char(check_time, 'SS')::int, device_sn FROM attendance_logs WHERE check_time::date = '2026-02-03'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		stored := 0
		for rows.Next() {
			var i int
			var sn string
			rows.Scan(&i, &sn)
			stored++
			want := map[bool]string{false: "DEV-A", true: "DEV-B"}[i%2 == 1]
			if i%3 == 0 {
				t.Errorf("spoofed request %d was stored", i)
			}
			if sn != want {
				t.Errorf("punch %d stored under %s, want %s (the device that sent it)", i, sn, want)
			}
		}
		if stored != 40 {
			t.Errorf("stored %d punches, want 40 (60 requests minus 20 spoofed)", stored)
		}
	})

	t.Run("every stored punch belongs to a verified active device", func(t *testing.T) {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM attendance_logs a JOIN devices d ON d.serial_number = a.device_sn WHERE NOT d.is_active`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d punches stored under a disabled device", n)
		}
	})

	t.Run("JSON push route is wrapped in DeviceAuthMiddleware", func(t *testing.T) {
		src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "api", "main.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, `"/api/attendance/push/json"`) && !strings.Contains(line, "DeviceAuthMiddleware(appEnv.HardwareAttendancePushHandler)") {
				t.Errorf("JSON push route is not authenticated: %s", strings.TrimSpace(line))
			}
		}
	})

	if t.Failed() {
		t.Log("FAIL: JSON push device authentication is not enforced (see subtest errors above)")
	} else {
		t.Log("PASS: Only the authenticated, active device can store punches, under its own serial")
	}
}
