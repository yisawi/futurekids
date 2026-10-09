package tests

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"future_kids/internal/handlers"
	"future_kids/internal/testdb"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestAttendanceTimeWindows(t *testing.T) {
	// 1. Isolated, freshly migrated database (dropped when the test ends)
	db, _ := testdb.New(t, "attn")

	// 2. Setup mock data
	var studentID int
	err := db.QueryRow(`
		INSERT INTO students (full_name, rfid_tag, is_active, created_at)
		VALUES ('Time Window Kid', 'RFID-TIME', true, '2026-01-01 08:00')
		RETURNING id
	`).Scan(&studentID)
	if err != nil {
		t.Fatalf("Failed to insert test student: %v", err)
	}

	_, err = db.Exec(`
		INSERT INTO devices (serial_number, location_name, is_active)
		VALUES ('DEVICE-TEST', 'Gate 1', true)
	`)
	if err != nil {
		t.Fatalf("Failed to insert test device: %v", err)
	}

	// 3. Simulate chaotic punches on a fixed day
	baseDay := "2026-09-21 "
	punches := []string{
		baseDay + "07:15:00", // Valid In
		baseDay + "07:18:00", // Spam In
		baseDay + "10:45:00", // Dead Zone
		baseDay + "12:30:00", // Valid Out
		baseDay + "12:35:00", // Spam Out
	}

	for _, p := range punches {
		pt, _ := time.Parse("2006-01-02 15:04:05", p)
		_, err := db.Exec(`
			INSERT INTO attendance_logs (student_id, device_sn, check_time)
			VALUES ($1, 'DEVICE-TEST', $2)
		`, studentID, pt)
		if err != nil {
			t.Fatalf("Failed to insert punch %s: %v", p, err)
		}
	}

	// 4. Test the API output
	app := &handlers.AppEnv{DB: db}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/attendance?date=2026-09-21", nil)
	rec := httptest.NewRecorder()

	app.AdminDailyAttendanceHandler(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if status := rec.Code; status != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", status)
	}

	var response map[string]interface{}
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	dataList := response["data"].([]interface{})
	if len(dataList) != 1 {
		t.Fatalf("Expected 1 attendance record, got %d", len(dataList))
	}

	record := dataList[0].(map[string]interface{})
	if record["status"] != "Present" {
		t.Errorf("Expected status 'Present', got '%v'", record["status"])
	}
	if record["check_in_time"] != "07:15 AM" {
		t.Errorf("Expected check_in_time '07:15 AM', got '%v'", record["check_in_time"])
	}
	if record["check_out_time"] != "12:30 PM" {
		t.Errorf("Expected check_out_time '12:30 PM', got '%v'", record["check_out_time"])
	}
}
