package tests

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"future_kids/internal/handlers"

	_ "github.com/jackc/pgx/v5/stdlib"
	excelize "github.com/xuri/excelize/v2"
)

// newTestApp creates a minimal AppEnv for handler tests.
// It uses a real DB connection for integration-style tests,
// but for this test we only care about the Excel generation logic,
// so we pass nil — the handler generates the file before hitting the DB in this scenario.
// For a fully isolated unit test, replace with a mock DB.
func newTestApp(db *sql.DB) *handlers.AppEnv {
	return &handlers.AppEnv{DB: db}
}

// TestAdminExportExcelHandler_Structure validates the Excel file structure
// returned by the handler against the UI/UX specification:
//   - RTL view is enabled on Sheet1
//   - Cell A1 == "وزارة التربية والتعليم"
//   - Cell A2 == "مدرسة الرحمن الابتدائية الأهلية"
//   - Row 5 contains the exact 8 column headers
func TestAdminExportExcelHandler_Structure(t *testing.T) {
	// ── Arrange ──────────────────────────────────────────────────────────────
	// Open a real test DB. Adjust DSN to your local test database.
	// If you want a fully mocked test (no DB), you can stub AppEnv.DB
	// with a sqlmock library instead.
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://yisawi@localhost:5432/future_kids?sslmode=disable"
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("Failed to connect to test database: %v", err)
	}
	defer db.Close()

	app := newTestApp(db)

	// Build a GET request with a fixed date so results are deterministic
	req := httptest.NewRequest(http.MethodGet, "/api/admin/export/excel?date=2026-09-14", nil)
	rec := httptest.NewRecorder()

	// ── Act ──────────────────────────────────────────────────────────────────
	app.AdminExportExcelHandler(rec, req)

	// ── Assert: HTTP Layer ────────────────────────────────────────────────────
	res := rec.Result()
	defer res.Body.Close()

	if status := rec.Code; status != http.StatusOK {
		t.Errorf("Expected HTTP 200, got %v. Body: %s", status, rec.Body.String())
	}

	contentType := res.Header.Get("Content-Type")
	expectedCT := "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	if contentType != expectedCT {
		t.Errorf("Wrong Content-Type.\nExpected: %s\nGot:      %s", expectedCT, contentType)
	}

	contentDisp := res.Header.Get("Content-Disposition")
	if contentDisp == "" {
		t.Error("Expected Content-Disposition header to be set, but it was empty")
	}

	// ── Assert: Excel Structure ───────────────────────────────────────────────
	// Read the raw binary response into a buffer and parse it with excelize
	body := rec.Body.Bytes()
	if len(body) == 0 {
		t.Fatal("Response body is empty — Excel file was not generated")
	}

	f, err := excelize.OpenReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Failed to parse Excel response as a valid xlsx file: %v", err)
	}
	defer f.Close()

	sheet := "Sheet1"

	// ── Assert: RTL View ──────────────────────────────────────────────────────
	views, err := f.GetSheetView(sheet, 0)
	if err != nil {
		t.Fatalf("Failed to get sheet view options: %v", err)
	}
	if views.RightToLeft == nil || !*views.RightToLeft {
		t.Error("Assertion FAILED: RTL (Right-To-Left) is not enabled on Sheet1")
	} else {
		t.Log("✅ RTL: PASS")
	}

	// ── Assert: Official Header Row 1 ─────────────────────────────────────────
	cellA1, err := f.GetCellValue(sheet, "A1")
	if err != nil {
		t.Fatalf("Failed to read cell A1: %v", err)
	}
	if cellA1 != "وزارة التربية والتعليم" {
		t.Errorf("Assertion FAILED A1.\nExpected: وزارة التربية والتعليم\nGot:      %s", cellA1)
	} else {
		t.Log("✅ A1 Ministry Header: PASS")
	}

	// ── Assert: Official Header Row 2 ─────────────────────────────────────────
	cellA2, err := f.GetCellValue(sheet, "A2")
	if err != nil {
		t.Fatalf("Failed to read cell A2: %v", err)
	}
	if cellA2 != "مدرسة الرحمن الابتدائية الأهلية" {
		t.Errorf("Assertion FAILED A2.\nExpected: مدرسة الرحمن الابتدائية الأهلية\nGot:      %s", cellA2)
	} else {
		t.Log("✅ A2 School Name: PASS")
	}

	// ── Assert: Column Headers in Row 5 ──────────────────────────────────────
	expectedHeaders := []struct {
		cell  string
		value string
	}{
		{"A5", "رقم الطالب"},
		{"B5", "اسم الطالب"},
		{"C5", "الصف"},
		{"D5", "الشعبة"},
		{"E5", "ولي الأمر"},
		{"F5", "رقم الهاتف"},
		{"G5", "الحالة"},
		{"H5", "وقت البصمة"},
	}

	for _, h := range expectedHeaders {
		val, err := f.GetCellValue(sheet, h.cell)
		if err != nil {
			t.Errorf("Failed to read cell %s: %v", h.cell, err)
			continue
		}
		if val != h.value {
			t.Errorf("Column header mismatch at %s.\nExpected: %s\nGot:      %s", h.cell, h.value, val)
		} else {
			t.Logf("✅ Header %s (%s): PASS", h.cell, h.value)
		}
	}
}
