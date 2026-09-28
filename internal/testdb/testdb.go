// Package testdb gives each test its own throwaway PostgreSQL database.
//
// TEST_DATABASE_URL must point at a local PostgreSQL server (localhost, 127.0.0.1 or ::1).
// It is used only to create and drop throwaway databases; no existing database is touched.
package testdb

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	// EnvVar names the admin connection used to create and drop throwaway databases.
	EnvVar = "TEST_DATABASE_URL"
	// Prefix starts every throwaway database name.
	Prefix = "fk_test_"
	// StaleAfter is how old a leftover throwaway database must be before New drops it.
	// Younger ones may belong to another test process running in parallel.
	StaleAfter = time.Hour
)

var (
	labelRE = regexp.MustCompile(`^[a-z0-9]{1,12}$`)
	nameRE  = regexp.MustCompile(`^` + Prefix + `[a-z0-9]{1,12}_(\d+)_[0-9a-f]{8}$`)
)

// ValidateAdminURL checks that raw is a postgres:// URL for a local server.
// Error messages never include the password.
func ValidateAdminURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%s is not set; point it at a LOCAL PostgreSQL server, e.g. export %s=postgres://localhost:5432/postgres", EnvVar, EnvVar)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return nil, fmt.Errorf("refusing %s: must be a postgres:// URL with a host, e.g. postgres://localhost:5432/postgres", EnvVar)
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, "railway") {
		return nil, fmt.Errorf("refusing %s: host %q is a Railway database; tests only run against a local PostgreSQL server", EnvVar, host)
	}
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return u, nil
	}
	return nil, fmt.Errorf("refusing %s: host %q is not local; tests only run against localhost, 127.0.0.1 or ::1", EnvVar, host)
}

// New creates a uniquely named database on the server in TEST_DATABASE_URL, applies every
// up-migration, and drops it when t finishes, even if the test fails or panics.
// It returns a pool on the new database and its DSN. label ([a-z0-9]{1,12}) aids debugging.
func New(t testing.TB, label string) (*sql.DB, string) {
	t.Helper()
	if !labelRE.MatchString(label) {
		t.Fatalf("testdb: label %q must match %s", label, labelRE)
	}
	adminURL, err := ValidateAdminURL(os.Getenv(EnvVar))
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}

	admin, err := sql.Open("pgx", adminURL.String())
	if err != nil {
		t.Fatalf("testdb: open admin connection: %v", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Fatalf("testdb: PostgreSQL unreachable at %s: %v", adminURL.Redacted(), err)
	}
	dropStale(t, admin)

	name := Name(label, time.Now())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		admin.Close()
		t.Fatalf("testdb: create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"); err != nil {
			t.Errorf("testdb: drop %s: %v", name, err)
		}
		admin.Close()
	})

	dbURL := *adminURL
	dbURL.Path = "/" + name
	dsn := dbURL.String()
	t.Logf("testdb: using throwaway database %s on %s", name, dbURL.Host)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("testdb: open %s: %v", name, err)
	}
	t.Cleanup(func() { db.Close() })

	files, err := filepath.Glob(filepath.Join(MigrationsDir(), "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("testdb: no up-migrations in %s: %v", MigrationsDir(), err)
	}
	sort.Strings(files)
	for _, f := range files {
		sqlText, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("testdb: read %s: %v", f, err)
		}
		if _, err := db.Exec(string(sqlText)); err != nil {
			t.Fatalf("testdb: apply %s to %s: %v", filepath.Base(f), name, err)
		}
	}
	return db, dsn
}

// Name builds a throwaway database name: fk_test_<label>_<unix nanos>_<8 random hex>.
func Name(label string, at time.Time) string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%s%s_%d_%s", Prefix, label, at.UnixNano(), hex.EncodeToString(b))
}

// CreatedAt parses the creation time from a throwaway database name.
func CreatedAt(name string) (time.Time, error) {
	m := nameRE.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, errors.New("not a throwaway database name")
	}
	nanos, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, nanos), nil
}

// MigrationsDir returns the absolute path of db/migrations in this repository.
func MigrationsDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "db", "migrations")
}

// dropStale drops throwaway databases older than StaleAfter, left behind by killed test runs.
func dropStale(t testing.TB, admin *sql.DB) {
	t.Helper()
	rows, err := admin.Query(`SELECT datname FROM pg_database WHERE datname LIKE 'fk\_test\_%'`)
	if err != nil {
		t.Fatalf("testdb: list throwaway databases: %v", err)
	}
	var stale []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("testdb: scan database name: %v", err)
		}
		if created, err := CreatedAt(name); err == nil && time.Since(created) > StaleAfter {
			stale = append(stale, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("testdb: list throwaway databases: %v", err)
	}
	for _, name := range stale {
		if _, err := admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"); err != nil {
			t.Logf("testdb: could not drop stale %s: %v", name, err)
			continue
		}
		t.Logf("testdb: dropped stale throwaway database %s", name)
	}
}
