package warnings

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"future_kids/internal/auth"
	"future_kids/internal/background"
	"future_kids/internal/cron"
	"future_kids/internal/handlers"
)

const p2SettingsPath = "/api/mobile/notification-settings"

func p2Settings(enabled bool) string {
	return fmt.Sprintf(`{"data":{"push_enabled":%v},"status":"success"}`, enabled)
}

func p2Registered(registered bool) string {
	if registered {
		return `{"message":"Device token registered","registered":true,"status":"success"}`
	}
	return `{"message":"` + handlers.PushDisabledTokenMessage + `","registered":false,"status":"success"}`
}

// p2Tokens lists the device tokens of a parent, sorted.
func p2Tokens(t *testing.T, db *sql.DB, phone string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT dt.token FROM device_tokens dt JOIN parents p ON p.id = dt.parent_id WHERE p.phone_number = $1 ORDER BY dt.token`, phone)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// TestP2PushPreference verifies Part B through the real API: the switch defaults to on, turning
// it off removes every device of that parent only, registration stores nothing while it is off
// (and answers registered false), turning it on stores nothing until the app registers again,
// repeats are harmless, and authentication and body validation follow the house rules.
func TestP2PushPreference(t *testing.T) {
	srv, db, admin := l1Server(t, "p2push", true)
	const phone, otherPhone = "+9647000007101", "+9647000007102"
	p2Student(t, srv, admin, "P2 Push Kid", "P2 Push Parent", phone, p2Pin, "7101")
	p2Student(t, srv, admin, "P2 Other Push Kid", "P2 Other Push Parent", otherPhone, p2Pin, "7102")
	tok1, tok2, tok3, other := b1Token("p2-push-1"), b1Token("p2-push-2"), b1Token("p2-push-3"), b1Token("p2-push-other")
	token := p2Login(t, srv, phone, p2Pin, "fcm_token", tok1)
	otherToken := p2Login(t, srv, otherPhone, p2Pin, "fcm_token", other)
	get := func(tok string) a14Response { return a14Do(t, srv, "GET", p2SettingsPath, a14Bearer(tok), nil) }
	put := func(tok string, body any) a14Response {
		return a14Do(t, srv, "PUT", p2SettingsPath, a14Bearer(tok), body)
	}
	register := func(tok, device string) a14Response {
		return a14Do(t, srv, "PUT", "/api/mobile/device-token", a14Bearer(tok), map[string]string{"token": device})
	}
	tokens := func(p string) string { return strings.Join(p2Tokens(t, db, p), ",") }

	p2Expect(t, "default", get(token), 200, p2Settings(true))
	p2Expect(t, "register a second phone", register(token, tok2), 200, p2Registered(true))
	if got := tokens(phone); got != tok1+","+tok2 {
		t.Fatalf("tokens before switching off: %s", got)
	}

	p2Expect(t, "switch off", put(token, map[string]bool{"push_enabled": false}), 200, p2Settings(false))
	if got := tokens(phone); got != "" {
		t.Errorf("tokens after switching off: %s, want none", got)
	}
	if got := tokens(otherPhone); got != other {
		t.Errorf("another parent's tokens: %s", got)
	}
	p2Expect(t, "switch off again", put(token, map[string]bool{"push_enabled": false}), 200, p2Settings(false))
	p2Expect(t, "read while off", get(token), 200, p2Settings(false))
	p2Expect(t, "other parent unaffected", get(otherToken), 200, p2Settings(true))

	p2Expect(t, "register while off", register(token, tok3), 200, p2Registered(false))
	p2Expect(t, "register again while off", register(token, tok3), 200, p2Registered(false))
	if got := tokens(phone); got != "" {
		t.Errorf("tokens after registering while off: %s", got)
	}
	p2Expect(t, "this phone was another parent's", register(token, other), 200, p2Registered(false))
	if got := tokens(otherPhone); got != "" {
		t.Errorf("the phone now signed in as a parent with push off still gets the other parent's pushes: %s", got)
	}
	p2Expect(t, "the other parent registers it back", register(otherToken, other), 200, p2Registered(true))
	if r := a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": phone, "pin": p2Pin, "fcm_token": tok1}); r.status != 200 {
		t.Errorf("login with fcm_token while off: %d %s", r.status, r.body)
	}
	if got := tokens(phone); got != "" {
		t.Errorf("login's fcm_token stored while off: %s", got)
	}

	p2Expect(t, "switch on", put(token, map[string]bool{"push_enabled": true}), 200, p2Settings(true))
	if got := tokens(phone); got != "" {
		t.Errorf("switching on stored tokens: %s", got)
	}
	p2Expect(t, "register after switching on", register(token, tok1), 200, p2Registered(true))
	p2Expect(t, "switch on again", put(token, map[string]bool{"push_enabled": true}), 200, p2Settings(true))
	if got := tokens(phone); got != tok1 {
		t.Errorf("tokens after on, register, on: %s, want %s", got, tok1)
	}
	p2Expect(t, "DELETE device-token unchanged", a14Do(t, srv, "DELETE", "/api/mobile/device-token", a14Bearer(token), map[string]string{"token": tok1}), 200,
		`{"message":"Device token removed","status":"success"}`)
	var stored bool
	db.QueryRow(`SELECT push_enabled FROM parents WHERE phone_number = $1`, phone).Scan(&stored)
	if !stored {
		t.Errorf("push_enabled false after switching on")
	}

	t.Run("body validation", func(t *testing.T) {
		for _, c := range []struct {
			name string
			body any
			want int
			msg  string
		}{
			{"missing", map[string]any{}, 400, handlers.PushEnabledRequiredMessage},
			{"null", map[string]any{"push_enabled": nil}, 400, handlers.PushEnabledRequiredMessage},
			{"other field only", map[string]any{"enabled": false}, 400, handlers.PushEnabledRequiredMessage},
			{"string", map[string]any{"push_enabled": "false"}, 400, "Invalid request body"},
			{"number", map[string]any{"push_enabled": 0}, 400, "Invalid request body"},
			{"malformed", "{", 400, "Invalid request body"},
			{"oversized", `{"push_enabled":false,"pad":"` + strings.Repeat("a", 70<<10) + `"}`, 413, "Request body too large"},
		} {
			p2Expect(t, c.name, put(token, c.body), c.want, p2Error(c.msg))
		}
		p2Expect(t, "still on", get(token), 200, p2Settings(true))
	})

	t.Run("authentication", func(t *testing.T) {
		for _, method := range []string{"GET", "PUT"} {
			body := map[string]bool{"push_enabled": false}
			p2Expect(t, method+" no token", a14Do(t, srv, method, p2SettingsPath, nil, body), 401, p2Error("Unauthorized"))
			p2Expect(t, method+" garbage token", a14Do(t, srv, method, p2SettingsPath, a14Bearer("x.y.z"), body), 401, p2Error("Invalid or expired token"))
			p2Expect(t, method+" admin token", a14Do(t, srv, method, p2SettingsPath, a14Bearer(admin), body), 403, p2Error("Access denied"))
		}
		p2Expect(t, "POST is not a method of the route", a14Do(t, srv, "POST", p2SettingsPath, a14Bearer(token), nil), 405, "")
		p2Expect(t, "unchanged", get(token), 200, p2Settings(true))
	})

	t.Run("a new parent defaults to on and a deletion leaves no orphan", func(t *testing.T) {
		const p3 = "+9647000007103"
		p2Student(t, srv, admin, "P2 New Push Kid", "P2 New Push Parent", p3, p2Pin, "7103")
		tok := p2Login(t, srv, p3, p2Pin, "fcm_token", b1Token("p2-push-new"))
		p2Expect(t, "new parent", get(tok), 200, p2Settings(true))
		p2Expect(t, "off", put(tok, map[string]bool{"push_enabled": false}), 200, p2Settings(false))
		k1Exec(t, db, `INSERT INTO notifications (parent_id, title, body) SELECT id, 'T', 'B' FROM parents WHERE phone_number = '`+p3+`'`)
		var id int
		db.QueryRow(`SELECT id FROM parents WHERE phone_number = $1`, p3).Scan(&id)
		p2Expect(t, "delete", p2Delete(t, srv, tok, map[string]string{"pin": p2Pin}), 200, "")
		for _, q := range []string{`SELECT COUNT(*) FROM parents WHERE id = $1`, `SELECT COUNT(*) FROM device_tokens WHERE parent_id = $1`,
			`SELECT COUNT(*) FROM notifications WHERE parent_id = $1`, `SELECT COUNT(*) FROM students WHERE parent_id = $1`} {
			if n := k1Count(t, db, q, id); n != 0 {
				t.Errorf("%s = %d after deletion", q, n)
			}
		}
		p2Expect(t, "settings after deletion", get(tok), 401, p2Error("Invalid or expired token"))
	})

	if strings.Contains(srv.out.String(), `"level":"ERROR"`) {
		t.Errorf("server logged an ERROR:\n%s", srv.out)
	}
}

// p2App is an in-process app with a fake FCM: parent 7201 (push turned off through the API, with
// children 7201 and 7203) and parent 7202 (push on, child 7202), with a device each.
type p2App struct {
	app   *handlers.AppEnv
	db    *sql.DB
	fcm   *b1FakeFCM
	bg    *background.Group
	admin string
	off   string
	on    string
}

func newP2App(t *testing.T, label string) *p2App {
	t.Helper()
	db, _ := setupThrowawayDB(t, label)
	k1Exec(t, db,
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (7201, 'P2 Off', '+9647000007201', 'x'), (7202, 'P2 On', '+9647000007202', 'x')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id, grade, section, is_active, created_at) VALUES
			(7201, 'P2 Off Kid', '7201', 7201, 'G1', 'A', true, '2026-01-01 08:00'),
			(7202, 'P2 On Kid', '7202', 7202, 'G1', 'A', true, '2026-01-01 08:00'),
			(7203, 'P2 Off Absent Kid', '7203', 7201, 'G1', 'A', true, '2026-01-01 08:00')`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('P2-DEV', 'Gate', true)`,
		fmt.Sprintf(`INSERT INTO device_tokens (parent_id, token) VALUES (7201, '%s'), (7201, '%s'), (7202, '%s')`, b1Token("p2-off-1"), b1Token("p2-off-2"), b1Token("p2-on")))
	auth.InitAuth("p2-test-secret")
	a := &p2App{db: db, fcm: newB1FakeFCM(t), bg: &background.Group{}}
	a.app = &handlers.AppEnv{DB: db, FCMClient: a.fcm.client(t), Background: a.bg}
	a.admin, _ = auth.GenerateAdminToken("admin", 0)
	a.off, _ = auth.GenerateParentToken(7201, "+9647000007201", 0)
	a.on, _ = auth.GenerateParentToken(7202, "+9647000007202", 0)
	if rec := serve(t, a.app.AuthMiddleware(a.app.NotificationSettingsHandler), http.MethodPut, p2SettingsPath, a.off, `{"push_enabled":false}`); rec.Code != 200 {
		t.Fatalf("switch off: %d %s", rec.Code, rec.Body)
	}
	return a
}

func (a *p2App) settle(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if pending, err := a.bg.Wait(ctx); err != nil {
		t.Fatalf("background work still running: %d", pending)
	}
}

// inbox returns parent's notification titles in order.
func (a *p2App) inbox(t *testing.T, parent int) []string {
	t.Helper()
	rows, err := a.db.Query(`SELECT title FROM notifications WHERE parent_id = $1 ORDER BY id`, parent)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// TestP2PushOffKeepsInbox verifies that a parent with push turned off still gets every
// notification row (punch, broadcasts, absence job) while no push is attempted for that parent,
// and that a parent with push on still gets pushes.
func TestP2PushOffKeepsInbox(t *testing.T) {
	capture := &w10Capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })
	a := newP2App(t, "p2inbox")
	if n := k1Count(t, a.db, `SELECT COUNT(*) FROM device_tokens WHERE parent_id = 7201`); n != 0 {
		t.Fatalf("parent with push off still has %d tokens", n)
	}

	pinToday(t, k1Today+" 07:50")
	for _, tag := range []string{"7201", "7202"} {
		rec := httptest.NewRecorder()
		a.app.ADMSHandler(rec, httptest.NewRequest("POST", "/iclock/cdata?SN=P2-DEV&table=ATTLOG", strings.NewReader(tag+"\t"+k1Today+" 07:50:00\t1\t1\n")))
		if rec.Code != 200 || rec.Body.String() != "OK" {
			t.Fatalf("punch %s: %d %s", tag, rec.Code, rec.Body)
		}
	}
	a.settle(t)
	if sent := a.fcm.take(); strings.Join(sent, ",") != b1Token("p2-on") {
		t.Errorf("punch pushes went to %v, want only the parent with push on", sent)
	}

	for i, audience := range []map[string]any{j1All(), j1Parent("07000007201")} {
		raw := fmt.Sprintf(`{"title":"P2 broadcast %d","body":"Body %d","audience":%s}`, i, i, mustJSON(t, audience))
		if rec := serve(t, a.app.AdminMiddleware(a.app.AdminBroadcastsHandler), http.MethodPost, "/api/admin/broadcasts", a.admin, raw); rec.Code != 200 {
			t.Fatalf("broadcast %d: %d %s", i, rec.Code, rec.Body)
		}
		a.settle(t)
	}
	sent := a.fcm.take()
	if strings.Join(sent, ",") != b1Token("p2-on") {
		t.Errorf("broadcast pushes went to %v, want only the 'all' broadcast to the parent with push on", sent)
	}

	pinToday(t, k1Today+" 12:00")
	cron.ProcessDailyAbsences(a.db, a.app.FCMClient, a.bg)
	a.settle(t)
	if sent := a.fcm.take(); len(sent) != 0 {
		t.Errorf("absence pushes: %v, want none (the absent child's parent has push off)", sent)
	}

	want := []string{"إشعار دخول", "P2 broadcast 0", "P2 broadcast 1", "إشعار غياب"}
	if got := a.inbox(t, 7201); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("inbox of the parent with push off: %v, want %v", got, want)
	}
	if got := a.inbox(t, 7202); strings.Join(got, "|") != "إشعار دخول|P2 broadcast 0" {
		t.Errorf("inbox of the parent with push on: %v", got)
	}
	for _, l := range capture.snapshot() {
		if l.Level >= slog.LevelError {
			t.Errorf("ERROR logged: %s %v", l.Msg, l.Attrs)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestP2PushConcurrency races switching push off against registering a phone: whatever the
// order, a parent with push off never keeps a token.
func TestP2PushConcurrency(t *testing.T) {
	a := newP2App(t, "p2pushrace")
	settings := a.app.AuthMiddleware(a.app.NotificationSettingsHandler)
	register := a.app.AuthMiddleware(a.app.RegisterDeviceTokenHandler)
	call := func(h http.HandlerFunc, method, path, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+a.off)
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}
	for round := 0; round < 50; round++ {
		k1Exec(t, a.db, `UPDATE parents SET push_enabled = true WHERE id = 7201`)
		device := b1Token(fmt.Sprintf("p2-race-%02d", round))
		codes := make([]int, 2)
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			codes[0] = call(settings, "PUT", p2SettingsPath, `{"push_enabled":false}`)
		}()
		go func() {
			defer wg.Done()
			<-start
			codes[1] = call(register, "PUT", "/api/mobile/device-token", `{"token":"`+device+`"}`)
		}()
		close(start)
		wg.Wait()
		if codes[0] != 200 || codes[1] != 200 {
			t.Fatalf("round %d: statuses %v", round, codes)
		}
		var enabled bool
		a.db.QueryRow(`SELECT push_enabled FROM parents WHERE id = 7201`).Scan(&enabled)
		n := k1Count(t, a.db, `SELECT COUNT(*) FROM device_tokens WHERE parent_id = 7201`)
		if enabled || n != 0 {
			t.Errorf("round %d: push_enabled %v with %d tokens, want off with none", round, enabled, n)
		}
		if k1Count(t, a.db, `SELECT COUNT(*) FROM device_tokens WHERE token = $1`, device) != 0 {
			t.Errorf("round %d: the raced token survived", round)
		}
	}
	if k1Count(t, a.db, `SELECT COUNT(*) FROM device_tokens WHERE parent_id = 7202`) != 1 {
		t.Errorf("another parent's token was touched")
	}
}

// TestP2PushMigration verifies 000033: existing parents get push_enabled true, the column is
// NOT NULL with default true, and the migration goes down and up again cleanly.
func TestP2PushMigration(t *testing.T) {
	db, _ := setupThrowawayDB(t, "p2migr")
	up, down := a14Migration(t, "000033_add_parent_push_enabled.up.sql"), a14Migration(t, "000033_add_parent_push_enabled.down.sql")
	k1Exec(t, db, down, `INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (7301, 'P2 Old', '+9647000007301', 'x')`, up)
	var enabled bool
	var nullable, def string
	db.QueryRow(`SELECT push_enabled FROM parents WHERE id = 7301`).Scan(&enabled)
	db.QueryRow(`SELECT is_nullable, column_default FROM information_schema.columns WHERE table_name = 'parents' AND column_name = 'push_enabled'`).Scan(&nullable, &def)
	if !enabled || nullable != "NO" || def != "true" {
		t.Errorf("after up: existing parent %v, nullable %s, default %s; want true, NO, true", enabled, nullable, def)
	}
	k1Exec(t, db, up, down)
	if n := k1Count(t, db, `SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'parents' AND column_name = 'push_enabled'`); n != 0 {
		t.Errorf("column still there after down")
	}
	k1Exec(t, db, up)
	if n := k1Count(t, db, `SELECT COUNT(*) FROM parents WHERE push_enabled`); n != 1 {
		t.Errorf("after down and up again: %d parents with push on, want 1", n)
	}
}
