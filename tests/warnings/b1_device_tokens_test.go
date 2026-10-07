package warnings

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"future_kids/internal/background"
	"future_kids/internal/cron"
	"future_kids/internal/handlers"
	"future_kids/internal/notify"
	"future_kids/internal/testdb"
	"future_kids/internal/tz"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/api/option"
)

func b1Token(name string) string { return "test-device-" + name + "-000000000000" }

// b1FakeFCM records every token FCM is asked to send to and answers by prefix:
// "test-device-dead-*" → UNREGISTERED, "test-device-bad-*" → INVALID_ARGUMENT, else success.
type b1FakeFCM struct {
	mu   sync.Mutex
	sent []string
	url  string
}

func newB1FakeFCM(t *testing.T) *b1FakeFCM {
	t.Helper()
	f := &b1FakeFCM{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Message struct {
				Token string `json:"token"`
			} `json:"message"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		f.mu.Lock()
		f.sent = append(f.sent, req.Message.Token)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(req.Message.Token, "test-device-dead-"):
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":404,"message":"Requested entity was not found.","status":"NOT_FOUND","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`)
		case strings.HasPrefix(req.Message.Token, "test-device-bad-"):
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"code":400,"message":"The registration token is not a valid FCM registration token","status":"INVALID_ARGUMENT"}}`)
		default:
			io.WriteString(w, `{"name":"projects/fk-test/messages/1"}`)
		}
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *b1FakeFCM) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.sent
	f.sent = nil
	sort.Strings(out)
	return out
}

func (f *b1FakeFCM) client(t *testing.T) *messaging.Client {
	t.Helper()
	app, err := firebase.NewApp(context.Background(), &firebase.Config{ProjectID: "fk-test"}, option.WithEndpoint(f.url), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	c, err := app.Messaging(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestB1DeviceTokenEndpoints verifies Task B1 through the real API: a parent registers,
// refreshes and removes device tokens with PUT and DELETE /api/mobile/device-token; a token
// belongs to one parent at a time; each parent keeps at most 10 (least recently seen evicted);
// the routes sit behind the parent middleware; invalid tokens get 400; a PIN change and
// deleting the parent remove the parent's tokens; login's fcm_token uses the same path.
func TestB1DeviceTokenEndpoints(t *testing.T) {
	srv, db := a14Server(t, "b1api")
	admin := a14AdminToken(t, srv)

	type parent struct {
		id      int
		student any
		phone   string
		pin     string
		token   string
	}
	newParent := func(name, phone, pin, rfid string) *parent {
		t.Helper()
		r := a14Do(t, srv, "POST", "/api/admin/students", a14Bearer(admin), map[string]any{
			"name": name + " Child", "parent_name": name, "parent_phone": phone, "parent_pin": pin, "rfid_tag": rfid,
		})
		if r.status != 200 {
			t.Fatalf("create %s: %d %s", name, r.status, r.body)
		}
		p := &parent{student: r.json(t)["data"].(map[string]any)["id"], phone: phone, pin: pin}
		db.QueryRow(`SELECT id FROM parents WHERE phone_number = $1`, phone).Scan(&p.id)
		return p
	}
	login := func(p *parent, extra map[string]string) (int, string) {
		body := map[string]string{"phone": p.phone, "pin": p.pin}
		for k, v := range extra {
			body[k] = v
		}
		r := a14Do(t, srv, "POST", "/api/mobile/login", nil, body)
		if r.status != 200 {
			return r.status, ""
		}
		return 200, fmt.Sprint(r.json(t)["data"].(map[string]any)["token"])
	}
	register := func(bearer, token string) a14Response {
		return a14Do(t, srv, "PUT", "/api/mobile/device-token", a14Bearer(bearer), map[string]string{"token": token})
	}
	remove := func(bearer, token string) a14Response {
		return a14Do(t, srv, "DELETE", "/api/mobile/device-token", a14Bearer(bearer), map[string]string{"token": token})
	}
	tokensOf := func(parentID int) []string {
		rows, err := db.Query(`SELECT token FROM device_tokens WHERE parent_id = $1 ORDER BY token`, parentID)
		if err != nil {
			t.Fatalf("read device_tokens: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var tok string
			rows.Scan(&tok)
			out = append(out, tok)
		}
		return out
	}
	ok := func(t *testing.T, r a14Response, what string) {
		t.Helper()
		if r.status != 200 || r.json(t)["status"] != "success" {
			t.Fatalf("%s: %d %s, want 200 success", what, r.status, r.body)
		}
	}
	expect := func(t *testing.T, what string, got []string, want ...string) {
		t.Helper()
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: %v, want %v", what, got, want)
		}
	}

	p1 := newParent("B1 Parent One", "+9647000000811", "482193", "B1-1")
	p2 := newParent("B1 Parent Two", "+9647000000812", "593047", "B1-2")
	_, p1.token = login(p1, nil)
	_, p2.token = login(p2, nil)
	a, b := b1Token("phone-a"), b1Token("phone-b")

	t.Run("registering is idempotent", func(t *testing.T) {
		ok(t, register(p1.token, a), "first register")
		ok(t, register(p1.token, a), "second register")
		expect(t, "parent 1 tokens", tokensOf(p1.id), a)
	})

	t.Run("a refreshed token is added without duplicates", func(t *testing.T) {
		ok(t, register(p1.token, b), "register refreshed token")
		ok(t, register(p1.token, a), "re-register old token")
		expect(t, "parent 1 tokens", tokensOf(p1.id), a, b)
	})

	t.Run("a token registered by another parent moves to that parent", func(t *testing.T) {
		ok(t, register(p2.token, a), "parent 2 registers parent 1's token")
		expect(t, "parent 1 tokens", tokensOf(p1.id), b)
		expect(t, "parent 2 tokens", tokensOf(p2.id), a)
	})

	t.Run("removal is idempotent and only removes the caller's own token", func(t *testing.T) {
		ok(t, remove(p1.token, a), "parent 1 removes parent 2's token")
		expect(t, "parent 2 tokens after parent 1's removal attempt", tokensOf(p2.id), a)
		ok(t, remove(p1.token, b), "parent 1 removes its token")
		ok(t, remove(p1.token, b), "parent 1 removes it again")
		expect(t, "parent 1 tokens", tokensOf(p1.id))
	})

	t.Run("at most 10 tokens per parent; the least recently seen is evicted", func(t *testing.T) {
		var all []string
		for i := 1; i <= 12; i++ {
			tok := b1Token(fmt.Sprintf("cap-%02d", i))
			all = append(all, tok)
			ok(t, register(p1.token, tok), "register "+tok)
		}
		expect(t, "after 12 registrations", tokensOf(p1.id), all[2:]...)
		ok(t, register(p1.token, all[2]), "refresh the oldest kept token")
		extra := b1Token("cap-13")
		ok(t, register(p1.token, extra), "register a 13th")
		expect(t, "after refreshing cap-03 and adding cap-13", tokensOf(p1.id), append([]string{all[2], extra}, all[4:]...)...)
	})

	t.Run("invalid tokens get 400", func(t *testing.T) {
		for name, body := range map[string]string{
			"empty object":   `{}`,
			"empty token":    `{"token":""}`,
			"too short":      `{"token":"short-token"}`,
			"space":          `{"token":"test device with spaces 0000"}`,
			"quote":          `{"token":"test-device-\"quoted\"-000000"}`,
			"non-ASCII":      `{"token":"test-device-جهاز-00000000000000"}`,
			"too long":       `{"token":"` + strings.Repeat("a", 1025) + `"}`,
			"number":         `{"token":12345678901234567890123}`,
			"malformed JSON": `{"token":`,
		} {
			for _, method := range []string{"PUT", "DELETE"} {
				r := a14Do(t, srv, method, "/api/mobile/device-token", a14Bearer(p1.token), body)
				if r.status != 400 || r.json(t)["status"] != "error" {
					t.Errorf("%s %s: %d %s, want 400 with the error envelope", method, name, r.status, r.body)
				}
			}
		}
		r := a14Do(t, srv, "PUT", "/api/mobile/device-token", a14Bearer(p1.token), `{"token":"`+strings.Repeat("a", 70<<10)+`"}`)
		if r.status != 413 {
			t.Errorf("oversized body: %d, want 413", r.status)
		}
	})

	t.Run("the routes need a valid parent token", func(t *testing.T) {
		expired, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"parent_id": p1.id, "role": "parent", "sv": 0, "exp": time.Now().Add(-time.Hour).Unix(), "iat": time.Now().Add(-2 * time.Hour).Unix(),
		}).SignedString([]byte("g3"))
		for _, method := range []string{"PUT", "DELETE"} {
			for _, c := range []struct {
				name   string
				header map[string]string
				want   int
			}{
				{"missing", nil, 401},
				{"malformed", a14Bearer("not-a-jwt"), 401},
				{"expired", a14Bearer(expired), 401},
				{"admin token", a14Bearer(admin), 403},
			} {
				r := a14Do(t, srv, method, "/api/mobile/device-token", c.header, map[string]string{"token": b1Token("auth")})
				if r.status != c.want {
					t.Errorf("%s with %s token: %d, want %d", method, c.name, r.status, c.want)
				}
			}
		}
	})

	t.Run("a PIN change signs the parent out and removes the parent's device tokens", func(t *testing.T) {
		other := b1Token("other-parent")
		ok(t, register(p1.token, other), "parent 1 registers")
		before := tokensOf(p1.id)
		r := a14Do(t, srv, "PUT", "/api/admin/students", a14Bearer(admin), map[string]any{
			"id": p2.student, "name": "B1 Parent Two Child", "parent_name": "B1 Parent Two", "parent_phone": p2.phone, "parent_pin": "604158",
		})
		if r.status != 200 {
			t.Fatalf("change PIN: %d %s", r.status, r.body)
		}
		expect(t, "parent 2 tokens after the PIN change", tokensOf(p2.id))
		expect(t, "parent 1 tokens after parent 2's PIN change", tokensOf(p1.id), before...)
		if r := register(p2.token, a); r.status != 401 {
			t.Errorf("register with the signed-out token: %d, want 401", r.status)
		}
		p2.pin = "604158"
		_, p2.token = login(p2, nil)
		ok(t, register(p2.token, a), "register after logging in again")
		expect(t, "parent 2 tokens after logging in again", tokensOf(p2.id), a)
	})

	t.Run("an update without a PIN keeps the parent's device tokens", func(t *testing.T) {
		before := tokensOf(p2.id)
		r := a14Do(t, srv, "PUT", "/api/admin/students", a14Bearer(admin), map[string]any{
			"id": p2.student, "name": "B1 Renamed Child", "parent_name": "B1 Parent Two", "parent_phone": p2.phone,
		})
		if r.status != 200 {
			t.Fatalf("update: %d %s", r.status, r.body)
		}
		expect(t, "parent 2 tokens after an update without PIN", tokensOf(p2.id), before...)
	})

	t.Run("login fcm_token registers through the same path", func(t *testing.T) {
		viaLogin := b1Token("via-login")
		if code, _ := login(p2, map[string]string{"fcm_token": viaLogin}); code != 200 {
			t.Fatalf("login with fcm_token: %d", code)
		}
		expect(t, "parent 2 tokens after login", tokensOf(p2.id), a, viaLogin)
		if code, _ := login(p2, map[string]string{"fcm_token": "not a valid token"}); code != 200 {
			t.Errorf("login with an invalid fcm_token: %d, want 200 (the token is ignored)", code)
		}
		expect(t, "parent 2 tokens after an invalid fcm_token", tokensOf(p2.id), a, viaLogin)
		var copied int
		db.QueryRow(`SELECT COUNT(*) FROM students WHERE fcm_token IS NOT NULL`).Scan(&copied)
		if copied != 0 {
			t.Errorf("%d students got fcm_token written; new code must not write the column", copied)
		}
	})

	t.Run("deleting a parent deletes the parent's device tokens", func(t *testing.T) {
		if _, err := db.Exec(`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (8199, 'B1 Gone', '+9647000000899', $1)`, a14Hash(t, "7777")); err != nil {
			t.Fatal(err)
		}
		gone := &parent{id: 8199, phone: "+9647000000899", pin: "7777"}
		if code, _ := login(gone, map[string]string{"fcm_token": b1Token("gone")}); code != 200 {
			t.Fatalf("login: %d", code)
		}
		expect(t, "tokens before delete", tokensOf(8199), b1Token("gone"))
		if _, err := db.Exec(`DELETE FROM parents WHERE id = 8199`); err != nil {
			t.Fatal(err)
		}
		expect(t, "tokens after delete", tokensOf(8199))
	})

	t.Run("logs never contain a full device token", func(t *testing.T) {
		out := srv.out.String()
		for _, tok := range []string{a, b, b1Token("via-login"), b1Token("cap-01"), b1Token("gone")} {
			if strings.Contains(out, tok) {
				t.Errorf("the server log contains a full device token")
			}
		}
	})
}

// TestB1PushFanOut verifies Task B1's sending side with a fake FCM server: every device of the
// student's parent gets each push, including for a child added after the devices registered;
// the C3/W5 rules still decide which punches notify; an unregistered token deletes only its own
// row and other errors delete nothing; absence pushes fan out the same way; no log line holds a
// full token.
func TestB1PushFanOut(t *testing.T) {
	capture := &w10Capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	db, _ := setupThrowawayDB(t, "b1push")
	fcm := newB1FakeFCM(t)
	bg := &background.Group{}
	app := &handlers.AppEnv{DB: db, FCMClient: fcm.client(t), Background: bg}
	settle := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if pending, err := bg.Wait(ctx); err != nil {
			t.Fatalf("background work still running: %d", pending)
		}
	}
	a, b := b1Token("fan-a"), b1Token("fan-b")
	dead, bad, live := b1Token("dead-1"), b1Token("bad-1"), b1Token("live-1")
	other := b1Token("other-parent")
	for _, q := range []string{
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('B1-DEV', 'Gate', true)`,
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (821, 'Fan Parent', '+9647000000821', 'x'), (822, 'Dead Parent', '+9647000000822', 'x'), (823, 'Other Parent', '+9647000000823', 'x')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES (821, 'Fan Child', 'B1-F1', 821), (822, 'Dead Child', 'B1-D1', 822), (823, 'Other Child', 'B1-O1', 823)`,
		`INSERT INTO device_tokens (parent_id, token) VALUES (821, '` + a + `'), (821, '` + b + `'), (822, '` + dead + `'), (822, '` + bad + `'), (822, '` + live + `'), (823, '` + other + `')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	today := tz.Today()
	punch := func(rfid, at string) {
		t.Helper()
		rec := httptest.NewRecorder()
		app.ADMSHandler(rec, httptest.NewRequest("POST", "/iclock/cdata?SN=B1-DEV&table=ATTLOG", strings.NewReader(rfid+"\t"+today+" "+at+"\t1\t1\n")))
		if rec.Code != 200 {
			t.Fatalf("ADMS push: %d", rec.Code)
		}
		settle()
	}
	expectSent := func(t *testing.T, what string, want ...string) {
		t.Helper()
		sort.Strings(want)
		if got := fcm.take(); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: sent to %v, want %v", what, got, want)
		}
	}
	tokenExists := func(tok string) bool {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM device_tokens WHERE token = $1`, tok).Scan(&n)
		return n == 1
	}

	t.Run("both phones of a parent receive the push", func(t *testing.T) {
		punch("B1-F1", "07:15:00")
		expectSent(t, "first check-in", a, b)
	})

	t.Run("C3 rules still decide which punches notify", func(t *testing.T) {
		punch("B1-F1", "07:20:00")
		expectSent(t, "second punch in the check-in window")
		punch("B1-F1", "10:00:00")
		expectSent(t, "dead-zone punch")
		punch("B1-F1", "12:30:00")
		expectSent(t, "first check-out", a, b)
	})

	t.Run("a child added later gets pushes with no new login", func(t *testing.T) {
		if _, err := db.Exec(`INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES (824, 'Fan Sibling', 'B1-F2', 821)`); err != nil {
			t.Fatal(err)
		}
		punch("B1-F2", "07:40:00")
		expectSent(t, "new sibling's check-in", a, b)
	})

	t.Run("an unregistered token deletes only its own row; other errors delete nothing", func(t *testing.T) {
		punch("B1-D1", "07:15:00")
		expectSent(t, "check-in to a parent with dead, bad and live phones", dead, bad, live)
		if tokenExists(dead) {
			t.Errorf("the unregistered token is still stored")
		}
		for _, tok := range []string{bad, live, a, b, other} {
			if !tokenExists(tok) {
				t.Errorf("a token that FCM did not report unregistered was deleted")
			}
		}
	})

	t.Run("absence pushes fan out to every phone", func(t *testing.T) {
		cron.ProcessDailyAbsences(db, app.FCMClient, bg)
		settle()
		sent := fcm.take()
		if strings.Join(sent, ",") != other {
			t.Errorf("absence pushes went to %v, want only the absent child's parent's phone", sent)
		}
	})

	t.Run("logs never contain a full device token", func(t *testing.T) {
		for _, l := range capture.snapshot() {
			line := l.Msg + fmt.Sprint(l.Attrs)
			for _, tok := range []string{a, b, dead, bad, live, other} {
				if strings.Contains(line, tok) {
					t.Errorf("full device token in log: %s", l.Msg)
				}
			}
		}
		var fingerprint bool
		for _, l := range capture.snapshot() {
			if l.Attrs["token"] == notify.TokenFingerprint(dead) {
				fingerprint = true
			}
		}
		if !fingerprint {
			t.Errorf("the dead token was never logged by its fingerprint")
		}
	})
}

// TestB1DeviceTokensMigration verifies migration 000022: the backfill copies each well-formed
// students.fcm_token once, to the parent of the newest student holding it, at most 10 per
// parent; re-running it adds nothing; down and up again work, also through golang-migrate.
func TestB1DeviceTokensMigration(t *testing.T) {
	db, _ := setupThrowawayDB(t, "b1mig")
	up := a14Migration(t, "000022_create_device_tokens.up.sql")
	down := a14Migration(t, "000022_create_device_tokens.down.sql")
	sqlExec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%v\n%.300s", err, q)
		}
	}
	sqlExec(down)
	sqlExec(`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (831, 'P1', '+9647000000831', 'x'), (832, 'P2', '+9647000000832', 'x'), (833, 'Many', '+9647000000833', 'x')`)
	shared, moved := b1Token("shared"), b1Token("moved")
	sqlExec(`INSERT INTO students (id, full_name, rfid_tag, parent_id, fcm_token) VALUES
		(8301, 'S1', 'M-1', 831, '` + shared + `'), (8302, 'S2', 'M-2', 831, '` + shared + `'),
		(8303, 'S3', 'M-3', 831, '` + moved + `'), (8304, 'S4', 'M-4', 832, '` + moved + `'),
		(8305, 'S5', 'M-5', 831, 'short'), (8306, 'S6', 'M-6', 831, 'has spaces in this token value'),
		(8307, 'S7', 'M-7', NULL, '` + b1Token("orphan") + `'), (8308, 'S8', 'M-8', 831, NULL)`)
	for i := 1; i <= 12; i++ {
		sqlExec(fmt.Sprintf(`INSERT INTO students (id, full_name, rfid_tag, parent_id, fcm_token) VALUES (%d, 'Many %d', 'MM-%d', 833, '%s')`, 8400+i, i, i, b1Token(fmt.Sprintf("many-%02d", i))))
	}
	rows := func() map[string]int {
		r, err := db.Query(`SELECT token, parent_id FROM device_tokens`)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		out := map[string]int{}
		for r.Next() {
			var tok string
			var p int
			r.Scan(&tok, &p)
			out[tok] = p
		}
		return out
	}
	check := func(t *testing.T) {
		t.Helper()
		got := rows()
		if got[shared] != 831 || got[moved] != 832 {
			t.Errorf("shared → %d (want 831), moved → %d (want 832, the newest student's parent)", got[shared], got[moved])
		}
		for tok := range got {
			if tok == "short" || strings.Contains(tok, " ") || tok == b1Token("orphan") {
				t.Errorf("backfill copied an invalid or parentless token")
			}
		}
		many := 0
		for i := 1; i <= 12; i++ {
			if _, ok := got[b1Token(fmt.Sprintf("many-%02d", i))]; ok {
				many++
				if i <= 2 {
					t.Errorf("backfill kept many-%02d, one of the two oldest", i)
				}
			}
		}
		if many != 10 || len(got) != 12 {
			t.Errorf("backfill: %d tokens for the parent with 12, %d in total; want 10 and 12", many, len(got))
		}
		var fcmLeft int
		db.QueryRow(`SELECT COUNT(*) FROM students WHERE fcm_token IS NOT NULL`).Scan(&fcmLeft)
		if fcmLeft != 19 {
			t.Errorf("students.fcm_token values: %d, want all 19 left untouched", fcmLeft)
		}
	}

	t.Run("backfill on messy data", func(t *testing.T) {
		sqlExec(up)
		check(t)
	})
	t.Run("running it again adds nothing", func(t *testing.T) {
		sqlExec(up)
		check(t)
	})
	t.Run("down and up again", func(t *testing.T) {
		sqlExec(down)
		var exists bool
		db.QueryRow(`SELECT to_regclass('device_tokens') IS NOT NULL`).Scan(&exists)
		if exists {
			t.Fatalf("down-migration left device_tokens")
		}
		sqlExec(up)
		check(t)
	})

	t.Run("golang-migrate up, down 1, up 1", func(t *testing.T) {
		migrate, err := exec.LookPath("migrate")
		if err != nil {
			t.Skip("golang-migrate CLI not installed")
		}
		cli, cliDSN := setupThrowawayDB(t, "b1cli")
		for _, tbl := range []string{"device_tokens", "settings", "notifications", "weekly_schedules", "student_leaves", "banners", "admins", "attendance_logs", "devices", "students", "parents"} {
			if _, err := cli.Exec("DROP TABLE IF EXISTS " + tbl + " CASCADE"); err != nil {
				t.Fatal(err)
			}
		}
		cli.Exec(`DROP FUNCTION IF EXISTS get_student_status(INT, DATE)`)
		u, _ := url.Parse(cliDSN)
		q := u.Query()
		q.Set("sslmode", "disable")
		u.RawQuery = q.Encode()
		run := func(args ...string) string {
			out, err := exec.Command(migrate, append([]string{"-path", testdb.MigrationsDir(), "-database", u.String()}, args...)...).CombinedOutput()
			if err != nil {
				t.Fatalf("migrate %v: %v\n%s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		run("up", "22")
		if v := run("version"); v != "22" {
			t.Errorf("version after up 22: %q", v)
		}
		run("down", "1")
		run("up", "1")
		var exists bool
		cli.QueryRow(`SELECT to_regclass('device_tokens') IS NOT NULL`).Scan(&exists)
		if v := run("version"); v != "22" || !exists {
			t.Errorf("after down 1 and up 1: version %q, device_tokens exists %v", v, exists)
		}
	})
}
