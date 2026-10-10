package warnings

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"future_kids/internal/testdb"
)

const w11ModeEnv = "W11_HELPER_MODE"

// TestW11Helper runs only as a subprocess of TestW11TestIsolation. It creates a throwaway
// database (or tries to), records its name, then ends abnormally so the parent can check cleanup.
func TestW11Helper(t *testing.T) {
	mode := os.Getenv(w11ModeEnv)
	if mode == "" {
		t.Skip("subprocess helper for TestW11TestIsolation")
	}
	_, dsn := testdb.New(t, "w11"+mode)
	if err := os.WriteFile(os.Getenv("W11_NAME_FILE"), []byte(dbName(t, dsn)), 0o600); err != nil {
		t.Fatalf("write name file: %v", err)
	}
	switch mode {
	case "panic":
		panic("w11: simulated test panic")
	case "fatal":
		t.Fatal("w11: simulated test failure")
	}
}

// runW11Helper runs TestW11Helper in a subprocess and returns its output, exit error and the
// database name it recorded (empty if it never created one).
func runW11Helper(t *testing.T, mode, adminURL string) (string, error, string) {
	t.Helper()
	nameFile := filepath.Join(t.TempDir(), "name")
	cmd := exec.Command(os.Args[0], "-test.run=^TestW11Helper$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), w11ModeEnv+"="+mode, "W11_NAME_FILE="+nameFile, testdb.EnvVar+"="+adminURL)
	out, err := cmd.CombinedOutput()
	name, _ := os.ReadFile(nameFile)
	return string(out), err, string(name)
}

// TestW11TestIsolation verifies audit warning W11: every test gets its own freshly migrated,
// uniquely named database on a local server only, which is dropped when the test ends
// (pass, fail or panic); unsafe TEST_DATABASE_URL values are refused; stale leftovers are
// swept; and no test or the e2e script connects to a persistent database.
func TestW11TestIsolation(t *testing.T) {
	adminURL, err := testdb.ValidateAdminURL(os.Getenv(testdb.EnvVar))
	if err != nil {
		t.Fatalf("%v", err)
	}
	admin := openDB(t, adminURL.String())
	exists := func(t *testing.T, name string) bool {
		t.Helper()
		var n int
		if err := admin.QueryRow(`SELECT COUNT(*) FROM pg_database WHERE datname = $1`, name).Scan(&n); err != nil {
			t.Fatalf("query pg_database: %v", err)
		}
		return n == 1
	}

	t.Run("validation rejects unsafe URLs", func(t *testing.T) {
		rejected := map[string]string{
			"":                        "is not set",
			"   ":                     "is not set",
			"host=localhost dbname=x": "must be a postgres:// URL",
			"mysql://localhost/x":     "must be a postgres:// URL",
			"postgres://postgres:s3cret@postgres.railway.internal:5432/railway":   "Railway",
			"postgresql://u:s3cret@containers-us-west-1.RAILWAY.app:6543/railway": "Railway",
			"postgres://u:s3cret@roundhouse.proxy.rlwy.net.railway.app:5432/prod": "Railway",
			"postgres://u:s3cret@db.example.com:5432/future_kids":                 "not local",
			"postgres://u:s3cret@10.0.0.5:5432/future_kids":                       "not local",
			"postgres://u:s3cret@localhost.evil.com:5432/x":                       "not local",
		}
		for raw, want := range rejected {
			_, err := testdb.ValidateAdminURL(raw)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%q: got %v, want error containing %q", raw, err, want)
			}
			if err != nil && strings.Contains(err.Error(), "s3cret") {
				t.Errorf("%q: error leaks the password: %v", raw, err)
			}
		}
		for _, raw := range []string{
			"postgres://localhost:5432/postgres",
			"postgresql://yisawi@127.0.0.1/postgres?sslmode=disable",
			"postgres://u:p@[::1]:5432/postgres",
			"postgres://LOCALHOST/postgres",
		} {
			if _, err := testdb.ValidateAdminURL(raw); err != nil {
				t.Errorf("%q: rejected a local URL: %v", raw, err)
			}
		}
	})

	t.Run("New refuses production and unset URLs before creating anything", func(t *testing.T) {
		for url, want := range map[string]string{
			"postgres://postgres:s3cret@postgres.railway.internal:5432/railway": "Railway",
			"": "is not set",
		} {
			out, err, name := runW11Helper(t, "refuse", url)
			if err == nil {
				t.Errorf("URL %q: subprocess passed, want a loud failure; output:\n%s", url, out)
			}
			if !strings.Contains(out, want) {
				t.Errorf("URL %q: failure does not mention %q:\n%s", url, want, out)
			}
			if name != "" {
				t.Errorf("URL %q: a database (%s) was created despite the refusal", url, name)
			}
			if strings.Contains(out, "s3cret") {
				t.Errorf("URL %q: output leaks the password", url)
			}
		}
	})

	t.Run("fresh database has the full, clean schema", func(t *testing.T) {
		db, _ := setupThrowawayDB(t, "w11")
		for _, table := range []string{"students", "parents", "devices", "attendance_logs", "student_leaves", "notifications", "banners", "banner_images", "announcements", "admins", "settings", "weekly_schedules"} {
			var ok bool
			if err := db.QueryRow(`SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&ok); err != nil || !ok {
				t.Errorf("table %s missing (err=%v)", table, err)
			}
		}
		var fn, nullable string
		var pinDefault sql.NullString
		if err := db.QueryRow(`SELECT pg_get_functiondef('get_student_status(int,date)'::regprocedure)`).Scan(&fn); err != nil || !strings.Contains(fn, "9 hours 31 minutes") {
			t.Errorf("get_student_status is not the 000017 version (err=%v)", err)
		}
		if err := db.QueryRow(`SELECT is_nullable FROM information_schema.columns WHERE table_name='attendance_logs' AND column_name='student_id'`).Scan(&nullable); err != nil || nullable != "NO" {
			t.Errorf("000016 not applied: attendance_logs.student_id nullable=%q err=%v", nullable, err)
		}
		if err := db.QueryRow(`SELECT column_default FROM information_schema.columns WHERE table_name='parents' AND column_name='pin_code'`).Scan(&pinDefault); err != nil || pinDefault.Valid {
			t.Errorf("000018 not applied: parents.pin_code default=%q err=%v", pinDefault.String, err)
		}
		for table, want := range map[string]int{"students": 0, "parents": 0, "devices": 0, "attendance_logs": 0, "student_leaves": 0, "notifications": 0, "admins": 1, "settings": 2} {
			if n := countRows(t, db, "SELECT COUNT(*) FROM "+table); n != want {
				t.Errorf("fresh database: %s has %d rows, want %d (migration seed only)", table, n, want)
			}
		}
		if n := countRows(t, db, `SELECT COUNT(*) FROM settings WHERE setting_key IN ('whatsapp_number', 'school_name')`); n != 2 {
			t.Errorf("fresh database: seeded settings are not exactly whatsapp_number (000010) and school_name (000020)")
		}
	})

	var mu sync.Mutex
	var names []string
	t.Run("parallel tests get separate databases", func(t *testing.T) {
		for i := 0; i < 6; i++ {
			t.Run(fmt.Sprintf("worker%d", i), func(t *testing.T) {
				t.Parallel()
				db, dsn := setupThrowawayDB(t, "w11par")
				mu.Lock()
				names = append(names, dbName(t, dsn))
				mu.Unlock()
				phone := fmt.Sprintf("+96470000011%02d", i)
				if _, err := db.Exec(`INSERT INTO parents (full_name, phone_number, pin_code) VALUES ('W11', $1, 'hash')`, phone); err != nil {
					t.Fatalf("insert: %v", err)
				}
				time.Sleep(50 * time.Millisecond)
				var got []string
				rows, err := db.Query(`SELECT phone_number FROM parents`)
				if err != nil {
					t.Fatalf("select: %v", err)
				}
				for rows.Next() {
					var p string
					rows.Scan(&p)
					got = append(got, p)
				}
				rows.Close()
				if len(got) != 1 || got[0] != phone {
					t.Errorf("worker %d sees parents %v, want only its own %s", i, got, phone)
				}
			})
		}
	})

	t.Run("databases are unique and dropped when their test ends", func(t *testing.T) {
		seen := map[string]bool{}
		for _, n := range names {
			if seen[n] {
				t.Errorf("database name %s reused", n)
			}
			seen[n] = true
			if !regexp.MustCompile(`^fk_test_w11par_\d+_[0-9a-f]{8}$`).MatchString(n) {
				t.Errorf("unexpected database name %s", n)
			}
			if exists(t, n) {
				t.Errorf("database %s still exists after its test finished", n)
			}
		}
		if len(names) != 6 {
			t.Errorf("recorded %d databases, want 6", len(names))
		}
	})

	for _, mode := range []string{"fatal", "panic"} {
		t.Run("database is dropped when a test ends with "+mode, func(t *testing.T) {
			out, err, name := runW11Helper(t, mode, adminURL.String())
			if err == nil {
				t.Fatalf("helper was expected to fail; output:\n%s", out)
			}
			if name == "" {
				t.Fatalf("helper never created a database; output:\n%s", out)
			}
			if mode == "panic" && !strings.Contains(out, "w11: simulated test panic") {
				t.Errorf("helper did not panic as expected; output:\n%s", out)
			}
			if exists(t, name) {
				t.Errorf("database %s survived a test %s", name, mode)
				admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
			}
		})
	}

	t.Run("stale leftovers are swept, live databases of other runs are kept", func(t *testing.T) {
		stale := testdb.Name("w11stale", time.Now().Add(-2*testdb.StaleAfter))
		live := testdb.Name("w11live", time.Now())
		for _, n := range []string{stale, live} {
			if _, err := admin.Exec("CREATE DATABASE " + n); err != nil {
				t.Fatalf("create %s: %v", n, err)
			}
		}
		t.Cleanup(func() {
			admin.Exec("DROP DATABASE IF EXISTS " + stale + " WITH (FORCE)")
			admin.Exec("DROP DATABASE IF EXISTS " + live + " WITH (FORCE)")
		})
		setupThrowawayDB(t, "w11")
		if exists(t, stale) {
			t.Errorf("stale database %s was not swept", stale)
		}
		if !exists(t, live) {
			t.Errorf("live database %s of another run was dropped", live)
		}
		if created, err := testdb.CreatedAt(live); err != nil || time.Since(created) > time.Minute {
			t.Errorf("CreatedAt(%s) = %v, %v", live, created, err)
		}
		if _, err := testdb.CreatedAt("future_kids"); err == nil {
			t.Error("CreatedAt accepted a non-throwaway name; the sweep could drop a real database")
		}
	})

	t.Run("no test or e2e script uses a persistent database", func(t *testing.T) {
		root := filepath.Join("..", "..")
		literalDSN := regexp.MustCompile(`sql\.Open\("pgx", "([^"]*)"\)`)
		var files []string
		for _, pattern := range []string{"tests/*_test.go", "tests/*/*_test.go", "internal/*/*_test.go"} {
			m, _ := filepath.Glob(filepath.Join(root, pattern))
			files = append(files, m...)
		}
		for _, f := range files {
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			text := string(src)
			if filepath.Base(f) == "w11_test_isolation_test.go" {
				continue
			}
			for _, bad := range []string{"TRUNCATE", "/future_kids", "TEST_PG_ADMIN_URL"} {
				if strings.Contains(text, bad) {
					t.Errorf("%s contains %q", f, bad)
				}
			}
			for _, m := range literalDSN.FindAllStringSubmatch(text, -1) {
				if m[1] != "postgres://localhost:1/none" {
					t.Errorf("%s opens a hard-coded database %q instead of using testdb", f, m[1])
				}
			}
		}
		e2e, err := os.ReadFile(filepath.Join(root, "tests", "e2e_test.sh"))
		if err != nil {
			t.Fatalf("read e2e_test.sh: %v", err)
		}
		script := string(e2e)
		for _, bad := range []string{"TRUNCATE", "future_kids", ":-http://localhost:8080"} {
			if strings.Contains(script, bad) {
				t.Errorf("e2e_test.sh contains %q", bad)
			}
		}
		for _, want := range []string{"fk_test_e2e_", "trap cleanup EXIT", "DROP DATABASE IF EXISTS", "*railway*", "localhost|127.0.0.1|::1"} {
			if !strings.Contains(script, want) {
				t.Errorf("e2e_test.sh is missing %q", want)
			}
		}
	})

	if t.Failed() {
		t.Log("FAIL: Test database isolation not enforced (see subtest errors above)")
	} else {
		t.Log("PASS: Every test runs in its own local throwaway database, dropped on exit")
	}
}
