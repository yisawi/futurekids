package warnings

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type w3Case struct {
	name       string
	punches    []string
	leave      bool
	wantStatus string
	wantIn     string
	wantOut    string
}

// TestW3TimeWindowGaps verifies the RULES.md §5 windows have no boundary gaps
// (audit warning W3): check-in 06:30:00–09:30:59, check-out 11:30:00–13:30:59,
// dead zones ignored, and a check-out-only day reported as Present.
// Each case runs on its own date so get_student_status sees only that case's punches.
func TestW3TimeWindowGaps(t *testing.T) {
	db, _ := setupThrowawayDB(t, "w3")

	seed := []string{
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1, 'W3 Parent', '+9647700000303', 'unused')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES (1, 'W3 Student', 'W3-RFID-1', 1)`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('W3-DEVICE', 'Gate', true)`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed failed: %v\nquery: %s", err, q)
		}
	}

	cases := []w3Case{
		{name: "06:29:59 before check-in window", punches: []string{"06:29:59"}, wantStatus: "Absent"},
		{name: "06:30:00 check-in start", punches: []string{"06:30:00"}, wantStatus: "Present", wantIn: "06:30 AM"},
		{name: "09:30:00 check-in end", punches: []string{"09:30:00"}, wantStatus: "Present", wantIn: "09:30 AM"},
		{name: "09:30:30 former gap", punches: []string{"09:30:30"}, wantStatus: "Present", wantIn: "09:30 AM"},
		{name: "09:30:59 former gap edge", punches: []string{"09:30:59"}, wantStatus: "Present", wantIn: "09:30 AM"},
		{name: "09:30:59.999 last millisecond", punches: []string{"09:30:59.999"}, wantStatus: "Present", wantIn: "09:30 AM"},
		{name: "09:31:00 dead zone start", punches: []string{"09:31:00"}, wantStatus: "Absent"},
		{name: "11:29:59 dead zone end", punches: []string{"11:29:59"}, wantStatus: "Absent"},
		{name: "11:30:00 check-out start", punches: []string{"11:30:00"}, wantStatus: "Present", wantOut: "11:30 AM"},
		{name: "13:30:00 check-out end", punches: []string{"13:30:00"}, wantStatus: "Present", wantOut: "01:30 PM"},
		{name: "13:30:30 former gap", punches: []string{"13:30:30"}, wantStatus: "Present", wantOut: "01:30 PM"},
		{name: "13:30:59 former gap edge", punches: []string{"13:30:59"}, wantStatus: "Present", wantOut: "01:30 PM"},
		{name: "13:30:59.999 last millisecond", punches: []string{"13:30:59.999"}, wantStatus: "Present", wantOut: "01:30 PM"},
		{name: "13:31:00 after check-out window", punches: []string{"13:31:00"}, wantStatus: "Absent"},
		{name: "check-in only", punches: []string{"07:15:00"}, wantStatus: "Present", wantIn: "07:15 AM"},
		{name: "check-out only", punches: []string{"12:00:00"}, wantStatus: "Present", wantOut: "12:00 PM"},
		{name: "check-in and check-out", punches: []string{"07:15:00", "12:05:00"}, wantStatus: "Present", wantIn: "07:15 AM", wantOut: "12:05 PM"},
		{name: "earliest punch wins in both windows", punches: []string{"09:30:40", "09:30:20", "13:30:50", "13:30:10"}, wantStatus: "Present", wantIn: "09:30 AM", wantOut: "01:30 PM"},
		{name: "leave without punches", leave: true, wantStatus: "Excused"},
		{name: "leave with check-out only", punches: []string{"12:00:00"}, leave: true, wantStatus: "Present", wantOut: "12:00 PM"},
		{name: "no punches", wantStatus: "Absent"},
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	dateOf := func(i int) string { return base.AddDate(0, 0, i).Format("2006-01-02") }
	for i, c := range cases {
		for _, p := range c.punches {
			if _, err := db.Exec(`INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (1, 'W3-DEVICE', $1::timestamp)`, dateOf(i)+" "+p); err != nil {
				t.Fatalf("insert punch %s for %q: %v", p, c.name, err)
			}
		}
		if c.leave {
			if _, err := db.Exec(`INSERT INTO student_leaves (student_id, leave_date) VALUES (1, $1::date)`, dateOf(i)); err != nil {
				t.Fatalf("insert leave for %q: %v", c.name, err)
			}
		}
	}

	runCases := func(t *testing.T) {
		for i, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				status, in, out := w3Status(t, db, dateOf(i))
				if status != c.wantStatus || in != c.wantIn || out != c.wantOut {
					t.Errorf("punches %v on %s: got (status=%q, first_check=%q, last_check=%q), want (%q, %q, %q)",
						c.punches, dateOf(i), status, in, out, c.wantStatus, c.wantIn, c.wantOut)
				}
			})
		}
	}

	t.Run("windows after 000017", runCases)

	downSQL := readMigration(t, "000017_close_time_window_gaps.down.sql")
	upSQL := readMigration(t, "000017_close_time_window_gaps.up.sql")

	t.Run("rollback restores 000015 behavior", func(t *testing.T) {
		if _, err := db.Exec(downSQL); err != nil {
			t.Fatalf("apply 000017 down: %v", err)
		}
		oldBehavior := map[string]string{
			"09:30:30 former gap":   "Absent",
			"13:30:30 former gap":   "Absent",
			"check-out only":        "Absent",
			"09:30:00 check-in end": "Present",
		}
		for i, c := range cases {
			want, ok := oldBehavior[c.name]
			if !ok {
				continue
			}
			if status, _, _ := w3Status(t, db, dateOf(i)); status != want {
				t.Errorf("after rollback %q: status = %q, want %q (000015 behavior)", c.name, status, want)
			}
		}
		var signature string
		if err := db.QueryRow(`SELECT pg_get_function_result('get_student_status(int,date)'::regprocedure)`).Scan(&signature); err != nil {
			t.Fatalf("read function signature: %v", err)
		}
		if want := "TABLE(status text, first_check text, last_check text)"; signature != want {
			t.Errorf("after rollback signature = %q, want %q", signature, want)
		}
	})

	t.Run("re-apply 000017 after rollback", func(t *testing.T) {
		if _, err := db.Exec(upSQL); err != nil {
			t.Fatalf("re-apply 000017 up: %v", err)
		}
		runCases(t)
	})

	if t.Failed() {
		t.Log("FAIL: Time window gaps not eliminated (see subtest errors above for the failing boundary)")
	} else {
		t.Log("PASS: Time window gaps eliminated")
	}
}

func w3Status(t *testing.T, db *sql.DB, date string) (status, firstCheck, lastCheck string) {
	t.Helper()
	var in, out sql.NullString
	if err := db.QueryRow(`SELECT status, first_check, last_check FROM get_student_status(1, $1::date)`, date).Scan(&status, &in, &out); err != nil {
		t.Fatalf("get_student_status(1, %s): %v", date, err)
	}
	return status, in.String, out.String
}

func readMigration(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	return string(b)
}
