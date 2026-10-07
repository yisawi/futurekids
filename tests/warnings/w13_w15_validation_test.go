package warnings

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"future_kids/internal/auth"
	"future_kids/internal/handlers"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestW13W15ValidationAndPIN verifies audit warnings W13 and W15:
// student create/update rejects missing name, parent name or parent phone, and
// parents.pin_code has no plaintext default, so a parent inserted without a PIN
// fails loudly instead of receiving an unusable '1234'.
func TestW13W15ValidationAndPIN(t *testing.T) {
	db, _ := setupThrowawayDB(t, "w13")
	auth.InitAuth("w13-test-secret")
	app := &handlers.AppEnv{DB: db}
	adminToken, err := auth.GenerateAdminToken("admin", 0)
	if err != nil {
		t.Fatalf("GenerateAdminToken: %v", err)
	}

	call := func(t *testing.T, method string, body map[string]any) (int, string) {
		t.Helper()
		b, _ := json.Marshal(body)
		rec := serve(t, app.AdminMiddleware(app.AdminStudentsHandler), method, "/api/admin/students", adminToken, string(b))
		var resp struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp.Message
	}

	invalid := []struct {
		name string
		body map[string]any
		msg  string
	}{
		{"empty parent_phone", map[string]any{"name": "Kid", "parent_name": "Parent", "parent_phone": "", "rfid_tag": "W13-X1"}, "parent_phone is required"},
		{"whitespace parent_phone", map[string]any{"name": "Kid", "parent_name": "Parent", "parent_phone": "   ", "rfid_tag": "W13-X2"}, "parent_phone is required"},
		{"missing parent_phone", map[string]any{"name": "Kid", "parent_name": "Parent", "rfid_tag": "W13-X3"}, "parent_phone is required"},
		{"empty parent_name", map[string]any{"name": "Kid", "parent_name": "", "parent_phone": "+9647000000399", "rfid_tag": "W13-X4"}, "parent_name is required"},
		{"whitespace parent_name", map[string]any{"name": "Kid", "parent_name": " \t", "parent_phone": "+9647000000399", "rfid_tag": "W13-X5"}, "parent_name is required"},
		{"empty name", map[string]any{"name": "", "parent_name": "Parent", "parent_phone": "+9647000000399", "rfid_tag": "W13-X6"}, "name is required"},
		{"whitespace name", map[string]any{"name": "  ", "parent_name": "Parent", "parent_phone": "+9647000000399", "rfid_tag": "W13-X7"}, "name is required"},
	}

	t.Run("W13 validation/POST rejects missing fields", func(t *testing.T) {
		for _, c := range invalid {
			if code, msg := call(t, http.MethodPost, c.body); code != http.StatusBadRequest || msg != c.msg {
				t.Errorf("%s: got HTTP %d %q, want 400 %q", c.name, code, msg, c.msg)
			}
		}
		if n := countRows(t, db, `SELECT COUNT(*) FROM students`); n != 0 {
			t.Errorf("rejected requests must not create students, found %d", n)
		}
		if n := countRows(t, db, `SELECT COUNT(*) FROM parents`); n != 0 {
			t.Errorf("rejected requests must not create parents, found %d", n)
		}
	})

	var studentID int
	t.Run("W13 validation/POST valid student stores trimmed phone", func(t *testing.T) {
		code, msg := call(t, http.MethodPost, map[string]any{
			"name": " W13 Kid ", "parent_name": " W13 Parent ", "parent_phone": "  +9647000000305  ", "parent_pin": "432187", "rfid_tag": "W13-RFID-1",
		})
		if code != http.StatusOK {
			t.Fatalf("valid POST: got HTTP %d %q, want 200", code, msg)
		}
		var phone, parentName, kidName string
		err := db.QueryRow(`SELECT s.id, s.full_name, p.full_name, p.phone_number FROM students s JOIN parents p ON p.id = s.parent_id WHERE s.rfid_tag = 'W13-RFID-1'`).
			Scan(&studentID, &kidName, &parentName, &phone)
		if err != nil {
			t.Fatalf("valid student not stored with a parent: %v", err)
		}
		if phone != "+9647000000305" || parentName != "W13 Parent" || kidName != "W13 Kid" {
			t.Errorf("stored (name=%q, parent=%q, phone=%q), want trimmed (W13 Kid, W13 Parent, +9647000000305)", kidName, parentName, phone)
		}
		if n := countRows(t, db, `SELECT COUNT(*) FROM parents WHERE TRIM(phone_number) = ''`); n != 0 {
			t.Errorf("found %d parent rows with an empty phone", n)
		}
	})

	t.Run("W13 validation/PUT rejects missing fields", func(t *testing.T) {
		if studentID == 0 {
			t.Skip("valid student was not created")
		}
		for _, c := range invalid {
			body := map[string]any{"id": studentID}
			for k, v := range c.body {
				if k != "rfid_tag" {
					body[k] = v
				}
			}
			if code, msg := call(t, http.MethodPut, body); code != http.StatusBadRequest || msg != c.msg {
				t.Errorf("%s: got HTTP %d %q, want 400 %q", c.name, code, msg, c.msg)
			}
		}
		var phone string
		if err := db.QueryRow(`SELECT p.phone_number FROM students s JOIN parents p ON p.id = s.parent_id WHERE s.id = $1`, studentID).Scan(&phone); err != nil {
			t.Fatalf("read student parent: %v", err)
		}
		if phone != "+9647000000305" {
			t.Errorf("rejected PUTs must not relink the student, parent phone is now %q", phone)
		}
	})

	const notNull = "23502"
	t.Run("W15 pin_code/insert without PIN fails", func(t *testing.T) {
		_, err := db.Exec(`INSERT INTO parents (full_name, phone_number) VALUES ('No PIN', '+9647000000306')`)
		if code := pgCode(err); code != notNull {
			var pin string
			_ = db.QueryRow(`SELECT pin_code FROM parents WHERE phone_number = '+9647000000306'`).Scan(&pin)
			t.Errorf("insert without pin_code: got err=%v (code %q, stored pin %q), want not_null_violation", err, code, pin)
		}
	})

	t.Run("W15 pin_code/explicit NULL insert fails", func(t *testing.T) {
		_, err := db.Exec(`INSERT INTO parents (full_name, phone_number, pin_code) VALUES ('Null PIN', '+9647000000307', NULL)`)
		if code := pgCode(err); code != notNull {
			t.Errorf("insert NULL pin_code: got err=%v (code %q), want not_null_violation", err, code)
		}
	})

	t.Run("W15 pin_code/update to NULL fails", func(t *testing.T) {
		var id int
		if err := db.QueryRow(`INSERT INTO parents (full_name, phone_number, pin_code) VALUES ('Has PIN', '+9647000000310', 'hash') RETURNING id`).Scan(&id); err != nil {
			t.Fatalf("insert parent: %v", err)
		}
		_, err := db.Exec(`UPDATE parents SET pin_code = NULL WHERE id = $1`, id)
		if code := pgCode(err); code != notNull {
			t.Errorf("update pin_code to NULL: got err=%v (code %q), want not_null_violation", err, code)
		}
	})

	t.Run("W15 pin_code/column is NOT NULL with no default", func(t *testing.T) {
		nullable, def := pinColumn(t, db)
		if nullable != "NO" || def.Valid {
			t.Errorf("pin_code: is_nullable=%s default=%q, want NO and no default", nullable, def.String)
		}
	})

	t.Run("W15 rollback/000018 down restores 000009 default", func(t *testing.T) {
		if _, err := db.Exec(readMigration(t, "000018_enforce_pin_not_null.down.sql")); err != nil {
			t.Fatalf("apply 000018 down: %v", err)
		}
		if nullable, def := pinColumn(t, db); nullable != "NO" || def.String != "'1234'::character varying" {
			t.Errorf("after rollback: is_nullable=%s default=%q, want NO and '1234' (000009 state)", nullable, def.String)
		}
		var pin string
		if err := db.QueryRow(`INSERT INTO parents (full_name, phone_number) VALUES ('Rollback', '+9647000000308') RETURNING pin_code`).Scan(&pin); err != nil || pin != "1234" {
			t.Errorf("after rollback, insert without PIN should get plaintext '1234' (the W15 hazard), got pin=%q err=%v", pin, err)
		}

		if _, err := db.Exec(readMigration(t, "000018_enforce_pin_not_null.up.sql")); err != nil {
			t.Fatalf("re-apply 000018 up: %v", err)
		}
		_, err := db.Exec(`INSERT INTO parents (full_name, phone_number) VALUES ('Again', '+9647000000309')`)
		if code := pgCode(err); code != notNull {
			t.Errorf("after re-applying 000018, insert without PIN: got err=%v, want not_null_violation", err)
		}
	})

	if t.Failed() {
		t.Log("FAIL: Validation and PIN constraints not enforced (see subtest errors above)")
	} else {
		t.Log("PASS: Validation and PIN constraints enforced")
	}
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func pinColumn(t *testing.T, db *sql.DB) (nullable string, def sql.NullString) {
	t.Helper()
	err := db.QueryRow(`SELECT is_nullable, column_default FROM information_schema.columns WHERE table_name = 'parents' AND column_name = 'pin_code'`).Scan(&nullable, &def)
	if err != nil {
		t.Fatalf("read pin_code column: %v", err)
	}
	return nullable, def
}

func countRows(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}
