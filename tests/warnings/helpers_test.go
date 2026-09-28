package warnings

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"future_kids/internal/testdb"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// setupThrowawayDB returns an isolated, fully migrated database and its DSN (see internal/testdb).
func setupThrowawayDB(t *testing.T, label string) (*sql.DB, string) {
	t.Helper()
	return testdb.New(t, label)
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
