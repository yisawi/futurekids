package warnings

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"future_kids/internal/auth"
	"future_kids/internal/handlers"
	"future_kids/internal/ratelimit"

	"golang.org/x/crypto/bcrypt"
)

const h2RuleMessage = "parent_pin must be 16 to 72 characters (ASCII, no spaces) with at least one letter, one digit and one symbol"

// TestH2ParentPasswordRule verifies Task H2: a parent_pin set or replaced through the admin API
// must be 16 to 72 printable ASCII characters with at least one letter, one digit and one
// symbol; blank keeps its meaning; parent login accepts any stored credential exactly as before.
func TestH2ParentPasswordRule(t *testing.T) {
	db, _ := setupThrowawayDB(t, "h2")
	auth.InitAuth("h2-test-secret")
	admin, _ := auth.GenerateAdminToken("admin", 0)
	app := &handlers.AppEnv{DB: db, LoginLimiter: ratelimit.NewLoginLimiter(5, time.Minute)}
	students := app.AdminMiddleware(app.AdminStudentsHandler)
	phoneN := 0
	newPhone := func() string { phoneN++; return fmt.Sprintf("+96470000016%02d", phoneN) }
	send := func(t *testing.T, method string, body map[string]any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := serve(t, students, method, "/api/admin/students", admin, string(raw))
		return rec.Code, rec.Body.String()
	}
	create := func(t *testing.T, phone string, pin any) (int, int, string) {
		t.Helper()
		body := map[string]any{"name": "H2 Kid", "parent_name": "H2 Parent", "parent_phone": phone}
		if pin != nil {
			body["parent_pin"] = pin
		}
		raw, _ := json.Marshal(body)
		rec := serve(t, students, "POST", "/api/admin/students", admin, string(raw))
		var resp struct {
			Data struct {
				ID int `json:"id"`
			} `json:"data"`
		}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp.Data.ID, rec.Body.String()
	}
	update := func(t *testing.T, id int, phone string, pin any) (int, string) {
		t.Helper()
		body := map[string]any{"id": id, "name": "H2 Kid", "parent_name": "H2 Parent", "parent_phone": phone}
		if pin != nil {
			body["parent_pin"] = pin
		}
		return send(t, "PUT", body)
	}
	login := func(t *testing.T, phone, pin string) int {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"phone": phone, "pin": pin})
		return serve(t, app.MobileLoginHandler, "POST", "/api/mobile/login", "", string(raw)).Code
	}
	parentState := func(phone string) string {
		var id, version, tokens int
		var hash string
		db.QueryRow(`SELECT id, session_version, pin_code FROM parents WHERE phone_number = $1`, phone).Scan(&id, &version, &hash)
		db.QueryRow(`SELECT COUNT(*) FROM device_tokens WHERE parent_id = $1`, id).Scan(&tokens)
		return fmt.Sprintf("version=%d tokens=%d hash=%s", version, tokens, hash)
	}
	addToken := func(t *testing.T, phone, token string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO device_tokens (parent_id, token) SELECT id, $2 FROM parents WHERE phone_number = $1`, phone, token); err != nil {
			t.Fatal(err)
		}
	}
	const first = "Kq7#vR2m!Tx9pW4z"
	long72 := "Zx8&" + strings.Repeat("Qw4%", 17)

	t.Run("accepted values work on create and update, and log in exactly", func(t *testing.T) {
		for _, pin := range []string{first, long72, `Qa9"r\Tz$w#4%&'Lp`, "Hb8@nP3e%Yw6sJ5d"} {
			if len(pin) < 16 || len(pin) > 72 {
				t.Fatalf("fixture %q has %d characters", pin, len(pin))
			}
			phone := newPhone()
			code, id, body := create(t, phone, pin)
			if code != 200 {
				t.Errorf("create with %q: %d %s", pin, code, body)
				continue
			}
			if c := login(t, phone, pin); c != 200 {
				t.Errorf("login with %q after create: %d", pin, c)
			}
			replacement := "Ze4&uM9k?Ga2fC7x" + pin[:2]
			if code, body := update(t, id, phone, replacement); code != 200 {
				t.Errorf("update to %q: %d %s", replacement, code, body)
			}
			if c := login(t, phone, replacement); c != 200 {
				t.Errorf("login with %q after update: %d", replacement, c)
			}
			if c := login(t, phone, pin); c != 401 {
				t.Errorf("login with the replaced %q: %d, want 401", pin, c)
			}
			if code, body := update(t, id, phone, pin); code != 200 || login(t, phone, pin) != 200 {
				t.Errorf("setting %q on an existing parent: %d %s", pin, code, body)
			}
		}
	})

	t.Run("rejected values are 400 naming parent_pin and write nothing", func(t *testing.T) {
		existingPhone := newPhone()
		code, existingID, body := create(t, existingPhone, first)
		if code != 200 {
			t.Fatalf("existing parent: %d %s", code, body)
		}
		addToken(t, existingPhone, "h2-device-token-0000000001")
		before := parentState(existingPhone)
		for _, tc := range []struct{ name, pin string }{
			{"15 characters", "Kq7#vR2m!Tx9pW4"},
			{"73 characters", long72 + "a"},
			{"6 digits", "482193"},
			{"16 letters", "KqvRmTxpWzabcdef"},
			{"16 digits", "4821937465019283"},
			{"letters and digits, no symbol", "Kq7vR2mTx9pW4zab"},
			{"digits and symbols, no letter", "48#21!93%74&65?0"},
			{"letters and symbols, no digit", "Kq#vR!mT%xpW&zab"},
			{"inner space", "Kq7#vR2m Tx9pW4z"},
			{"leading space", " Kq7#vR2m!Tx9pW4z"},
			{"trailing space", "Kq7#vR2m!Tx9pW4z "},
			{"tab", "Kq7#vR2m\tTx9pW4z"},
			{"Arabic letter", "Kq7#vR2m!Tx9pW4ع"},
			{"Arabic-Indic digit instead of a digit", "Kq#vR!mT%xpW&za٤"},
		} {
			phone := newPhone()
			if code, _, body := create(t, phone, tc.pin); code != 400 || !strings.Contains(body, h2RuleMessage) {
				t.Errorf("create with %s (%q): %d %s, want 400 %q", tc.name, tc.pin, code, body, h2RuleMessage)
			}
			var n int
			db.QueryRow(`SELECT COUNT(*) FROM parents WHERE phone_number = $1`, phone).Scan(&n)
			if n != 0 {
				t.Errorf("create with %s wrote a parent", tc.name)
			}
			if code, body := update(t, existingID, existingPhone, tc.pin); code != 400 || !strings.Contains(body, h2RuleMessage) {
				t.Errorf("update with %s (%q): %d %s, want 400", tc.name, tc.pin, code, body)
			}
		}
		if after := parentState(existingPhone); after != before {
			t.Errorf("rejected updates changed the parent\nbefore: %s\nafter:  %s", before, after)
		}
		if c := login(t, existingPhone, first); c != 200 {
			t.Errorf("the existing credential no longer logs in: %d", c)
		}
	})

	t.Run("blank and replacement semantics are unchanged", func(t *testing.T) {
		phone := newPhone()
		for _, blank := range []any{nil, "", "   "} {
			if code, _, body := create(t, phone, blank); code != 400 || !strings.Contains(body, "parent_pin is required for a new parent") {
				t.Errorf("new parent with blank %q: %d %s", blank, code, body)
			}
		}
		code, id, body := create(t, phone, first)
		if code != 200 {
			t.Fatalf("create: %d %s", code, body)
		}
		addToken(t, phone, "h2-device-token-0000000002")
		before := parentState(phone)
		for _, blank := range []any{nil, "", "   "} {
			if code, body := update(t, id, phone, blank); code != 200 {
				t.Errorf("update with blank %q: %d %s", blank, code, body)
			}
		}
		if after := parentState(phone); after != before {
			t.Errorf("blank updates changed credential, sessions or tokens\nbefore: %s\nafter:  %s", before, after)
		}
		var versionBefore int
		db.QueryRow(`SELECT session_version FROM parents WHERE phone_number = $1`, phone).Scan(&versionBefore)
		if code, body := update(t, id, phone, "Rt5!jX8q-Bn3vL6h"); code != 200 {
			t.Fatalf("replace: %d %s", code, body)
		}
		var versionAfter, tokens int
		db.QueryRow(`SELECT p.session_version, (SELECT COUNT(*) FROM device_tokens d WHERE d.parent_id = p.id) FROM parents p WHERE p.phone_number = $1`, phone).Scan(&versionAfter, &tokens)
		if versionAfter != versionBefore+1 || tokens != 0 {
			t.Errorf("replacing: session_version %d → %d, %d device tokens left; want +1 and 0", versionBefore, versionAfter, tokens)
		}
	})

	t.Run("legacy 4-digit and 6-digit credentials stored earlier still log in", func(t *testing.T) {
		for i, legacy := range []string{"7319", "482193"} {
			phone := fmt.Sprintf("+96470000016%02d", 90+i)
			hash, _ := bcrypt.GenerateFromPassword([]byte(legacy), bcrypt.MinCost)
			if _, err := db.Exec(`INSERT INTO parents (full_name, phone_number, pin_code) VALUES ('H2 Legacy', $1, $2)`, phone, string(hash)); err != nil {
				t.Fatal(err)
			}
			if c := login(t, phone, legacy); c != 200 {
				t.Errorf("legacy %d-digit credential: %d, want 200", len(legacy), c)
			}
			if c := login(t, phone, " "+legacy); c != 401 {
				t.Errorf("login trims the credential: %d, want 401", c)
			}
		}
	})

	t.Run("the login rate limit still applies", func(t *testing.T) {
		phone := newPhone()
		if code, _, body := create(t, phone, first); code != 200 {
			t.Fatalf("create: %d %s", code, body)
		}
		for i := 0; i < 5; i++ {
			if c := login(t, phone, "Wrong#Value9xyzq"); c != http.StatusUnauthorized {
				t.Fatalf("failed attempt %d: %d", i+1, c)
			}
		}
		if c := login(t, phone, first); c != http.StatusTooManyRequests {
			t.Errorf("after 5 failures the right credential got %d, want 429", c)
		}
	})
}
