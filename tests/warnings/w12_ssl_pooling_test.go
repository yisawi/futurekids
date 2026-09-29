package warnings

import (
	"context"
	"database/sql"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"future_kids/internal/config"
	"future_kids/internal/database"
)

// TestW12SSLAndPooling verifies audit warning W12: TLS is never silently disabled (require by
// default, disable only for loopback or when explicitly configured), invalid sslmode or pool
// settings fail startup, and pool limits come from one place and are actually applied.
func TestW12SSLAndPooling(t *testing.T) {
	capture := &w10Capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	load := func(t *testing.T, env map[string]string) (*config.Config, error) {
		t.Helper()
		for _, k := range []string{"DATABASE_URL", "DB_URL", "DB_SSL_MODE", "DB_MAX_OPEN_CONNS", "DB_MAX_IDLE_CONNS", "DB_CONN_MAX_LIFETIME"} {
			t.Setenv(k, env[k])
		}
		return config.LoadConfig()
	}
	query := func(t *testing.T, raw string) url.Values {
		t.Helper()
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return u.Query()
	}

	sslCases := []struct {
		name, dsn, env, want string
		remoteWarn           bool
	}{
		{"URL sslmode=require is kept", "postgres://u:p@db.example.com:5432/app?sslmode=require", "", "require", false},
		{"URL sslmode=verify-full is kept", "postgres://u:p@db.example.com/app?sslmode=verify-full", "", "verify-full", false},
		{"URL sslmode wins over DB_SSL_MODE", "postgres://u:p@db.example.com/app?sslmode=require", "disable", "require", false},
		{"remote, no sslmode, no env → require", "postgres://u:p@db.example.com:5432/app", "", "require", false},
		{"Railway internal host defaults to require", "postgresql://postgres:p@postgres.railway.internal:5432/railway", "", "require", false},
		{"remote + DB_SSL_MODE=disable → disable, warned", "postgres://u:p@db.example.com/app", "disable", "disable", true},
		{"DB_SSL_MODE is normalized", "postgres://u:p@db.example.com/app", "  Require ", "require", false},
		{"localhost defaults to disable", "postgres://yisawi@localhost:5432/app", "", "disable", false},
		{"127.0.0.1 defaults to disable", "postgres://u@127.0.0.1/app", "", "disable", false},
		{"::1 defaults to disable", "postgres://u@[::1]:5432/app", "", "disable", false},
		{"localhost + DB_SSL_MODE=require → require", "postgres://u@localhost/app", "require", "require", false},
		{"URL sslmode=disable on a remote host is kept, warned", "postgres://u:p@db.example.com/app?sslmode=disable", "", "disable", true},
	}
	for _, c := range sslCases {
		t.Run("sslmode/"+c.name, func(t *testing.T) {
			before := len(capture.snapshot())
			cfg, err := load(t, map[string]string{"DATABASE_URL": c.dsn, "DB_SSL_MODE": c.env})
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			q := query(t, cfg.DBUrl)
			if got := q.Get("sslmode"); got != c.want {
				t.Errorf("sslmode = %q, want %q (DBUrl %s)", got, c.want, cfg.DBUrl)
			}
			if len(q["sslmode"]) != 1 {
				t.Errorf("sslmode appears %d times in %s", len(q["sslmode"]), cfg.DBUrl)
			}
			if q.Get("timezone") != "Asia/Baghdad" {
				t.Errorf("session timezone lost: %s", cfg.DBUrl)
			}
			warned := false
			for _, l := range capture.snapshot()[before:] {
				if l.Level == slog.LevelWarn && strings.HasPrefix(l.Msg, "TLS is not enforced for a remote database") {
					warned = true
				}
			}
			if warned != c.remoteWarn {
				t.Errorf("remote-TLS warning logged = %v, want %v", warned, c.remoteWarn)
			}
		})
	}

	t.Run("sslmode/URL parameters, credentials and explicit timezone are preserved", func(t *testing.T) {
		cfg, err := load(t, map[string]string{"DATABASE_URL": "postgres://u:s3cret@db.example.com:6543/app?application_name=fk&timezone=UTC"})
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(cfg.DBUrl)
		pw, _ := u.User.Password()
		if u.User.Username() != "u" || pw != "s3cret" || u.Host != "db.example.com:6543" || u.Path != "/app" ||
			u.Query().Get("application_name") != "fk" || u.Query().Get("timezone") != "UTC" || u.Query().Get("sslmode") != "require" {
			t.Errorf("URL not preserved correctly: %s", cfg.DBUrl)
		}
	})

	for _, c := range []struct{ name, dsn, env, want string }{
		{"invalid DB_SSL_MODE", "postgres://u@db.example.com/app", "maybe", "use one of disable, allow, prefer, require, verify-ca, verify-full"},
		{"invalid sslmode in URL", "postgres://u@db.example.com/app?sslmode=on", "", `invalid sslmode "on" (from DATABASE_URL/DB_URL)`},
		{"keyword DSN", "host=db.example.com dbname=app", "", "must be a postgres:// URL"},
		{"other scheme", "mysql://db.example.com/app", "", "must be a postgres:// URL"},
	} {
		t.Run("sslmode/rejects "+c.name, func(t *testing.T) {
			_, err := load(t, map[string]string{"DATABASE_URL": c.dsn, "DB_SSL_MODE": c.env})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want error containing %q", err, c.want)
			}
		})
	}

	t.Run("sslmode/empty URL is left for NewConnection to report", func(t *testing.T) {
		cfg, err := load(t, map[string]string{})
		if err != nil || cfg.DBUrl != "" {
			t.Errorf("got %q, %v; want empty URL and no error", cfg.DBUrl, err)
		}
		if _, err := database.NewConnection(cfg.DBUrl, database.DefaultPool); err == nil || !strings.Contains(err.Error(), "DSN is empty") {
			t.Errorf("NewConnection(empty) = %v, want DSN is empty", err)
		}
	})

	t.Run("pool/production-safe defaults", func(t *testing.T) {
		want := database.Pool{MaxOpenConns: 25, MaxIdleConns: 5, ConnMaxLifetime: 5 * time.Minute}
		if database.DefaultPool != want {
			t.Errorf("DefaultPool = %+v, want %+v", database.DefaultPool, want)
		}
		cfg, err := load(t, map[string]string{"DATABASE_URL": "postgres://u@db.example.com/app"})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DBPool != want {
			t.Errorf("config pool = %+v, want defaults %+v", cfg.DBPool, want)
		}
		for key := range query(t, cfg.DBUrl) {
			if strings.Contains(key, "pool") || strings.Contains(key, "conn") {
				t.Errorf("pool setting %q leaked into the DSN: %s", key, cfg.DBUrl)
			}
		}
	})

	t.Run("pool/environment overrides", func(t *testing.T) {
		cfg, err := load(t, map[string]string{"DATABASE_URL": "postgres://u@db.example.com/app", "DB_MAX_OPEN_CONNS": "10", "DB_MAX_IDLE_CONNS": "2", "DB_CONN_MAX_LIFETIME": "90s"})
		if err != nil {
			t.Fatal(err)
		}
		if want := (database.Pool{MaxOpenConns: 10, MaxIdleConns: 2, ConnMaxLifetime: 90 * time.Second}); cfg.DBPool != want {
			t.Errorf("pool = %+v, want %+v", cfg.DBPool, want)
		}
	})

	for _, c := range []struct{ name, key, val, want string }{
		{"non-numeric max open", "DB_MAX_OPEN_CONNS", "many", "invalid DB_MAX_OPEN_CONNS"},
		{"zero max open", "DB_MAX_OPEN_CONNS", "0", "invalid DB_MAX_OPEN_CONNS"},
		{"negative max idle", "DB_MAX_IDLE_CONNS", "-1", "invalid DB_MAX_IDLE_CONNS"},
		{"idle above open", "DB_MAX_IDLE_CONNS", "30", "must not exceed DB_MAX_OPEN_CONNS"},
		{"lifetime without unit", "DB_CONN_MAX_LIFETIME", "5", "invalid DB_CONN_MAX_LIFETIME"},
		{"zero lifetime", "DB_CONN_MAX_LIFETIME", "0s", "invalid DB_CONN_MAX_LIFETIME"},
	} {
		t.Run("pool/rejects "+c.name, func(t *testing.T) {
			_, err := load(t, map[string]string{"DATABASE_URL": "postgres://u@db.example.com/app", c.key: c.val})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want error containing %q", err, c.want)
			}
		})
	}

	_, dsn := setupThrowawayDB(t, "w12")

	t.Run("pool/limits are applied to the real pool", func(t *testing.T) {
		before := len(capture.snapshot())
		db, err := database.NewConnection(dsn, database.Pool{MaxOpenConns: 3, MaxIdleConns: 1, ConnMaxLifetime: 300 * time.Millisecond})
		if err != nil {
			t.Fatalf("NewConnection: %v", err)
		}
		defer db.Close()

		if got := db.Stats().MaxOpenConnections; got != 3 {
			t.Errorf("MaxOpenConnections = %d, want 3", got)
		}

		ctx := context.Background()
		var conns []*sql.Conn
		for i := 0; i < 3; i++ {
			c, err := db.Conn(ctx)
			if err != nil {
				t.Fatalf("conn %d: %v", i, err)
			}
			conns = append(conns, c)
		}
		waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		if _, err := db.Conn(waitCtx); err == nil {
			t.Error("a 4th connection was opened beyond MaxOpenConns=3")
		}
		cancel()
		if db.Stats().WaitCount == 0 {
			t.Error("pool did not make the 4th caller wait")
		}
		for _, c := range conns {
			c.Close()
		}
		if s := db.Stats(); s.Idle > 1 || s.MaxIdleClosed < 2 {
			t.Errorf("after releasing 3 connections: idle=%d maxIdleClosed=%d, want idle<=1 and >=2 closed", s.Idle, s.MaxIdleClosed)
		}

		deadline := time.Now().Add(4 * time.Second)
		for db.Stats().MaxLifetimeClosed == 0 && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if db.Stats().MaxLifetimeClosed == 0 {
			t.Error("no connection was recycled after ConnMaxLifetime")
		}

		var info *w10Log
		for _, l := range capture.snapshot()[before:] {
			if l.Msg == "Connected to the database" {
				l := l
				info = &l
			}
		}
		if info == nil || info.Attrs["max_open_conns"] != "3" || info.Attrs["max_idle_conns"] != "1" || info.Attrs["conn_max_lifetime"] != "300ms" || info.Attrs["sslmode"] == "" || info.Attrs["dsn"] == "" {
			t.Errorf("startup log missing DSN, sslmode or pool settings: %+v", info)
		}
	})

	t.Run("pool/concurrent load stays within MaxOpenConns", func(t *testing.T) {
		db, err := database.NewConnection(dsn, database.Pool{MaxOpenConns: 4, MaxIdleConns: 2, ConnMaxLifetime: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var wg sync.WaitGroup
		var mu sync.Mutex
		peak := 0
		for i := 0; i < 40; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := db.Exec(`SELECT pg_sleep(0.02)`); err != nil {
					t.Errorf("query: %v", err)
				}
				mu.Lock()
				if n := db.Stats().OpenConnections; n > peak {
					peak = n
				}
				mu.Unlock()
			}()
		}
		wg.Wait()
		if peak > 4 {
			t.Errorf("peak open connections %d exceeded MaxOpenConns 4", peak)
		}
	})

	t.Run("connection failure names the sslmode and hides the password", func(t *testing.T) {
		_, err := database.NewConnection("postgres://u:s3cret@localhost:1/app?sslmode=require&connect_timeout=2", database.DefaultPool)
		if err == nil {
			t.Fatal("expected a connection error")
		}
		msg := err.Error()
		if !strings.Contains(msg, "sslmode=require") || !strings.Contains(msg, "DB_SSL_MODE=disable") {
			t.Errorf("error lacks sslmode or hint: %s", msg)
		}
		if strings.Contains(msg, "s3cret") {
			t.Errorf("error leaks the password: %s", msg)
		}
	})

	t.Run("source: no forced sslmode=disable and pool set only in database", func(t *testing.T) {
		poolCall := regexp.MustCompile(`\.Set(MaxOpenConns|MaxIdleConns|ConnMaxLifetime|ConnMaxIdleTime)\(`)
		forced := regexp.MustCompile(`sslmode=disable|"sslmode", "disable"`)
		files, _ := filepath.Glob(filepath.Join("..", "..", "internal", "*", "*.go"))
		main, _ := filepath.Glob(filepath.Join("..", "..", "cmd", "*", "*.go"))
		for _, f := range append(files, main...) {
			if strings.HasSuffix(f, "_test.go") || strings.Contains(f, "testdb") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if forced.Match(src) {
				t.Errorf("%s hard-codes sslmode=disable", f)
			}
			if poolCall.Match(src) && filepath.Base(filepath.Dir(f)) != "database" {
				t.Errorf("%s configures the connection pool; only internal/database may", f)
			}
		}
	})

	if t.Failed() {
		t.Log("FAIL: Connection security or pooling is misconfigured (see subtest errors above)")
	} else {
		t.Log("PASS: TLS required by default, sslmode and pool validated, pool configured in one place")
	}
}
