package warnings

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"future_kids/internal/background"
	"future_kids/internal/cron"
	"future_kids/internal/handlers"
	"future_kids/internal/testdb"

	"golang.org/x/crypto/bcrypt"
)

const (
	m1Up30   = "000030_allow_notifications_without_phone.up.sql"
	m1Down30 = "000030_allow_notifications_without_phone.down.sql"
	m1Up31   = "000031_drop_legacy_phone_and_fcm_columns.up.sql"
	m1Down31 = "000031_drop_legacy_phone_and_fcm_columns.down.sql"
)

func m1Apply(t *testing.T, db *sql.DB, files ...string) {
	t.Helper()
	for _, f := range files {
		if _, err := db.Exec(a14Migration(t, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

// m1Restore29 rolls a fully migrated database back to the schema of version 29.
func m1Restore29(t *testing.T, db *sql.DB) {
	t.Helper()
	m1Apply(t, db, m1Down31, m1Down30)
}

// m1Legacy reports which of the objects 000031 drops exist.
func m1Legacy(t *testing.T, db *sql.DB) string {
	t.Helper()
	var s string
	if err := db.QueryRow(`SELECT concat_ws(' ',
		CASE WHEN EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'notifications' AND column_name = 'parent_phone') THEN 'parent_phone' END,
		CASE WHEN EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'notifications' AND column_name = 'parent_phone_original') THEN 'parent_phone_original' END,
		CASE WHEN EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'students' AND column_name = 'fcm_token') THEN 'fcm_token' END,
		CASE WHEN to_regclass('idx_notifications_phone') IS NOT NULL THEN 'index' END,
		CASE WHEN EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'notifications_fill_parent_id') THEN 'trigger' END,
		CASE WHEN to_regprocedure('notifications_fill_parent_id()') IS NOT NULL THEN 'function' END)`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

const m1AllLegacy = "parent_phone parent_phone_original fcm_token index trigger function"

func m1PhoneNullable(t *testing.T, db *sql.DB) string {
	t.Helper()
	var s string
	db.QueryRow(`SELECT is_nullable FROM information_schema.columns WHERE table_name = 'notifications' AND column_name = 'parent_phone'`).Scan(&s)
	return s
}

// m1Schema is g3Schema plus every user trigger.
func m1Schema(t *testing.T, db *sql.DB) string {
	t.Helper()
	var triggers string
	db.QueryRow(`SELECT COALESCE(string_agg(tgname || ' on ' || tgrelid::regclass::text || ' ' || pg_get_triggerdef(oid), ',' ORDER BY tgname), '') FROM pg_trigger WHERE NOT tgisinternal`).Scan(&triggers)
	return g3Schema(t, db) + "\ntriggers " + triggers
}

func m1Exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%v\n%.300s", err, q)
	}
}

func m1Count(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%v\n%.300s", err, q)
	}
	return n
}

// m1Fresh returns an empty database and a golang-migrate runner for it.
func m1Fresh(t *testing.T, label string) (*sql.DB, func(args ...string) (string, error)) {
	t.Helper()
	migrate, err := exec.LookPath("migrate")
	if err != nil {
		t.Skip("golang-migrate CLI not installed")
	}
	cli, cliDSN := setupThrowawayDB(t, label)
	for _, tbl := range []string{"admin_activity_log", "school_closures", "broadcasts", "announcements", "banner_images", "device_tokens", "settings", "notifications", "weekly_schedules", "student_leaves", "banners", "admins", "attendance_logs", "devices", "students", "parents"} {
		m1Exec(t, cli, "DROP TABLE IF EXISTS "+tbl+" CASCADE")
	}
	cli.Exec(`DROP FUNCTION IF EXISTS get_student_status(INT, DATE)`)
	cli.Exec(`DROP FUNCTION IF EXISTS notifications_fill_parent_id()`)
	u, _ := url.Parse(cliDSN)
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	return cli, func(args ...string) (string, error) {
		out, err := exec.Command(migrate, append([]string{"-path", testdb.MigrationsDir(), "-database", u.String()}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
}

func m1Run(t *testing.T, run func(args ...string) (string, error), args ...string) {
	t.Helper()
	if out, err := run(args...); err != nil {
		t.Fatalf("migrate %v: %v\n%s", args, err, out)
	}
}

func m1Version(t *testing.T, run func(args ...string) (string, error), want string) {
	t.Helper()
	if v, _ := run("version"); v != want {
		t.Errorf("version %q, want %q", v, want)
	}
}

// TestM1LegacyObjectsGone verifies the schema after 000031: notifications and students keep no
// phone or FCM token column, the 000023 trigger, its function and the phone index are gone, and
// what replaced them (notifications.parent_id with its indexes, device_tokens, the unique
// parents.phone_number) is in place.
func TestM1LegacyObjectsGone(t *testing.T) {
	db, _ := setupThrowawayDB(t, "m1gone")
	if got := m1Legacy(t, db); got != "" {
		t.Errorf("legacy objects left: %s", got)
	}
	if n := m1Count(t, db, `SELECT COUNT(*) FROM information_schema.columns WHERE table_name IN ('notifications', 'students') AND (column_name LIKE '%phone%' OR column_name LIKE '%fcm%')`); n != 0 {
		t.Errorf("%d phone or FCM columns left on notifications and students", n)
	}
	var parentID, parentIndexes, tokens int
	var phoneNullable string
	var phoneUnique bool
	db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'notifications' AND column_name = 'parent_id'),
		(SELECT COUNT(*) FROM pg_indexes WHERE indexname IN ('idx_notifications_parent_id_id', 'idx_notifications_parent_unread')),
		(SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'device_tokens' AND column_name IN ('parent_id', 'token')),
		(SELECT is_nullable FROM information_schema.columns WHERE table_name = 'parents' AND column_name = 'phone_number'),
		EXISTS (SELECT 1 FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
			WHERE i.indrelid = 'parents'::regclass AND i.indisunique AND i.indnatts = 1 AND a.attname = 'phone_number')`).Scan(&parentID, &parentIndexes, &tokens, &phoneNullable, &phoneUnique)
	if parentID != 1 || parentIndexes != 2 || tokens != 2 || phoneNullable != "NO" || !phoneUnique {
		t.Errorf("parent_id %d, its indexes %d, device_tokens columns %d, parents.phone_number nullable %s unique %v; want 1, 2, 2, NO, true", parentID, parentIndexes, tokens, phoneNullable, phoneUnique)
	}
}

// TestM1NewCode runs the new code on 000031 and on 000030 without 000031 (the deploy window):
// punch, absence, broadcast and closure notifications are stored with parent_id and no phone,
// the parent lists them, and login's fcm_token still registers a device token for any phone
// format.
func TestM1NewCode(t *testing.T) {
	for _, at30 := range []bool{false, true} {
		name := "000031"
		if at30 {
			name = "000030 without 000031"
		}
		t.Run(name, func(t *testing.T) {
			db, dsn := setupThrowawayDB(t, fmt.Sprintf("m1new%v", at30))
			if at30 {
				m1Apply(t, db, m1Down31)
				if got := m1Legacy(t, db); got != m1AllLegacy || m1PhoneNullable(t, db) != "YES" {
					t.Fatalf("at 30: legacy %q, parent_phone nullable %q", got, m1PhoneNullable(t, db))
				}
			}
			j1Seed(t, db)
			m1Exec(t, db, `INSERT INTO devices (serial_number, location_name, is_active) VALUES ('M1-DEV', 'Gate', true)`)
			m1Exec(t, db, `INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (3101, 'M1 Login', '+9647000003101', $1)`, a14Hash(t, "482193"))
			hash, _ := bcrypt.GenerateFromPassword([]byte(a14AdminPassword), bcrypt.DefaultCost)
			m1Exec(t, db, `UPDATE admins SET password_hash = $1 WHERE username = 'admin'`, string(hash))

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
			today := pinToday(t, "2026-09-24 13:00")
			rec := httptest.NewRecorder()
			app.ADMSHandler(rec, httptest.NewRequest("POST", "/iclock/cdata?SN=M1-DEV&table=ATTLOG", strings.NewReader("J1-1A\t"+today+" 07:15:00\t1\t1\n")))
			settle()
			if rec.Code != 200 || m1Count(t, db, `SELECT COUNT(*) FROM attendance_logs WHERE device_sn = 'M1-DEV'`) != 1 {
				t.Fatalf("punch: %d %s", rec.Code, rec.Body)
			}
			if n := m1Count(t, db, `SELECT COUNT(*) FROM notifications WHERE parent_id = 3001 AND title = 'إشعار دخول'`); n != 1 {
				t.Errorf("punch notifications for the parent: %d, want 1", n)
			}
			cron.ProcessDailyAbsences(db, app.FCMClient, bg)
			settle()
			if n := m1Count(t, db, `SELECT COUNT(*) FROM notifications WHERE parent_id = 3002 AND title = 'إشعار غياب'`); n != 2 {
				t.Errorf("absence notifications for the parent of two absent children: %d, want 2", n)
			}

			srv := g3Start(t, dsn, g3FreePort(t), true, "TRUSTED_PROXY_CIDRS=127.0.0.1/32", "ABSENCE_CRON_SCHEDULE=0 0 1 1 *", "FAKE_TODAY="+k1Today)
			admin := a14Bearer(a14AdminToken(t, srv))
			for _, c := range []struct {
				body map[string]any
				want int
			}{
				{j1Body("M1 class", "Class", j1Class("G1", "A")), 2},
				{j1Body("M1 parent", "One parent", j1Parent("0700 000 3003")), 1},
				{j1Body("M1 all", "Everyone", j1All()), 7},
			} {
				r := a14Do(t, srv, "POST", "/api/admin/broadcasts", admin, c.body)
				var resp struct {
					Data struct {
						ID             *int `json:"id"`
						RecipientCount int  `json:"recipient_count"`
					} `json:"data"`
				}
				json.Unmarshal(r.body, &resp)
				if r.status != 200 || resp.Data.ID == nil || resp.Data.RecipientCount != c.want {
					t.Fatalf("%s: %d %s, want %d recipients", c.body["title"], r.status, r.body, c.want)
				}
				if rows, linked := m1Count(t, db, `SELECT COUNT(*) FROM notifications WHERE broadcast_id = $1`, *resp.Data.ID), m1Count(t, db, `SELECT COUNT(DISTINCT parent_id) FROM notifications WHERE broadcast_id = $1`, *resp.Data.ID); rows != c.want || linked != c.want {
					t.Errorf("%s: %d rows for %d parents, want %d each", c.body["title"], rows, linked, c.want)
				}
			}
			r := a14Do(t, srv, "POST", "/api/admin/holidays", admin, k1Closure("kind", "holiday", "start_date", "2026-10-11", "end_date", "2026-10-15", "title", "عطلة"))
			if res := k1Decode(t, r); r.status != 200 || res.Data.RecipientCount != 7 {
				t.Errorf("closure: %d %s, want 7 recipients", r.status, r.body)
			}
			if n := m1Count(t, db, `SELECT COUNT(DISTINCT n.parent_id) FROM notifications n JOIN school_closures c ON c.broadcast_id = n.broadcast_id`); n != 7 {
				t.Errorf("closure notifications reach %d parents, want 7", n)
			}
			if n := m1Count(t, db, `SELECT COUNT(*) FROM notifications WHERE parent_id IS NULL`); n != 0 {
				t.Errorf("%d notifications without parent_id", n)
			}
			if at30 {
				if n := m1Count(t, db, `SELECT COUNT(*) FROM notifications WHERE parent_phone IS NOT NULL`); n != 0 {
					t.Errorf("the new code stored a phone number on %d notifications", n)
				}
			}
			r = a14Do(t, srv, "GET", "/api/mobile/notifications", j1ParentToken(t, 3001, "+9647000003001"), nil)
			var list struct {
				Data []struct {
					Title string `json:"title"`
				} `json:"data"`
			}
			json.Unmarshal(r.body, &list)
			if want := m1Count(t, db, `SELECT COUNT(*) FROM notifications WHERE parent_id = 3001`); r.status != 200 || len(list.Data) != want || want != 5 {
				t.Errorf("parent list: %d, %d items, %d stored; want 200 and 5 (punch, absence, class, all, closure)", r.status, len(list.Data), want)
			}

			token := b1Token("m1-login")
			login := func(body map[string]string) {
				t.Helper()
				if r := a14Do(t, srv, "POST", "/api/mobile/login", nil, body); r.status != 200 {
					t.Fatalf("login %v: %d %s", body, r.status, r.body)
				}
			}
			tokens := func() string {
				var s string
				db.QueryRow(`SELECT COALESCE(string_agg(parent_id || ':' || token, ',' ORDER BY token), '') FROM device_tokens`).Scan(&s)
				return s
			}
			login(map[string]string{"phone": "07000003101", "pin": "482193", "fcm_token": token})
			if got := tokens(); got != "3101:"+token {
				t.Errorf("after login with fcm_token: %q", got)
			}
			login(map[string]string{"phone": "009647000003101", "pin": "482193", "fcm_token": "not a valid token"})
			login(map[string]string{"phone": "+9647000003101", "pin": "482193"})
			if got := tokens(); got != "3101:"+token {
				t.Errorf("after an invalid token and a login without one: %q", got)
			}
		})
	}
}

// TestM1OldStatements runs the notification inserts of the code before this release, copied
// from e00de47, and of the code before 000023: they work on 000030, where 000023's trigger
// still links rows by phone, and fail on 000031.
func TestM1OldStatements(t *testing.T) {
	db, _ := setupThrowawayDB(t, "m1old")
	j1Seed(t, db)
	broadcast := func() int {
		var id int
		if err := db.QueryRow(`INSERT INTO broadcasts (title, body, audience_type, recipient_count) VALUES ('Old', 'Old body', 'all', 0) RETURNING id`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	const oldRecipientsSQL = `
	FROM parents p
	WHERE CASE $1::text
		WHEN 'all' THEN true
		WHEN 'parent' THEN p.id = $2::int
		ELSE EXISTS (
			SELECT 1 FROM students s
			WHERE s.parent_id = p.id AND s.is_active = true
			  AND ($3::text IS NULL OR s.grade = $3::text)
			  AND ($4::text IS NULL OR s.section = $4::text))
	END`
	statements := []struct {
		name string
		run  func() (int64, error)
	}{
		{"SaveNotificationHistory", func() (int64, error) {
			res, err := db.Exec("INSERT INTO notifications (parent_id, parent_phone, title, body) VALUES ($1, $2, $3, $4)", 3001, "+9647000003001", "Old punch", "b")
			if err != nil {
				return 0, err
			}
			return res.RowsAffected()
		}},
		{"CreateBroadcast", func() (int64, error) {
			res, err := db.Exec(`
		INSERT INTO notifications (parent_id, parent_phone, title, body, broadcast_id)
		SELECT p.id, p.phone_number, $5, $6, $7 `+oldRecipientsSQL+`
		ORDER BY p.id`, "all", nil, nil, nil, "Old", "Old body", broadcast())
			if err != nil {
				return 0, err
			}
			return res.RowsAffected()
		}},
		{"insert before 000023", func() (int64, error) {
			res, err := db.Exec(`INSERT INTO notifications (parent_phone, title, body) VALUES ('+9647000003002', 'Older', 'b')`)
			if err != nil {
				return 0, err
			}
			return res.RowsAffected()
		}},
	}
	for _, s := range statements {
		if _, err := s.run(); err == nil || !strings.Contains(err.Error(), `column "parent_phone" of relation "notifications" does not exist`) {
			t.Errorf("%s on 000031: %v, want the missing column error", s.name, err)
		}
	}
	if n := m1Count(t, db, `SELECT COUNT(*) FROM notifications`); n != 0 {
		t.Errorf("failed statements stored %d rows", n)
	}
	m1Apply(t, db, m1Down31)
	for _, s := range statements {
		if n, err := s.run(); err != nil || n == 0 {
			t.Errorf("%s on 000030: %d rows, %v", s.name, n, err)
		}
	}
	var punch, older sql.NullInt64
	db.QueryRow(`SELECT (SELECT parent_id FROM notifications WHERE title = 'Old punch'), (SELECT parent_id FROM notifications WHERE title = 'Older')`).Scan(&punch, &older)
	if punch.Int64 != 3001 || older.Int64 != 3002 || m1Count(t, db, `SELECT COUNT(*) FROM notifications WHERE title = 'Old' AND parent_id IS NOT NULL`) != 6 {
		t.Errorf("on 000030: punch parent %v, pre-000023 row linked to %v; want 3001, 3002 and 6 broadcast rows", punch, older)
	}
}

// TestM1NewCodeAt29 documents why 000030 comes before the deploy: on 000029 the new code still
// answers the device and stores the punch exactly as on 000031, but the punch notification is
// missing from the history (one ERROR), and a broadcast or a closure with parents answers 500
// and writes nothing.
func TestM1NewCodeAt29(t *testing.T) {
	capture := &w10Capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })
	today := pinToday(t, "2026-09-24 13:00")

	type outcome struct {
		code    int
		header  string
		body    string
		punches string
		history int
	}
	punch := func(t *testing.T, at29 bool) outcome {
		db, _ := setupThrowawayDB(t, fmt.Sprintf("m1p%v", at29))
		if at29 {
			m1Restore29(t, db)
		}
		j1Seed(t, db)
		m1Exec(t, db, `INSERT INTO devices (serial_number, location_name, is_active) VALUES ('M1-DEV', 'Gate', true)`)
		bg := &background.Group{}
		app := &handlers.AppEnv{DB: db, FCMClient: newB1FakeFCM(t).client(t), Background: bg}
		rec := httptest.NewRecorder()
		app.ADMSHandler(rec, httptest.NewRequest("POST", "/iclock/cdata?SN=M1-DEV&table=ATTLOG", strings.NewReader("J1-1A\t"+today+" 07:15:00\t1\t1\nJ1-1A\t"+today+" 07:15:30\t1\t1\nNOBODY\t"+today+" 07:16:00\t1\t1\n")))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if pending, err := bg.Wait(ctx); err != nil {
			t.Fatalf("background work still running: %d", pending)
		}
		var o outcome
		o.code, o.body = rec.Code, rec.Body.String()
		o.header = fmt.Sprint(rec.Header())
		db.QueryRow(`SELECT string_agg(student_id || ' ' || device_sn || ' ' || check_time, ',' ORDER BY id) FROM attendance_logs`).Scan(&o.punches)
		o.history = m1Count(t, db, `SELECT COUNT(*) FROM notifications`)
		return o
	}
	before := len(capture.snapshot())
	at31 := punch(t, false)
	errors31 := 0
	for _, l := range capture.snapshot()[before:] {
		if l.Msg == "Failed to save notification history" {
			errors31++
		}
	}
	before = len(capture.snapshot())
	at29 := punch(t, true)
	if at29.code != at31.code || at29.body != at31.body || at29.header != at31.header || at29.punches != at31.punches || at29.punches == "" {
		t.Errorf("device answer or stored punches differ\n29: %+v\n31: %+v", at29, at31)
	}
	if at31.history != 1 || errors31 != 0 || at29.history != 0 {
		t.Errorf("history rows: %d on 31 (%d errors), %d on 29; want 1, 0, 0", at31.history, errors31, at29.history)
	}
	failed := 0
	for _, l := range capture.snapshot()[before:] {
		if l.Msg == "Failed to save notification history" && l.Attrs["parent_id"] == "3001" && strings.Contains(l.Attrs["error"], "parent_phone") {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("%d ERROR lines for the lost punch notification, want 1", failed)
	}

	db, dsn := setupThrowawayDB(t, "m1b29")
	m1Restore29(t, db)
	j1Seed(t, db)
	hash, _ := bcrypt.GenerateFromPassword([]byte(a14AdminPassword), bcrypt.DefaultCost)
	m1Exec(t, db, `UPDATE admins SET password_hash = $1 WHERE username = 'admin'`, string(hash))
	srv := g3Start(t, dsn, g3FreePort(t), true, "TRUSTED_PROXY_CIDRS=127.0.0.1/32", "ABSENCE_CRON_SCHEDULE=0 0 1 1 *", "FAKE_TODAY="+k1Today)
	admin := a14Bearer(a14AdminToken(t, srv))
	if r := a14Do(t, srv, "POST", "/api/admin/broadcasts", admin, j1Body("M1 on 29", "b", j1All())); r.status != 500 {
		t.Errorf("broadcast on 000029: %d %s, want 500", r.status, r.body)
	}
	if r := a14Do(t, srv, "POST", "/api/admin/holidays", admin, k1Closure("kind", "holiday", "start_date", "2026-10-11", "end_date", "2026-10-15", "title", "عطلة")); r.status != 500 {
		t.Errorf("closure on 000029: %d %s, want 500", r.status, r.body)
	}
	if n := m1Count(t, db, `SELECT (SELECT COUNT(*) FROM broadcasts) + (SELECT COUNT(*) FROM notifications) + (SELECT COUNT(*) FROM school_closures)`); n != 0 {
		t.Errorf("the failed requests wrote %d rows", n)
	}
}

// TestM1Migration verifies 000030 and 000031: each step up and down gives exactly the schema
// golang-migrate builds for that version (column order aside), re-running any of the four files
// changes nothing, the downs refill parent_phone as documented and keep 000029's down runnable,
// golang-migrate reaches 31 from every version 20 to 29 keeping device tokens and parent links,
// a phone collision still stops at 29 before 30, and every migration rolls back and applies
// again from an empty database.
func TestM1Migration(t *testing.T) {
	golden := map[string]string{}
	for _, v := range []string{"29", "30", "31"} {
		db, run := m1Fresh(t, "m1g"+v)
		m1Run(t, run, "goto", v)
		golden[v] = m1Schema(t, db)
	}
	if !strings.Contains(golden["29"], "col notifications.parent_phone character varying 20 null=NO") ||
		strings.Replace(golden["29"], "col notifications.parent_phone character varying 20 null=NO", "col notifications.parent_phone character varying 20 null=YES", 1) != golden["30"] {
		t.Errorf("000030 changes more than the nullability of notifications.parent_phone")
	}

	t.Run("up, down, down, up, up give each version's exact schema; every file re-runs cleanly", func(t *testing.T) {
		db, run := m1Fresh(t, "m1steps")
		m1Run(t, run, "goto", "31")
		check := func(t *testing.T, version string) {
			t.Helper()
			m1Version(t, run, version)
			if got := m1Schema(t, db); got != golden[version] {
				t.Errorf("schema at %s differs from the one built up to %s", version, version)
			}
		}
		check(t, "31")
		if got := m1Legacy(t, db); got != "" {
			t.Errorf("at 31: %s", got)
		}
		m1Apply(t, db, m1Up31)
		check(t, "31")
		m1Run(t, run, "down", "1")
		check(t, "30")
		m1Apply(t, db, m1Down31, m1Up30)
		check(t, "30")
		m1Run(t, run, "down", "1")
		check(t, "29")
		m1Apply(t, db, m1Down30)
		check(t, "29")
		m1Run(t, run, "up", "1")
		check(t, "30")
		m1Run(t, run, "up", "1")
		check(t, "31")
	})

	t.Run("data through the downs: phones come back from the parents, 000029's down still runs", func(t *testing.T) {
		db, run := m1Fresh(t, "m1data")
		m1Run(t, run, "goto", "29")
		m1Exec(t, db, `INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (7001, 'One', '+9647000007001', 'x'), (7002, 'Two', '+9647000007002', 'x')`)
		m1Exec(t, db, `INSERT INTO students (full_name, rfid_tag, parent_id, fcm_token) VALUES ('Kid', 'M1-K', 7001, $1)`, b1Token("m1-data"))
		m1Exec(t, db, `INSERT INTO notifications (parent_id, parent_phone, title, body) VALUES (7001, '+9647000007001', 'old', 'b')`)
		m1Exec(t, db, `INSERT INTO notifications (parent_phone, title, body) VALUES ('+9647000007999', 'orphan', 'b')`)
		m1Run(t, run, "goto", "30")
		m1Exec(t, db, `INSERT INTO notifications (parent_id, title, body) VALUES (7002, 'at 30', 'b')`)
		m1Run(t, run, "goto", "31")
		m1Exec(t, db, `INSERT INTO notifications (parent_id, title, body) VALUES (7001, 'at 31', 'b')`)
		rows := func() string {
			var s string
			db.QueryRow(`SELECT string_agg(title || '=' || COALESCE(parent_id::text, '-') || '/' || COALESCE(to_jsonb(n) ->> 'parent_phone', '<null>'), ' ' ORDER BY id) FROM notifications n`).Scan(&s)
			return s
		}
		if got, want := rows(), "old=7001/<null> orphan=-/<null> at 30=7002/<null> at 31=7001/<null>"; got != want {
			t.Errorf("at 31:\n got %s\nwant %s", got, want)
		}

		m1Run(t, run, "down", "1")
		if got, want := rows(), "old=7001/<null> orphan=-/<null> at 30=7002/<null> at 31=7001/<null>"; got != want {
			t.Errorf("after 000031 down, phones must come back empty:\n got %s\nwant %s", got, want)
		}
		if n := m1Count(t, db, `SELECT COUNT(*) FROM students WHERE fcm_token IS NOT NULL`) + m1Count(t, db, `SELECT COUNT(*) FROM notifications WHERE parent_phone_original IS NOT NULL`); n != 0 {
			t.Errorf("%d dropped values came back", n)
		}
		m1Exec(t, db, `INSERT INTO notifications (parent_phone, title, body) VALUES ('+9647000007002', 'trigger', 'b')`)

		m1Run(t, run, "down", "1")
		want := "old=7001/+9647000007001 orphan=-/ at 30=7002/+9647000007002 at 31=7001/+9647000007001 trigger=7002/+9647000007002"
		if got := rows(); got != want {
			t.Errorf("after 000030 down:\n got %s\nwant %s", got, want)
		}
		if m1PhoneNullable(t, db) != "NO" {
			t.Errorf("parent_phone is nullable after 000030 down")
		}

		m1Run(t, run, "down", "1")
		m1Version(t, run, "28")
		if got := rows(); got != want {
			t.Errorf("000029 down changed notification phones:\n got %s\nwant %s", got, want)
		}
		if n := m1Count(t, db, `SELECT COUNT(*) FROM information_schema.columns WHERE column_name IN ('parent_phone_original', 'phone_number_original')`); n != 0 {
			t.Errorf("000029 down left %d *_original columns", n)
		}
		m1Run(t, run, "up")
		m1Version(t, run, "31")
	})

	for from := 20; from <= 29; from++ {
		t.Run(fmt.Sprintf("golang-migrate from version %d to 31", from), func(t *testing.T) {
			db, run := m1Fresh(t, fmt.Sprintf("m1v%d", from))
			m1Run(t, run, "goto", fmt.Sprint(from))
			token := b1Token(fmt.Sprintf("m1-from-%d", from))
			m1Exec(t, db, `INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (7101, 'P', '+9647000007101', 'x')`)
			if from <= 21 {
				m1Exec(t, db, `INSERT INTO students (full_name, rfid_tag, parent_id, fcm_token) VALUES ('Kid', 'M1-V', 7101, $1)`, token)
			} else {
				m1Exec(t, db, `INSERT INTO students (full_name, rfid_tag, parent_id) VALUES ('Kid', 'M1-V', 7101)`)
				m1Exec(t, db, `INSERT INTO device_tokens (parent_id, token) VALUES (7101, $1)`, token)
			}
			if from <= 22 {
				m1Exec(t, db, `INSERT INTO notifications (parent_phone, title, body) VALUES ('+9647000007101', 't', 'b')`)
			} else {
				m1Exec(t, db, `INSERT INTO notifications (parent_id, parent_phone, title, body) VALUES (7101, '+9647000007101', 't', 'b')`)
			}
			m1Run(t, run, "up")
			m1Version(t, run, "31")
			if got := m1Legacy(t, db); got != "" {
				t.Errorf("at 31: %s", got)
			}
			if n := m1Count(t, db, `SELECT COUNT(*) FROM device_tokens WHERE parent_id = 7101 AND token = $1`, token); n != 1 {
				t.Errorf("device token kept: %d, want 1", n)
			}
			if n := m1Count(t, db, `SELECT COUNT(*) FROM notifications WHERE parent_id = 7101`); n != 1 {
				t.Errorf("linked notifications: %d, want 1", n)
			}
		})
	}

	t.Run("a phone collision stops at 29, before 30", func(t *testing.T) {
		db, run := m1Fresh(t, "m1coll")
		m1Run(t, run, "goto", "28")
		m1Exec(t, db, `INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (995, 'A', '07000000995', 'x'), (996, 'B', '+9647000000995', 'x')`)
		out, err := run("up")
		if err == nil || !strings.Contains(out, "[995, 996]") {
			t.Fatalf("000029 should abort listing [995, 996]: %v\n%s", err, out)
		}
		m1Version(t, run, "29 (dirty)")
		if got := m1Legacy(t, db); got != "parent_phone fcm_token index trigger function" || m1PhoneNullable(t, db) != "NO" {
			t.Errorf("after the aborted 000029: legacy %q, parent_phone nullable %q; want 000030 and 000031 not applied", got, m1PhoneNullable(t, db))
		}
	})

	t.Run("every migration rolls back and applies again from an empty database", func(t *testing.T) {
		db, run := m1Fresh(t, "m1all")
		m1Run(t, run, "up")
		m1Run(t, run, "down", "-all")
		if n := m1Count(t, db, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name <> 'schema_migrations'`); n != 0 {
			t.Errorf("%d tables left after down -all", n)
		}
		m1Run(t, run, "up")
		m1Version(t, run, "31")
		if got := m1Schema(t, db); got != golden["31"] {
			t.Errorf("schema after down -all and up differs from a straight up")
		}
	})
}

// TestM1PhoneCheckScript verifies that db/scripts/check_phone_formats.sql runs on 000031, where
// it reports no notification numbers, and still reports them on 000029.
func TestM1PhoneCheckScript(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "db", "scripts", "check_phone_formats.sql"))
	if err != nil {
		t.Fatal(err)
	}
	query := string(script)
	query = strings.TrimSuffix(strings.TrimSpace(query[strings.Index(query, "WITH p AS"):strings.Index(query, "ROLLBACK;")]), ";")
	report := func(t *testing.T, db *sql.DB) map[string]string {
		t.Helper()
		rows, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := map[string]string{}
		for rows.Next() {
			var kind, detail, value string
			rows.Scan(&kind, &detail, &value)
			got[kind+"|"+detail] = value
		}
		return got
	}
	for _, at29 := range []bool{false, true} {
		db, _ := setupThrowawayDB(t, fmt.Sprintf("m1scr%v", at29))
		if at29 {
			m1Restore29(t, db)
		}
		m1Exec(t, db, `INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (6101, 'A', '07000006101', 'h'), (6102, 'B', '+9647000006102', 'h')`)
		if at29 {
			m1Exec(t, db, `INSERT INTO notifications (parent_id, parent_phone, title, body) VALUES (6101, '07000006101', 't', 'b')`)
		} else {
			m1Exec(t, db, `INSERT INTO notifications (parent_id, title, body) VALUES (6101, 't', 'b')`)
		}
		got := report(t, db)
		if got["would change|"] != "1" || got["already canonical|"] != "1" || got["invalid parent ids|"] != "none" {
			t.Errorf("at29=%v: parent counts %v", at29, got)
		}
		shape, ok := got["notification shape|99999999999"]
		if at29 != ok || (at29 && shape != "1") {
			t.Errorf("at29=%v: notification shape %q present %v", at29, shape, ok)
		}
	}
}
