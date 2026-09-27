package warnings

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// setupThrowawayDB creates a uniquely named database, applies every up-migration,
// and registers cleanup that drops it. It returns the pool and the database DSN.
// Override the server with TEST_PG_ADMIN_URL.
func setupThrowawayDB(t *testing.T, prefix string) (*sql.DB, string) {
	t.Helper()

	adminURL := os.Getenv("TEST_PG_ADMIN_URL")
	if adminURL == "" {
		adminURL = "postgres://localhost:5432/postgres?sslmode=disable"
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Fatalf("Postgres unreachable at %s (set TEST_PG_ADMIN_URL): %v", adminURL, err)
	}

	name := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		admin.Close()
		t.Fatalf("create throwaway database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"); err != nil {
			t.Errorf("drop throwaway database %s: %v", name, err)
		}
		admin.Close()
	})

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse TEST_PG_ADMIN_URL: %v", err)
	}
	u.Path = "/" + name
	dsn := u.String()
	db := openDB(t, dsn)

	files, err := filepath.Glob(filepath.Join("..", "..", "db", "migrations", "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no up-migrations found: %v", err)
	}
	sort.Strings(files)
	for _, f := range files {
		sqlText, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := db.Exec(string(sqlText)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	return db, dsn
}

// openDB opens a pool on dsn and closes it before the throwaway database is dropped.
func openDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dsn, err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping %s: %v", dsn, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func serve(t *testing.T, h http.HandlerFunc, method, target, token, body string) (rec *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec = httptest.NewRecorder()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("handler panicked on %s %s: %v", method, target, r)
		}
	}()
	h(rec, req)
	return rec
}

func decodeData(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) []map[string]any {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("expected HTTP %d, got %d: %s", wantStatus, rec.Code, rec.Body.String())
	}
	var resp struct {
		Status string           `json:"status"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, rec.Body.String())
	}
	if resp.Status != "success" {
		t.Fatalf("expected status success, got %q", resp.Status)
	}
	return resp.Data
}
