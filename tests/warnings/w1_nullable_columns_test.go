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

	"future_kids/internal/auth"
	"future_kids/internal/handlers"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	w1DeviceSN    = "W1-NULL-DEVICE"
	w1ParentPhone = "+9647700000301"
)

// TestW1NullableColumnsHandling verifies that NULLs in nullable columns are
// scanned safely by every handler that reads them (audit warning W1).
func TestW1NullableColumnsHandling(t *testing.T) {
	db := setupThrowawayDB(t)
	auth.InitAuth("w1-test-secret")
	app := &handlers.AppEnv{DB: db}

	seed := []string{
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1, 'W1 Parent', '` + w1ParentPhone + `', 'unused')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id, grade, section) VALUES (1, 'W1 Student', 'W1-RFID-1', 1, 'G1', 'A')`,
		// No parent: ADMS punches for this student trigger no async notification writes.
		`INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES (2, 'W1 Orphan', 'W1-RFID-2', NULL)`,
		`INSERT INTO weekly_schedules (grade, section, day_of_week, period_number, subject_name, teacher_name) VALUES ('G1', 'A', 'Sunday', 1, 'Math', NULL)`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('` + w1DeviceSN + `', NULL, NULL)`,
		`INSERT INTO notifications (parent_phone, title, body, is_read, created_at) VALUES ('` + w1ParentPhone + `', 'W1 title', 'W1 body', NULL, NULL)`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed failed: %v\nquery: %s", err, q)
		}
	}

	parentToken, err := auth.GenerateParentToken(1, w1ParentPhone)
	if err != nil {
		t.Fatalf("GenerateParentToken: %v", err)
	}
	adminToken, err := auth.GenerateAdminToken("admin")
	if err != nil {
		t.Fatalf("GenerateAdminToken: %v", err)
	}

	t.Run("weekly_schedules.teacher_name/MobileScheduleHandler", func(t *testing.T) {
		rec := serve(t, handlers.AuthMiddleware(app.MobileScheduleHandler), http.MethodGet, "/api/mobile/schedule", parentToken, "")
		data := decodeData(t, rec, http.StatusOK)
		if len(data) != 1 {
			t.Fatalf("expected 1 schedule entry, got %d (row with NULL teacher_name was dropped)", len(data))
		}
		if got, ok := data[0]["teacher_name"].(string); !ok || got != "" {
			t.Errorf("teacher_name: expected \"\", got %#v", data[0]["teacher_name"])
		}
	})

	t.Run("notifications.is_read+created_at/MobileNotificationsHandler", func(t *testing.T) {
		rec := serve(t, handlers.AuthMiddleware(app.MobileNotificationsHandler), http.MethodGet, "/api/mobile/notifications", parentToken, "")
		data := decodeData(t, rec, http.StatusOK)
		if len(data) != 1 {
			t.Fatalf("expected 1 notification, got %d (row with NULL is_read/created_at was dropped)", len(data))
		}
		if got, ok := data[0]["is_read"].(bool); !ok || got {
			t.Errorf("is_read: expected false, got %#v", data[0]["is_read"])
		}
		createdAt, _ := data[0]["created_at"].(string)
		if _, err := time.Parse(time.RFC3339, createdAt); err != nil {
			t.Errorf("created_at: expected RFC3339 timestamp, got %q (%v)", createdAt, err)
		}
	})

	t.Run("devices.location_name+is_active/AdminDevicesHandler", func(t *testing.T) {
		rec := serve(t, handlers.AdminMiddleware(app.AdminDevicesHandler), http.MethodGet, "/api/admin/devices", adminToken, "")
		data := decodeData(t, rec, http.StatusOK)
		var device map[string]any
		for _, d := range data {
			if d["serial_number"] == w1DeviceSN {
				device = d
			}
		}
		if device == nil {
			t.Fatalf("device %s missing from response (row with NULL location_name/is_active was dropped)", w1DeviceSN)
		}
		if got, ok := device["location_name"].(string); !ok || got != "" {
			t.Errorf("location_name: expected \"\", got %#v", device["location_name"])
		}
		if got, ok := device["is_active"].(bool); !ok || !got {
			t.Errorf("is_active: expected true (column default), got %#v", device["is_active"])
		}
	})

	t.Run("devices.is_active/ADMSHandler", func(t *testing.T) {
		body := "W1-RFID-2\t2026-09-24 07:15:00\t1\t1\n"
		rec := serve(t, app.ADMSHandler, http.MethodPost, "/iclock/cdata?SN="+w1DeviceSN+"&table=ATTLOG", "", body)
		if rec.Code != http.StatusOK || rec.Body.String() != "OK" {
			t.Fatalf("expected 200 OK, got %d %q", rec.Code, rec.Body.String())
		}
		if n := countPunches(t, db, 2); n != 1 {
			t.Errorf("expected 1 stored punch from NULL-is_active device, got %d (punch silently dropped)", n)
		}
	})

	t.Run("devices.is_active/DeviceAuthMiddleware", func(t *testing.T) {
		body := `{"device_sn":"` + w1DeviceSN + `","rfid_tag":"W1-RFID-2","push_time":"2026-09-24 12:05:00"}`
		rec := serve(t, app.DeviceAuthMiddleware(app.HardwareAttendancePushHandler), http.MethodPost, "/api/attendance/push/json?SN="+w1DeviceSN, "", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if n := countPunches(t, db, 2); n != 2 {
			t.Errorf("expected 2 stored punches after JSON push, got %d", n)
		}
	})

	if t.Failed() {
		t.Log("FAIL: NULL values not handled correctly (see subtest errors above)")
	} else {
		t.Log("PASS: NULL values handled correctly")
	}
}

// setupThrowawayDB creates a uniquely named database, applies every up-migration,
// and registers cleanup that drops it. Override the server with TEST_PG_ADMIN_URL.
func setupThrowawayDB(t *testing.T) *sql.DB {
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

	name := fmt.Sprintf("fk_w1_test_%d", time.Now().UnixNano())
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
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open throwaway database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

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

func countPunches(t *testing.T, db *sql.DB, studentID int) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM attendance_logs WHERE student_id = $1", studentID).Scan(&n); err != nil {
		t.Fatalf("count punches: %v", err)
	}
	return n
}
