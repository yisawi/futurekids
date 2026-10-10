package warnings

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"future_kids/internal/auth"
	"future_kids/internal/background"
	"future_kids/internal/handlers"
	"future_kids/internal/testdb"
)

// TestGroup3N10WeekdayOrder verifies notice N10: the mobile schedule is ordered by student,
// then school week (Sunday→Thursday, then Friday, Saturday), then period. Arabic (with or
// without hamza) and English names in any case/spacing are recognized; unrecognized day
// names sort last, alphabetically, so the order is always deterministic.
func TestGroup3N10WeekdayOrder(t *testing.T) {
	db, _ := setupThrowawayDB(t, "g3n10")
	seed := []string{
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1001, 'G3 Parent', '+9647000001001', 'h')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id, grade, section) VALUES
			(1001, 'Arabic Kid', 'G3-1', 1001, 'G1', 'A'), (1002, 'English Kid', 'G3-2', 1001, 'G2', 'B')`,
		// Scrambled insertion order on purpose.
		`INSERT INTO weekly_schedules (grade, section, day_of_week, period_number, subject_name) VALUES
			('G1', 'A', 'Holiday',   1, 'x'),
			('G1', 'A', 'الخميس',    1, 'x'),
			('G1', 'A', 'الأحد',     2, 'x'),
			('G1', 'A', 'السبت',     1, 'x'),
			('G1', 'A', 'الثلاثاء',  1, 'x'),
			('G1', 'A', 'الإثنين',   2, 'x'),
			('G1', 'A', 'الأحد',     1, 'x'),
			('G1', 'A', 'الأربعاء',  1, 'x'),
			('G1', 'A', 'الاثنين',   1, 'x'),
			('G2', 'B', 'Thursday',  1, 'x'),
			('G2', 'B', 'Funday',    1, 'x'),
			('G2', 'B', 'Monday',    3, 'x'),
			('G2', 'B', ' Wednesday ', 1, 'x'),
			('G2', 'B', 'sunday',    1, 'x'),
			('G2', 'B', 'Tuesday',   2, 'x'),
			('G2', 'B', 'Monday',    1, 'x'),
			('G2', 'B', 'Aardvark',  1, 'x'),
			('G3', 'C', 'الأحد',     1, 'not this parent')`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed failed: %v\nquery: %s", err, q)
		}
	}
	auth.InitAuth("g3-test-secret")
	token, _ := auth.GenerateParentToken(1001, "+9647000001001", 0)
	app := &handlers.AppEnv{DB: db}

	req := httptest.NewRequest(http.MethodGet, "/api/mobile/schedule", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	app.AuthMiddleware(app.MobileScheduleHandler)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []struct {
			StudentID    int    `json:"student_id"`
			DayOfWeek    string `json:"day_of_week"`
			PeriodNumber int    `json:"period_number"`
			SubjectName  string `json:"subject_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range resp.Data {
		got = append(got, fmt.Sprintf("%d:%s:%d", e.StudentID, e.DayOfWeek, e.PeriodNumber))
	}
	want := []string{
		"1001:الأحد:1", "1001:الأحد:2", "1001:الاثنين:1", "1001:الإثنين:2", "1001:الثلاثاء:1",
		"1001:الأربعاء:1", "1001:الخميس:1", "1001:السبت:1", "1001:Holiday:1",
		"1002:sunday:1", "1002:Monday:1", "1002:Monday:3", "1002:Tuesday:2", "1002: Wednesday :1",
		"1002:Thursday:1", "1002:Aardvark:1", "1002:Funday:1",
	}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Errorf("schedule order\n got: %s\nwant: %s", strings.Join(got, " | "), strings.Join(want, " | "))
	}
}

// g3Schema returns a canonical description of the public schema (columns, indexes,
// constraints, functions), ignoring golang-migrate's own bookkeeping table.
func g3Schema(t *testing.T, db *sql.DB) string {
	t.Helper()
	queries := []string{
		`SELECT 'col ' || table_name || '.' || column_name || ' ' || data_type || ' ' || COALESCE(character_maximum_length::text, '-') || ' null=' || is_nullable || ' default=' || COALESCE(column_default, '-')
		 FROM information_schema.columns WHERE table_schema = 'public' AND table_name <> 'schema_migrations'`,
		`SELECT 'idx ' || indexdef FROM pg_indexes WHERE schemaname = 'public' AND tablename <> 'schema_migrations'`,
		`SELECT 'con ' || rel.relname || ' ' || c.conname || ' ' || pg_get_constraintdef(c.oid)
		 FROM pg_constraint c JOIN pg_class rel ON rel.oid = c.conrelid JOIN pg_namespace n ON n.oid = rel.relnamespace
		 WHERE n.nspname = 'public' AND rel.relname <> 'schema_migrations'`,
		`SELECT 'fn ' || pg_get_functiondef(p.oid) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'public'`,
	}
	var lines []string
	for _, q := range queries {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("schema query: %v", err)
		}
		for rows.Next() {
			var l string
			rows.Scan(&l)
			lines = append(lines, l)
		}
		rows.Close()
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// g3Apply runs one migration file's SQL against db.
func g3Apply(t *testing.T, db *sql.DB, file string) error {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testdb.MigrationsDir(), file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	_, err = db.Exec(string(b))
	return err
}

// TestGroup3N17IdempotentMigrations verifies notice N17: 000003 can be re-run on a migrated
// database, 000009's down-migration tolerates the students.parent_name drift, the resulting
// schema is unchanged, and golang-migrate treats a second "up" as a no-op.
func TestGroup3N17IdempotentMigrations(t *testing.T) {
	t.Run("000003 up re-runs cleanly on a migrated database", func(t *testing.T) {
		db, _ := setupThrowawayDB(t, "g3n17a")
		before := g3Schema(t, db)
		for i := 0; i < 2; i++ {
			if err := g3Apply(t, db, "000003_create_notifications_table.up.sql"); err != nil {
				t.Fatalf("re-running 000003 up (attempt %d): %v", i+1, err)
			}
		}
		if after := g3Schema(t, db); after != before {
			t.Error("re-running 000003 changed the schema")
		}
	})

	t.Run("schema is identical to the pre-edit 000003", func(t *testing.T) {
		db, _ := setupThrowawayDB(t, "g3n17b")
		legacy, _ := setupThrowawayDB(t, "g3n17c")
		if _, err := legacy.Exec(`DROP INDEX idx_notifications_phone`); err != nil {
			t.Fatal(err)
		}
		// The pre-edit statement, applied to an otherwise identical database.
		if _, err := legacy.Exec(`CREATE INDEX idx_notifications_phone ON notifications(parent_phone)`); err != nil {
			t.Fatal(err)
		}
		if a, b := g3Schema(t, db), g3Schema(t, legacy); a != b {
			t.Errorf("schema differs from the pre-edit index definition")
		}
		var def string
		db.QueryRow(`SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_notifications_phone'`).Scan(&def)
		if def != "CREATE INDEX idx_notifications_phone ON public.notifications USING btree (parent_phone)" {
			t.Errorf("index definition = %q", def)
		}
	})

	t.Run("000009 down tolerates the students.parent_name drift", func(t *testing.T) {
		db, _ := setupThrowawayDB(t, "g3n17d")
		downs, _ := filepath.Glob(filepath.Join(testdb.MigrationsDir(), "*.down.sql"))
		sort.Sort(sort.Reverse(sort.StringSlice(downs)))
		for _, f := range downs {
			if filepath.Base(f) < "000010" {
				break
			}
			if err := g3Apply(t, db, filepath.Base(f)); err != nil {
				t.Fatalf("roll back %s: %v", filepath.Base(f), err)
			}
		}
		if _, err := db.Exec(`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1, 'Drift Parent', '+9647000001101', 'h');
			INSERT INTO students (full_name, rfid_tag, parent_id) VALUES ('Drift Kid', 'D-1', 1);
			ALTER TABLE students ADD COLUMN parent_name VARCHAR(255)`); err != nil {
			t.Fatal(err)
		}
		if err := g3Apply(t, db, "000009_normalize_parents.down.sql"); err != nil {
			t.Fatalf("000009 down on a database that already has students.parent_name: %v", err)
		}
		var name, phone string
		if err := db.QueryRow(`SELECT parent_name, parent_phone FROM students WHERE rfid_tag = 'D-1'`).Scan(&name, &phone); err != nil || name != "Drift Parent" || phone != "+9647000001101" {
			t.Errorf("parent data not copied back: name=%q phone=%q err=%v", name, phone, err)
		}
	})

	t.Run("golang-migrate: up twice is a no-op, down all and up again", func(t *testing.T) {
		migrate, err := exec.LookPath("migrate")
		if err != nil {
			t.Skip("golang-migrate CLI not installed")
		}
		db, dsn := setupThrowawayDB(t, "g3n17e")
		reference := g3Schema(t, db)
		for _, tbl := range []string{"banner_images", "device_tokens", "settings", "notifications", "announcements", "weekly_schedules", "student_leaves", "banners", "admins", "attendance_logs", "devices", "students", "parents"} {
			if _, err := db.Exec("DROP TABLE IF EXISTS " + tbl + " CASCADE"); err != nil {
				t.Fatal(err)
			}
		}
		db.Exec(`DROP FUNCTION IF EXISTS get_student_status(INT, DATE)`)
		u, _ := url.Parse(dsn)
		q := u.Query()
		if q.Get("sslmode") == "" {
			q.Set("sslmode", "disable")
		}
		u.RawQuery = q.Encode()
		run := func(args ...string) string {
			cmd := exec.Command(migrate, append([]string{"-path", testdb.MigrationsDir(), "-database", u.String()}, args...)...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("migrate %v: %v\n%s", args, err, out)
			}
			return string(out)
		}
		run("up")
		if second := run("up"); !strings.Contains(second, "no change") {
			t.Errorf("second migrate up was not a no-op:\n%s", second)
		}
		if g3Schema(t, db) != reference {
			t.Error("schema after migrate up differs from the directly applied chain")
		}
		run("down", "-all")
		run("up")
		if g3Schema(t, db) != reference {
			t.Error("schema after down -all and up again differs")
		}
	})
}

// ── N16: graceful shutdown, exercised against the real API binary ───────────

// g3Output is a goroutine-safe buffer for the server's stdout/stderr.
type g3Output struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *g3Output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}
func (o *g3Output) String() string { o.mu.Lock(); defer o.mu.Unlock(); return o.buf.String() }

// messages returns the "msg" field of every JSON log line, in order.
func (o *g3Output) messages() []string {
	var msgs []string
	sc := bufio.NewScanner(strings.NewReader(o.String()))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var line map[string]any
		if json.Unmarshal(sc.Bytes(), &line) == nil {
			if m, ok := line["msg"].(string); ok {
				msgs = append(msgs, m)
			}
		}
	}
	return msgs
}

func (o *g3Output) index(msg string) int {
	for i, m := range o.messages() {
		if strings.HasPrefix(m, msg) {
			return i
		}
	}
	return -1
}

type g3Server struct {
	cmd    *exec.Cmd
	port   int
	out    *g3Output
	exited chan struct{}
	err    error
}

var (
	g3BinaryOnce sync.Once
	g3Binary     string
	g3BinaryErr  error
)

// g3APIBinary builds cmd/api once per test run; G3_API_BINARY overrides it (for example, to run
// these checks against a build of an older commit).
func g3APIBinary(t *testing.T) string {
	t.Helper()
	if b := os.Getenv("G3_API_BINARY"); b != "" {
		return b
	}
	g3BinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fk-g3-bin")
		if err != nil {
			g3BinaryErr = err
			return
		}
		g3Binary = filepath.Join(dir, "api")
		cmd := exec.Command("go", "build", "-o", g3Binary, "./cmd/api")
		cmd.Dir = filepath.Join("..", "..")
		if out, err := cmd.CombinedOutput(); err != nil {
			g3BinaryErr = fmt.Errorf("%v\n%s", err, out)
		}
	})
	if g3BinaryErr != nil {
		t.Fatalf("build cmd/api: %v", g3BinaryErr)
	}
	return g3Binary
}

// g3FirebaseJSON is a syntactically valid service account; nothing is ever sent with it.
func g3FirebaseJSON(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	b, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "fk-test", "private_key_id": "k1",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "fk@fk-test.iam.gserviceaccount.com", "client_id": "1",
		"token_uri": "https://oauth2.googleapis.com/token",
	})
	return string(b)
}

func g3FreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// g3Start launches the API binary against dsn. When waitHealthy is set it waits for /health.
func g3Start(t *testing.T, dsn string, port int, waitHealthy bool, extraEnv ...string) *g3Server {
	t.Helper()
	srv := &g3Server{port: port, out: &g3Output{}, exited: make(chan struct{})}
	srv.cmd = exec.Command(g3APIBinary(t))
	srv.cmd.Dir = t.TempDir()
	srv.cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"),
		"DATABASE_URL=" + dsn, "JWT_SECRET=g3", fmt.Sprintf("PORT=%d", port),
		"FIREBASE_CREDENTIALS_JSON=" + g3FirebaseJSON(t),
	}, extraEnv...)
	srv.cmd.Stdout, srv.cmd.Stderr = srv.out, srv.out
	if err := srv.cmd.Start(); err != nil {
		t.Fatalf("start api: %v", err)
	}
	go func() { srv.err = srv.cmd.Wait(); close(srv.exited) }()
	t.Cleanup(func() {
		select {
		case <-srv.exited:
		default:
			srv.cmd.Process.Kill()
			<-srv.exited
		}
	})
	if waitHealthy {
		deadline := time.Now().Add(15 * time.Second)
		for {
			if resp, err := http.Get(srv.url("/health")); err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					break
				}
			}
			select {
			case <-srv.exited:
				t.Fatalf("server exited during startup: %v\n%s", srv.err, srv.out)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("server never became healthy:\n%s", srv.out)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return srv
}

func (s *g3Server) url(path string) string { return fmt.Sprintf("http://127.0.0.1:%d%s", s.port, path) }

func (s *g3Server) signal(t *testing.T, sig os.Signal) time.Time {
	t.Helper()
	if err := s.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal: %v", err)
	}
	return time.Now()
}

// wait returns the exit code (-1 if the process was killed by a signal) and when it exited.
func (s *g3Server) wait(t *testing.T, within time.Duration) (int, time.Time) {
	t.Helper()
	select {
	case <-s.exited:
	case <-time.After(within):
		t.Fatalf("server did not exit within %v:\n%s", within, s.out)
	}
	at := time.Now()
	if s.err == nil {
		return 0, at
	}
	if ee, ok := s.err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return -1, at
		}
		return ee.ExitCode(), at
	}
	return -2, at
}

var g3ShutdownPhases = []string{
	"Shutdown: signal received",
	"Shutdown: HTTP server stopped",
	"Shutdown: cron stopped",
	"Shutdown: background work finished",
	"Shutdown: database closed",
	"Shutdown complete",
}

// TestGroup3N16GracefulShutdown verifies notice N16 against the real API binary.
// SHUTDOWN_TIMEOUT defaults to 25s so the drain completes inside a Railway draining period of
// 30s (RAILWAY_DEPLOYMENT_DRAINING_SECONDS=30); Railway's own default is 0s, i.e. an immediate
// SIGKILL, so that variable must be set for any graceful shutdown to happen.
func TestGroup3N16GracefulShutdown(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
		t.Run(fmt.Sprintf("idle server exits 0 on %v and logs every phase", sig), func(t *testing.T) {
			_, dsn := setupThrowawayDB(t, "g3n16a")
			srv := g3Start(t, dsn, g3FreePort(t), true)
			sent := srv.signal(t, sig)
			code, at := srv.wait(t, 10*time.Second)
			if code != 0 {
				t.Errorf("exit code %d, want 0", code)
			}
			if d := at.Sub(sent); d > 5*time.Second {
				t.Errorf("idle shutdown took %v", d)
			}
			last := -1
			for _, phase := range g3ShutdownPhases {
				i := srv.out.index(phase)
				if i < 0 {
					t.Errorf("missing log %q", phase)
					continue
				}
				if i < last {
					t.Errorf("log %q out of order", phase)
				}
				last = i
			}
			if t.Failed() {
				t.Logf("server output:\n%s", srv.out)
			}
		})
	}

	t.Run("in-flight ADMS batch and its notification finish; new connections are refused", func(t *testing.T) {
		db, dsn := setupThrowawayDB(t, "g3n16b")
		if _, err := db.Exec(`
			INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1, 'P', '+9647000001201', 'h');
			INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES (1, 'Kid', 'G3-SLOW', 1);
			INSERT INTO devices (serial_number, location_name, is_active) VALUES ('G3-DEV', 'Gate', true);
			CREATE FUNCTION g3_slow() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(0.05); RETURN NEW; END $$;
			CREATE TRIGGER g3_slow BEFORE INSERT ON attendance_logs FOR EACH ROW EXECUTE FUNCTION g3_slow();
			CREATE FUNCTION g3_slow_note() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(1.5); RETURN NEW; END $$;
			CREATE TRIGGER g3_slow_note BEFORE INSERT ON notifications FOR EACH ROW EXECUTE FUNCTION g3_slow_note();`); err != nil {
			t.Fatal(err)
		}
		srv := g3Start(t, dsn, g3FreePort(t), true)
		var body strings.Builder
		for i := 0; i < 40; i++ {
			fmt.Fprintf(&body, "G3-SLOW\t2026-05-03 07:00:%02d\t1\t1\n", i)
		}
		type result struct {
			code int
			body string
			err  error
		}
		done := make(chan result, 1)
		go func() {
			resp, err := (&http.Client{Timeout: 30 * time.Second}).Post(srv.url("/iclock/cdata?SN=G3-DEV&table=ATTLOG"), "text/plain", strings.NewReader(body.String()))
			if err != nil {
				done <- result{err: err}
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			done <- result{code: resp.StatusCode, body: string(b)}
		}()
		time.Sleep(500 * time.Millisecond)
		sent := srv.signal(t, syscall.SIGTERM)

		refused := false
		for i := 0; i < 40 && !refused; i++ {
			time.Sleep(50 * time.Millisecond)
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", srv.port), 200*time.Millisecond)
			if err != nil {
				refused = true
			} else {
				conn.Close()
			}
		}
		if !refused {
			t.Error("new connections were still accepted 2s after SIGTERM")
		}

		r := <-done
		if r.err != nil || r.code != http.StatusOK || r.body != "OK" {
			t.Errorf("in-flight ADMS request: code=%d body=%q err=%v, want 200 OK", r.code, r.body, r.err)
		}
		code, at := srv.wait(t, 30*time.Second)
		if code != 0 {
			t.Errorf("exit code %d, want 0", code)
		}
		if d := at.Sub(sent); d > 25*time.Second {
			t.Errorf("shutdown took %v, longer than SHUTDOWN_TIMEOUT", d)
		}
		if n := countRows(t, db, `SELECT COUNT(*) FROM attendance_logs`); n != 40 {
			t.Errorf("stored %d of 40 punches, want the whole batch", n)
		}
		if n := countRows(t, db, `SELECT COUNT(*) FROM notifications`); n != 1 {
			t.Errorf("notification history rows = %d, want 1 (background write finished before exit)", n)
		}
		if t.Failed() {
			t.Logf("server output:\n%s", srv.out)
		}
	})

	t.Run("running absence job finishes before the cron phase completes", func(t *testing.T) {
		db, dsn := setupThrowawayDB(t, "g3n16c")
		if _, err := db.Exec(`
			INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1, 'P', '+9647000001202', 'h');
			INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES (1, 'Absent Kid', 'G3-ABS', 1);
			CREATE FUNCTION g3_slow_note() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(1.5); RETURN NEW; END $$;
			CREATE TRIGGER g3_slow_note BEFORE INSERT ON notifications FOR EACH ROW EXECUTE FUNCTION g3_slow_note();`); err != nil {
			t.Fatal(err)
		}
		srv := g3Start(t, dsn, g3FreePort(t), true, "ABSENCE_CRON_SCHEDULE=@every 1s")
		deadline := time.Now().Add(10 * time.Second)
		for srv.out.index("ProcessDailyAbsences: starting") < 0 {
			if time.Now().After(deadline) {
				t.Fatalf("absence job never started:\n%s", srv.out)
			}
			time.Sleep(20 * time.Millisecond)
		}
		srv.signal(t, syscall.SIGTERM)
		code, _ := srv.wait(t, 30*time.Second)
		finished, stopped := srv.out.index("ProcessDailyAbsences: finished"), srv.out.index("Shutdown: cron stopped")
		if code != 0 || finished < 0 || stopped < 0 || finished > stopped {
			t.Errorf("exit=%d job-finished-log=%d cron-stopped-log=%d; want exit 0 and the job to finish before the cron phase ends\n%s", code, finished, stopped, srv.out)
		}
		if n := countRows(t, db, `SELECT COUNT(*) FROM notifications`); n < 1 {
			t.Error("the running absence job's notification was lost")
		}
	})

	t.Run("timeout exceeded: ERROR log, non-zero exit, batch rolled back not partial", func(t *testing.T) {
		db, dsn := setupThrowawayDB(t, "g3n16d")
		if _, err := db.Exec(`
			INSERT INTO students (id, full_name, rfid_tag) VALUES (1, 'Kid', 'G3-SLOWER');
			INSERT INTO devices (serial_number, location_name, is_active) VALUES ('G3-DEV', 'Gate', true);
			CREATE FUNCTION g3_slower() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(0.2); RETURN NEW; END $$;
			CREATE TRIGGER g3_slower BEFORE INSERT ON attendance_logs FOR EACH ROW EXECUTE FUNCTION g3_slower();`); err != nil {
			t.Fatal(err)
		}
		srv := g3Start(t, dsn, g3FreePort(t), true, "SHUTDOWN_TIMEOUT=1s")
		var body strings.Builder
		for i := 0; i < 30; i++ {
			fmt.Fprintf(&body, "G3-SLOWER\t2026-05-04 07:00:%02d\t1\t1\n", i)
		}
		go http.Post(srv.url("/iclock/cdata?SN=G3-DEV&table=ATTLOG"), "text/plain", strings.NewReader(body.String()))
		time.Sleep(500 * time.Millisecond)
		sent := srv.signal(t, syscall.SIGTERM)
		code, at := srv.wait(t, 15*time.Second)
		if code != 1 {
			t.Errorf("exit code %d, want 1 when the timeout is exceeded", code)
		}
		if d := at.Sub(sent); d > 3*time.Second {
			t.Errorf("process took %v to exit with SHUTDOWN_TIMEOUT=1s", d)
		}
		if srv.out.index("Shutdown: timeout exceeded") < 0 {
			t.Errorf("no ERROR log for the exceeded timeout:\n%s", srv.out)
		}
		deadline := time.Now().Add(15 * time.Second)
		for countRows(t, db, `SELECT COUNT(*) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()`) > 0 && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
		if n := countRows(t, db, `SELECT COUNT(*) FROM attendance_logs`); n != 0 {
			t.Errorf("%d of 30 punches committed from an interrupted batch; want 0 (never partial)", n)
		}
	})

	t.Run("a server that fails to start exits non-zero", func(t *testing.T) {
		_, dsn := setupThrowawayDB(t, "g3n16e")
		busy, err := net.Listen("tcp", ":0") // the same all-interfaces address the server binds
		if err != nil {
			t.Fatal(err)
		}
		defer busy.Close()
		srv := g3Start(t, dsn, busy.Addr().(*net.TCPAddr).Port, false)
		if code, _ := srv.wait(t, 15*time.Second); code != 1 {
			t.Errorf("exit code %d with the port taken, want 1\n%s", code, srv.out)
		}
	})
}

// TestGroup3BackgroundGroup verifies the tracker shutdown uses for fire-and-forget work:
// Wait returns once every task has finished (no goroutines left behind), times out with the
// number still pending, survives panicking tasks, and a nil Group still runs work.
func TestGroup3BackgroundGroup(t *testing.T) {
	settle := func() int {
		n := runtime.NumGoroutine()
		for i := 0; i < 50; i++ {
			time.Sleep(10 * time.Millisecond)
			if m := runtime.NumGoroutine(); m <= n {
				n = m
			}
		}
		return n
	}

	t.Run("wait drains every task and leaves no goroutines", func(t *testing.T) {
		base := settle()
		g := &background.Group{}
		var mu sync.Mutex
		done := 0
		for i := 0; i < 100; i++ {
			g.Go("task", func() {
				time.Sleep(time.Duration(i%10) * time.Millisecond)
				mu.Lock()
				done++
				mu.Unlock()
			})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if pending, err := g.Wait(ctx); err != nil || pending != 0 {
			t.Fatalf("Wait = %d, %v", pending, err)
		}
		mu.Lock()
		if done != 100 {
			t.Errorf("%d of 100 tasks finished before Wait returned", done)
		}
		mu.Unlock()
		if g.Running() != 0 {
			t.Errorf("Running = %d after Wait", g.Running())
		}
		if after := settle(); after > base {
			t.Errorf("goroutines: %d before, %d after — leak", base, after)
		}
	})

	t.Run("wait times out and reports what is still running", func(t *testing.T) {
		g := &background.Group{}
		release := make(chan struct{})
		g.Go("stuck", func() { <-release })
		g.Go("quick", func() {})
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if pending, err := g.Wait(ctx); err != context.DeadlineExceeded || pending != 1 {
			t.Errorf("Wait = %d, %v; want 1 pending and DeadlineExceeded", pending, err)
		}
		close(release)
		if pending, err := g.Wait(context.Background()); err != nil || pending != 0 {
			t.Errorf("second Wait = %d, %v", pending, err)
		}
	})

	t.Run("panicking task is recovered and counted as finished", func(t *testing.T) {
		g := &background.Group{}
		g.Go("boom", func() { panic("g3 test panic") })
		if pending, err := g.Wait(context.Background()); err != nil || pending != 0 || g.Running() != 0 {
			t.Errorf("after a panic: Wait = %d, %v, Running = %d", pending, err, g.Running())
		}
	})

	t.Run("nil group still runs work", func(t *testing.T) {
		var g *background.Group
		ran := make(chan struct{})
		g.Go("untracked", func() { close(ran) })
		select {
		case <-ran:
		case <-time.After(time.Second):
			t.Fatal("nil Group did not run the task")
		}
		if pending, err := g.Wait(context.Background()); err != nil || pending != 0 {
			t.Errorf("nil Wait = %d, %v", pending, err)
		}
	})

	t.Run("concurrent Go and Wait are safe", func(t *testing.T) {
		g := &background.Group{}
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 50; j++ {
					g.Go("burst", func() {})
				}
				g.Wait(context.Background())
			}()
		}
		wg.Wait()
		if pending, err := g.Wait(context.Background()); err != nil || pending != 0 {
			t.Errorf("final Wait = %d, %v", pending, err)
		}
	})
}
