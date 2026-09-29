package warnings

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"future_kids/internal/handlers"
)

// w5FailTrigger lets a test make the insert of one specific punch fail:
//
//	reject    — a data error (SQLSTATE P0001): the record is skipped, the rest are kept
//	transient — a connection failure (SQLSTATE 08006): the whole batch is rolled back
//	kill      — the database session is terminated mid-batch
const w5FailTrigger = `
CREATE TABLE w5_fail_rules (check_time timestamp PRIMARY KEY, mode text NOT NULL);
CREATE FUNCTION w5_fail() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE m text;
BEGIN
	SELECT mode INTO m FROM w5_fail_rules WHERE check_time = NEW.check_time;
	IF m = 'reject' THEN
		RAISE EXCEPTION 'w5 injected rejection';
	ELSIF m = 'transient' THEN
		RAISE EXCEPTION USING ERRCODE = 'connection_failure', MESSAGE = 'w5 injected connection failure';
	ELSIF m = 'kill' THEN
		PERFORM pg_terminate_backend(pg_backend_pid());
		PERFORM pg_sleep(5);
	END IF;
	RETURN NEW;
END $$;
CREATE TRIGGER w5_fail BEFORE INSERT ON attendance_logs FOR EACH ROW EXECUTE FUNCTION w5_fail();`

type w5Route struct {
	name string
	path string
	h    func(app *handlers.AppEnv) http.HandlerFunc
}

// w5Routes mirror cmd/api/main.go: both ADMS paths are served by ADMSHandler with no JSON middleware.
var w5Routes = []w5Route{
	{"iclock", "/iclock/cdata", func(app *handlers.AppEnv) http.HandlerFunc { return app.ADMSHandler }},
	{"alias", "/api/attendance/push", func(app *handlers.AppEnv) http.HandlerFunc {
		return handlers.HardwareLoggerMiddleware(app.ADMSHandler)
	}},
}

// TestW5ADMSErrorHandling verifies audit warning W5: every ADMS error path is logged with
// context, a batch is stored atomically (a transient DB failure rolls it back and replies 503
// so the device resends; a record the DB rejects is skipped and logged), bad input and
// unknown devices are always ACKed 200 OK, notifications happen only after commit and
// never block storage, and both ADMS routes behave identically.
func TestW5ADMSErrorHandling(t *testing.T) {
	capture := &w10Capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	db, _ := setupThrowawayDB(t, "w5")
	seed := []string{
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1, 'W5 Parent', '+9647700000501', 'hash')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES
			(1, 'W5 S1', 'W5-P1', NULL), (2, 'W5 S2', 'W5-P2', NULL), (3, 'W5 S3', 'W5-P3', NULL),
			(4, 'W5 S4', 'W5-P4', NULL), (5, 'W5 S5', 'W5-P5', 1)`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('W5-DEVICE', 'Gate', true), ('W5-OFF', 'Gate', false)`,
		w5FailTrigger,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed failed: %v\nquery: %s", err, q)
		}
	}
	app := &handlers.AppEnv{DB: db}

	day := 0
	nextDay := func() string {
		day++
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, day).Format("2006-01-02")
	}
	// batch5 builds five punches (students 1..5) at 07:01..07:05 on a fresh day.
	batch5 := func() (string, []string) {
		d := nextDay()
		var lines, times []string
		for i := 1; i <= 5; i++ {
			ts := fmt.Sprintf("%s 07:0%d:00", d, i)
			times = append(times, ts)
			lines = append(lines, fmt.Sprintf("W5-P%d\t%s\t1\t1", i, ts))
		}
		return strings.Join(lines, "\n") + "\n", times
	}
	stored := func(t *testing.T, times []string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM attendance_logs WHERE check_time::text = ANY($1)`, times).Scan(&n); err != nil {
			t.Fatalf("count punches: %v", err)
		}
		return n
	}
	failAt := func(t *testing.T, ts, mode string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO w5_fail_rules VALUES ($1::timestamp, $2)`, ts, mode); err != nil {
			t.Fatalf("add fail rule: %v", err)
		}
		t.Cleanup(func() { db.Exec(`DELETE FROM w5_fail_rules WHERE check_time = $1::timestamp`, ts) })
	}
	clearFail := func(t *testing.T, ts string) {
		t.Helper()
		if _, err := db.Exec(`DELETE FROM w5_fail_rules WHERE check_time = $1::timestamp`, ts); err != nil {
			t.Fatalf("clear fail rule: %v", err)
		}
	}
	notifications := func(t *testing.T) int { t.Helper(); return countRows(t, db, `SELECT COUNT(*) FROM notifications`) }

	type result struct {
		code        int
		body, ctype string
		retryAfter  string
		logs        []w10Log
	}
	push := func(t *testing.T, h http.HandlerFunc, path, sn, body string) result {
		t.Helper()
		target := path + "?table=ATTLOG"
		if sn != "" {
			target += "&SN=" + sn
		}
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
		before := len(capture.snapshot())
		rec := httptest.NewRecorder()
		h(rec, req)
		return result{rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"), rec.Header().Get("Retry-After"), capture.snapshot()[before:]}
	}
	expectOK := func(t *testing.T, r result) {
		t.Helper()
		if r.code != http.StatusOK || r.body != "OK" || r.ctype != "text/plain" {
			t.Errorf("got HTTP %d %q (%s), want 200 text/plain OK", r.code, r.body, r.ctype)
		}
	}
	expectRetry := func(t *testing.T, r result) {
		t.Helper()
		if r.code != http.StatusServiceUnavailable || r.body != "RETRY" || r.ctype != "text/plain" || r.retryAfter == "" {
			t.Errorf("got HTTP %d %q (%s, Retry-After %q), want 503 text/plain RETRY with Retry-After", r.code, r.body, r.ctype, r.retryAfter)
		}
	}
	findLog := func(logs []w10Log, level slog.Level, prefix string) *w10Log {
		for i := range logs {
			if logs[i].Level == level && strings.HasPrefix(logs[i].Msg, prefix) {
				return &logs[i]
			}
		}
		return nil
	}

	for _, route := range w5Routes {
		h := route.h(app)

		t.Run(route.name+"/device rejections are ACKed 200 OK and logged", func(t *testing.T) {
			body, times := batch5()
			for _, c := range []struct{ sn, logPrefix string }{
				{"", "ADMSHandler: missing SN"},
				{"UNKNOWN-SN", "ADMSHandler: unregistered device"},
				{"W5-OFF", "ADMSHandler: disabled device"},
			} {
				r := push(t, h, route.path, c.sn, body)
				expectOK(t, r)
				if findLog(r.logs, slog.LevelWarn, c.logPrefix) == nil {
					t.Errorf("SN %q: no WARN %q (logs %v)", c.sn, c.logPrefix, r.logs)
				}
			}
			if n := stored(t, times); n != 0 {
				t.Errorf("rejected devices stored %d punches", n)
			}
		})

		t.Run(route.name+"/malformed lines are skipped, valid ones stored", func(t *testing.T) {
			r := push(t, h, route.path, "W5-DEVICE", "garbage\nmore garbage\n")
			expectOK(t, r)
			if findLog(r.logs, slog.LevelWarn, "ADMSHandler: no valid records") == nil {
				t.Errorf("garbage batch not logged (logs %v)", r.logs)
			}
			body, times := batch5()
			r = push(t, h, route.path, "W5-DEVICE", "junk line\n"+body+"W5-P1\tnot-a-date\t1\t1\n")
			expectOK(t, r)
			if n := stored(t, times); n != 5 {
				t.Errorf("stored %d of 5 valid punches", n)
			}
			sum := findLog(r.logs, slog.LevelInfo, "ADMSHandler: batch stored")
			if sum == nil || sum.Attrs["lines"] != "7" || sum.Attrs["parsed"] != "5" || sum.Attrs["malformed"] != "2" || sum.Attrs["inserted"] != "5" {
				t.Errorf("batch summary missing counts, got %+v", sum)
			}
		})

		t.Run(route.name+"/unknown PINs are skipped and listed", func(t *testing.T) {
			d := nextDay()
			body := fmt.Sprintf("W5-P1\t%s 07:00:00\t1\t1\nGHOST-1\t%s 07:01:00\t1\t1\nGHOST-2\t%s 07:02:00\t1\t1\nGHOST-1\t%s 07:03:00\t1\t1\nW5-P2\t%s 07:04:00\t1\t1\n", d, d, d, d, d)
			r := push(t, h, route.path, "W5-DEVICE", body)
			expectOK(t, r)
			if n := stored(t, []string{d + " 07:00:00", d + " 07:04:00"}); n != 2 {
				t.Errorf("stored %d of 2 known punches", n)
			}
			w := findLog(r.logs, slog.LevelWarn, "ADMSHandler: punches for unknown device PINs")
			if w == nil || w.Attrs["pins"] != "[GHOST-1 GHOST-2]" {
				t.Errorf("unknown PINs not listed once each, got %+v", w)
			}
		})

		t.Run(route.name+"/resent batch is idempotent", func(t *testing.T) {
			body, times := batch5()
			expectOK(t, push(t, h, route.path, "W5-DEVICE", body))
			r := push(t, h, route.path, "W5-DEVICE", body)
			expectOK(t, r)
			if n := stored(t, times); n != 5 {
				t.Errorf("stored %d, want 5 after resend", n)
			}
			if sum := findLog(r.logs, slog.LevelInfo, "ADMSHandler: batch stored"); sum == nil || sum.Attrs["inserted"] != "0" || sum.Attrs["duplicates"] != "5" {
				t.Errorf("resend summary = %+v, want inserted 0 duplicates 5", sum)
			}
		})

		t.Run(route.name+"/record rejected by the database is skipped, the rest kept", func(t *testing.T) {
			body, times := batch5()
			failAt(t, times[2], "reject")
			r := push(t, h, route.path, "W5-DEVICE", body)
			expectOK(t, r)
			if n := stored(t, times); n != 4 {
				t.Errorf("stored %d, want the 4 good punches", n)
			}
			if n := stored(t, times[2:3]); n != 0 {
				t.Error("rejected punch was stored")
			}
			rej := findLog(r.logs, slog.LevelError, "ADMSHandler: punch rejected")
			if rej == nil || rej.Attrs["pin"] != "W5-P3" || rej.Attrs["check_time"] != times[2] || !strings.Contains(rej.Attrs["error"], "w5 injected rejection") {
				t.Errorf("rejected record not logged with pin, time and error: %+v", rej)
			}
			if sum := findLog(r.logs, slog.LevelInfo, "ADMSHandler: batch stored"); sum == nil || sum.Attrs["rejected"] != "1" || sum.Attrs["inserted"] != "4" {
				t.Errorf("summary = %+v, want rejected 1 inserted 4", sum)
			}
		})

		for _, mode := range []string{"transient", "kill"} {
			t.Run(route.name+"/database failure on record 3 of 5 rolls back the batch ("+mode+")", func(t *testing.T) {
				body, times := batch5()
				beforeNotes := notifications(t)
				failAt(t, times[2], mode)
				r := push(t, h, route.path, "W5-DEVICE", body)
				expectRetry(t, r)
				if n := stored(t, times); n != 0 {
					t.Errorf("partial batch committed: %d of 5 punches stored", n)
				}
				e := findLog(r.logs, slog.LevelError, "ADMSHandler: batch rolled back")
				if e == nil || e.Attrs["parsed"] != "5" || e.Attrs["device_sn"] != "W5-DEVICE" || e.Attrs["error"] == "" {
					t.Errorf("rollback not logged with batch context: %+v", e)
				}
				time.Sleep(200 * time.Millisecond)
				if n := notifications(t); n != beforeNotes {
					t.Errorf("notification sent for a rolled-back punch (%d -> %d)", beforeNotes, n)
				}

				clearFail(t, times[2])
				expectOK(t, push(t, h, route.path, "W5-DEVICE", body))
				if n := stored(t, times); n != 5 {
					t.Errorf("device resend stored %d, want 5", n)
				}
				deadline := time.Now().Add(2 * time.Second)
				for notifications(t) != beforeNotes+1 && time.Now().Before(deadline) {
					time.Sleep(20 * time.Millisecond)
				}
				if n := notifications(t); n != beforeNotes+1 {
					t.Errorf("after the successful resend, notifications %d -> %d, want exactly one check-in", beforeNotes, n)
				}
			})
		}

		t.Run(route.name+"/database unavailable replies 503 and stores nothing", func(t *testing.T) {
			down, err := sql.Open("pgx", "postgres://localhost:1/none")
			if err != nil {
				t.Fatal(err)
			}
			down.Close()
			body, times := batch5()
			r := push(t, route.h(&handlers.AppEnv{DB: down}), route.path, "W5-DEVICE", body)
			expectRetry(t, r)
			if e := findLog(r.logs, slog.LevelError, "ADMSHandler: database unavailable verifying device"); e == nil || e.Attrs["device_sn"] != "W5-DEVICE" {
				t.Errorf("outage not logged with device context (logs %v)", r.logs)
			}
			if n := stored(t, times); n != 0 {
				t.Errorf("stored %d punches while the database was down", n)
			}
		})

		t.Run(route.name+"/notification failure does not block storage", func(t *testing.T) {
			d := nextDay()
			ts := d + " 07:10:00"
			if _, err := db.Exec(`ALTER FUNCTION get_student_status(int, date) RENAME TO w5_status_off`); err != nil {
				t.Fatal(err)
			}
			restored := false
			restore := func() {
				if !restored {
					db.Exec(`ALTER FUNCTION w5_status_off(int, date) RENAME TO get_student_status`)
					restored = true
				}
			}
			t.Cleanup(restore)
			r := push(t, h, route.path, "W5-DEVICE", fmt.Sprintf("W5-P5\t%s\t1\t1\n", ts))
			restore()
			expectOK(t, r)
			if n := stored(t, []string{ts}); n != 1 {
				t.Errorf("punch not stored when notification failed")
			}
			if findLog(r.logs, slog.LevelError, "ADMSHandler: notification skipped") == nil {
				t.Errorf("notification failure not logged (logs %v)", r.logs)
			}
		})
	}

	t.Run("large batch is stored in one transaction", func(t *testing.T) {
		d := time.Date(2025, 1, 1, 7, 0, 0, 0, time.UTC)
		var b strings.Builder
		for i := 0; i < 5000; i++ {
			fmt.Fprintf(&b, "W5-P%d\t%s\t1\t1\n", i%4+1, d.Add(time.Duration(i)*time.Second).Format("2006-01-02 15:04:05"))
		}
		start := time.Now()
		r := push(t, app.ADMSHandler, "/iclock/cdata", "W5-DEVICE", b.String())
		expectOK(t, r)
		if n := countRows(t, db, `SELECT COUNT(*) FROM attendance_logs WHERE check_time::date = '2025-01-01'`); n != 5000 {
			t.Errorf("stored %d of 5000", n)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("5000-punch batch took %v", d)
		}
	})

	t.Run("both ADMS routes are wired without JSON middleware", func(t *testing.T) {
		src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "api", "main.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, `"POST /iclock/cdata"`) || strings.Contains(line, `"/api/attendance/push",`) {
				if !strings.Contains(line, "appEnv.ADMSHandler") || strings.Contains(line, "DeviceAuthMiddleware") {
					t.Errorf("ADMS route must be served by ADMSHandler without DeviceAuthMiddleware: %s", strings.TrimSpace(line))
				}
			}
		}
	})

	if t.Failed() {
		t.Log("FAIL: ADMS error handling is not safe (see subtest errors above)")
	} else {
		t.Log("PASS: ADMS batches are atomic, every failure is logged, and devices retry only when useful")
	}
}
