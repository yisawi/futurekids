package warnings

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"future_kids/internal/auth"
	"future_kids/internal/handlers"
)

const w4ParentPhone = "+9647700000304"

// TestW4OpenAPICompliance verifies the JSON contract fixes for audit warning W4:
// nullable time fields are always present (null when empty), students expose
// grade/section, and a PUT that omits rfid_tag/grade/section preserves them.
func TestW4OpenAPICompliance(t *testing.T) {
	db, _ := setupThrowawayDB(t, "fk_w4_test")
	baghdad, err := time.LoadLocation("Asia/Baghdad")
	if err != nil {
		t.Fatalf("load Asia/Baghdad: %v", err)
	}
	now := time.Now().In(baghdad)
	today := now.Format("2006-01-02")

	seed := []string{
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1, 'W4 Parent', '` + w4ParentPhone + `', 'unused')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id, grade, section) VALUES (1, 'W4 Present', 'W4-RFID-1', 1, 'G1', 'A')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES (2, 'W4 Absent', 'W4-RFID-2', 1)`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('W4-DEVICE', 'Gate', true)`,
		`INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (1, 'W4-DEVICE', '` + today + ` 07:15:00')`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed failed: %v\nquery: %s", err, q)
		}
	}

	auth.InitAuth("w4-test-secret")
	app := &handlers.AppEnv{DB: db}
	parentToken, err := auth.GenerateParentToken(1, w4ParentPhone)
	if err != nil {
		t.Fatalf("GenerateParentToken: %v", err)
	}
	adminToken, err := auth.GenerateAdminToken("admin")
	if err != nil {
		t.Fatalf("GenerateAdminToken: %v", err)
	}

	checkDaily := func(t *testing.T, data []map[string]any) {
		t.Helper()
		byName := indexBy(data, "full_name")
		assertField(t, byName["W4 Present"], "check_in_time", "07:15 AM")
		assertField(t, byName["W4 Present"], "check_out_time", nil)
		assertField(t, byName["W4 Absent"], "check_in_time", nil)
		assertField(t, byName["W4 Absent"], "check_out_time", nil)
	}

	t.Run("DailyAttendanceDTO keys always present/admin", func(t *testing.T) {
		rec := serve(t, handlers.AdminMiddleware(app.AdminDailyAttendanceHandler), http.MethodGet, "/api/admin/attendance?date="+today, adminToken, "")
		checkDaily(t, decodeData(t, rec, http.StatusOK))
	})

	t.Run("DailyAttendanceDTO keys always present/mobile", func(t *testing.T) {
		rec := serve(t, handlers.AuthMiddleware(app.MobileTodayAttendanceHandler), http.MethodGet, "/api/mobile/attendance/today", parentToken, "")
		checkDaily(t, decodeData(t, rec, http.StatusOK))
	})

	t.Run("MonthlyRecord check_time always present", func(t *testing.T) {
		rec := serve(t, handlers.AuthMiddleware(app.MobileMonthlyAttendanceHandler), http.MethodGet, "/api/mobile/attendance/monthly", parentToken, "")
		data := decodeData(t, rec, http.StatusOK)
		schoolDay := now.Weekday() != time.Friday && now.Weekday() != time.Saturday
		checked := 0
		for _, report := range data {
			records, _ := report["records"].([]any)
			for _, r := range records {
				record := r.(map[string]any)
				want := any(nil)
				if report["full_name"] == "W4 Present" && record["date"] == today {
					want = "07:15 AM"
				}
				assertField(t, record, "check_time", want)
				checked++
			}
		}
		if checked == 0 && (schoolDay || now.Day() > 1) {
			t.Fatalf("expected monthly records, got none")
		}
	})

	getStudent := func(t *testing.T, id int) map[string]any {
		t.Helper()
		rec := serve(t, handlers.AdminMiddleware(app.AdminStudentsHandler), http.MethodGet, "/api/admin/students", adminToken, "")
		for _, s := range decodeData(t, rec, http.StatusOK) {
			if s["id"] == float64(id) {
				return s
			}
		}
		t.Fatalf("student %d missing from GET /api/admin/students", id)
		return nil
	}
	put := func(t *testing.T, body map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		rec := serve(t, handlers.AdminMiddleware(app.AdminStudentsHandler), http.MethodPut, "/api/admin/students", adminToken, string(b))
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %s: HTTP %d %s", b, rec.Code, rec.Body.String())
		}
	}

	t.Run("GET students returns grade and section", func(t *testing.T) {
		assertField(t, getStudent(t, 1), "grade", "G1")
		assertField(t, getStudent(t, 1), "section", "A")
		assertField(t, getStudent(t, 2), "grade", nil)
		assertField(t, getStudent(t, 2), "section", nil)
	})

	t.Run("GET then PUT round trip preserves student fields", func(t *testing.T) {
		put(t, getStudent(t, 1))
		s := getStudent(t, 1)
		assertField(t, s, "grade", "G1")
		assertField(t, s, "section", "A")
		assertField(t, s, "rfid_tag", "W4-RFID-1")
	})

	t.Run("PUT without rfid_tag, grade, section keeps them", func(t *testing.T) {
		put(t, map[string]any{"id": 1, "name": "W4 Present Renamed", "parent_name": "W4 Parent", "parent_phone": w4ParentPhone})
		s := getStudent(t, 1)
		assertField(t, s, "name", "W4 Present Renamed")
		assertField(t, s, "rfid_tag", "W4-RFID-1")
		assertField(t, s, "grade", "G1")
		assertField(t, s, "section", "A")

		before := punchCount(t, db, 1)
		body := "W4-RFID-1\t" + today + " 16:00:00\t1\t1\n"
		rec := serve(t, app.ADMSHandler, http.MethodPost, "/iclock/cdata?SN=W4-DEVICE&table=ATTLOG", "", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("ADMS push: HTTP %d", rec.Code)
		}
		if after := punchCount(t, db, 1); after != before+1 {
			t.Errorf("device PIN W4-RFID-1 no longer maps to the student after PUT (punches %d -> %d)", before, after)
		}
	})

	t.Run("PUT with explicit rfid_tag, grade, section updates them", func(t *testing.T) {
		put(t, map[string]any{"id": 1, "name": "W4 Present", "parent_name": "W4 Parent", "parent_phone": w4ParentPhone,
			"rfid_tag": "W4-RFID-9", "grade": "G2", "section": "B"})
		s := getStudent(t, 1)
		assertField(t, s, "rfid_tag", "W4-RFID-9")
		assertField(t, s, "grade", "G2")
		assertField(t, s, "section", "B")
	})

	if t.Failed() {
		t.Log("FAIL: OpenAPI contract not met (see subtest errors above)")
	} else {
		t.Log("PASS: OpenAPI contract verified")
	}
}

// assertField fails unless obj has key present with exactly want (nil means JSON null).
func assertField(t *testing.T, obj map[string]any, key string, want any) {
	t.Helper()
	if obj == nil {
		t.Errorf("%s: object missing", key)
		return
	}
	got, ok := obj[key]
	if !ok {
		t.Errorf("%s: key missing from JSON (want %#v)", key, want)
		return
	}
	if got != want {
		t.Errorf("%s: got %#v, want %#v", key, got, want)
	}
}

func indexBy(data []map[string]any, key string) map[string]map[string]any {
	out := make(map[string]map[string]any, len(data))
	for _, d := range data {
		if k, ok := d[key].(string); ok {
			out[k] = d
		}
	}
	return out
}

func punchCount(t *testing.T, db *sql.DB, studentID int) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM attendance_logs WHERE student_id = $1", studentID).Scan(&n); err != nil {
		t.Fatalf("count punches: %v", err)
	}
	return n
}
