package warnings

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"future_kids/internal/background"
	"future_kids/internal/cron"
	"future_kids/internal/handlers"
	"future_kids/internal/testdb"

	"github.com/golang-jwt/jwt/v5"
)

type b2Page struct {
	ids        []int
	read       map[int]bool
	hasMore    bool
	nextBefore any
	unread     any
	hasUnread  bool
}

// TestB2NotificationHistory verifies Task B2 through the real API: history is keyed by parent,
// so it survives a change of the parent's phone number and never shows another parent's rows;
// pagination is unchanged; PUT /api/mobile/notifications/read?id= marks one notification and
// PUT /api/mobile/notifications/read-all marks all of the caller's, with the list reporting
// unread_count; both routes sit behind the parent middleware.
func TestB2NotificationHistory(t *testing.T) {
	srv, db := a14Server(t, "b2api")
	admin := a14AdminToken(t, srv)

	newParent := func(name, phone, pin, rfid string) (int, any) {
		t.Helper()
		r := a14Do(t, srv, "POST", "/api/admin/students", a14Bearer(admin), map[string]any{
			"name": name + " Child", "parent_name": name, "parent_phone": phone, "parent_pin": pin, "rfid_tag": rfid,
		})
		if r.status != 200 {
			t.Fatalf("create %s: %d %s", name, r.status, r.body)
		}
		var id int
		db.QueryRow(`SELECT id FROM parents WHERE phone_number = $1`, phone).Scan(&id)
		return id, r.json(t)["data"].(map[string]any)["id"]
	}
	login := func(phone, pin string) string {
		t.Helper()
		r := a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": phone, "pin": pin})
		if r.status != 200 {
			t.Fatalf("login %s: %d %s", phone, r.status, r.body)
		}
		return fmt.Sprint(r.json(t)["data"].(map[string]any)["token"])
	}
	page := func(t *testing.T, token, query string) b2Page {
		t.Helper()
		r := a14Do(t, srv, "GET", "/api/mobile/notifications"+query, a14Bearer(token), nil)
		if r.status != 200 {
			t.Fatalf("list%s: %d %s", query, r.status, r.body)
		}
		m := r.json(t)
		p := b2Page{read: map[int]bool{}, hasMore: m["has_more"] == true, nextBefore: m["next_before"]}
		p.unread, p.hasUnread = m["unread_count"]
		for _, n := range m["data"].([]any) {
			n := n.(map[string]any)
			id := int(n["id"].(float64))
			p.ids = append(p.ids, id)
			p.read[id] = n["is_read"] == true
		}
		return p
	}
	all := func(t *testing.T, token string) []int {
		t.Helper()
		var ids []int
		q := ""
		for {
			p := page(t, token, q)
			ids = append(ids, p.ids...)
			if !p.hasMore {
				return ids
			}
			q = fmt.Sprintf("?before=%v", p.nextBefore)
		}
	}
	markOne := func(token string, query string) a14Response {
		return a14Do(t, srv, "PUT", "/api/mobile/notifications/read"+query, a14Bearer(token), nil)
	}
	markAll := func(token string) a14Response {
		return a14Do(t, srv, "PUT", "/api/mobile/notifications/read-all", a14Bearer(token), nil)
	}
	insert := func(parentID int, phone string, n int, tag string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO notifications (parent_id, parent_phone, title, body) SELECT $1, $2, 'B2 title', $3 || ' ' || g FROM generate_series(1, $4) g`, parentID, phone, tag, n); err != nil {
			t.Fatal(err)
		}
	}
	idsOf := func(parentID int) []int {
		rows, _ := db.Query(`SELECT id FROM notifications WHERE parent_id = $1 ORDER BY id DESC`, parentID)
		defer rows.Close()
		var out []int
		for rows.Next() {
			var id int
			rows.Scan(&id)
			out = append(out, id)
		}
		return out
	}

	p1, _ := newParent("B2 Parent One", "+9647000000921", "Kq7#vR2m!Tx9pW4z", "B2-1")
	p2, s2 := newParent("B2 Parent Two", "+9647000000922", "Ze4&uM9k?Ga2fC7x", "B2-2")
	t1, t2 := login("+9647000000921", "Kq7#vR2m!Tx9pW4z"), login("+9647000000922", "Ze4&uM9k?Ga2fC7x")
	insert(p1, "+9647000000921", 3, "one")
	insert(p2, "+9647000000922", 150, "two")

	t.Run("history survives a change of the parent's phone number", func(t *testing.T) {
		if _, err := db.Exec(`UPDATE parents SET phone_number = '+9647000000929' WHERE id = $1`, p1); err != nil {
			t.Fatal(err)
		}
		if got, want := all(t, t1), idsOf(p1); fmt.Sprint(got) != fmt.Sprint(want) || len(got) != 3 {
			t.Errorf("parent 1 after the phone change sees %v, want its 3 notifications %v", got, want)
		}
	})

	t.Run("another parent's history never appears", func(t *testing.T) {
		mine := map[int]bool{}
		for _, id := range all(t, t1) {
			mine[id] = true
		}
		for _, id := range idsOf(p2) {
			if mine[id] {
				t.Fatalf("parent 1 sees parent 2's notification %d", id)
			}
		}
	})

	t.Run("pagination is unchanged: 100 then 50, no gaps or repeats", func(t *testing.T) {
		first := page(t, t2, "")
		if len(first.ids) != 100 || !first.hasMore || fmt.Sprint(first.nextBefore) != fmt.Sprint(first.ids[99]) {
			t.Fatalf("first page: %d items, has_more %v, next_before %v", len(first.ids), first.hasMore, first.nextBefore)
		}
		second := page(t, t2, fmt.Sprintf("?before=%v", first.nextBefore))
		if len(second.ids) != 50 || second.hasMore || second.nextBefore != nil {
			t.Fatalf("second page: %d items, has_more %v, next_before %v", len(second.ids), second.hasMore, second.nextBefore)
		}
		got := append(append([]int{}, first.ids...), second.ids...)
		if fmt.Sprint(got) != fmt.Sprint(idsOf(p2)) {
			t.Errorf("pages are not the parent's 150 notifications, newest first, without gaps or repeats")
		}
		for _, bad := range []string{"abc", "0", "-1", "1.5"} {
			if r := a14Do(t, srv, "GET", "/api/mobile/notifications?before="+bad, a14Bearer(t2), nil); r.status != 400 {
				t.Errorf("before=%s: %d, want 400", bad, r.status)
			}
		}
	})

	t.Run("unread_count counts every unread notification", func(t *testing.T) {
		p := page(t, t2, "")
		if !p.hasUnread || fmt.Sprint(p.unread) != "150" {
			t.Errorf("unread_count %v (present %v), want 150", p.unread, p.hasUnread)
		}
	})

	t.Run("mark one: own notification becomes read, idempotent", func(t *testing.T) {
		id := idsOf(p2)[5]
		for i := 0; i < 2; i++ {
			r := markOne(t2, fmt.Sprintf("?id=%d", id))
			if r.status != 200 || r.json(t)["status"] != "success" {
				t.Fatalf("mark %d (call %d): %d %s", id, i+1, r.status, r.body)
			}
		}
		p := page(t, t2, "")
		if !p.read[id] || fmt.Sprint(p.unread) != "149" {
			t.Errorf("after marking %d: is_read %v, unread_count %v; want true and 149", id, p.read[id], p.unread)
		}
		for _, other := range p.ids {
			if other != id && p.read[other] {
				t.Errorf("notification %d became read too", other)
			}
		}
	})

	t.Run("mark one: another parent's or a missing id is the same 404", func(t *testing.T) {
		notMine := a14Do(t, srv, "PUT", fmt.Sprintf("/api/mobile/notifications/read?id=%d", idsOf(p2)[0]), a14Bearer(t1), nil)
		missing := markOne(t1, "?id=999999")
		if notMine.status != 404 || missing.status != 404 || string(notMine.body) != string(missing.body) {
			t.Errorf("another parent's id: %d %s; missing id: %d %s; want the same 404", notMine.status, notMine.body, missing.status, missing.body)
		}
		if p := page(t, t2, ""); p.read[idsOf(p2)[0]] {
			t.Errorf("parent 1 marked parent 2's notification as read")
		}
	})

	t.Run("mark one: invalid ids get 400", func(t *testing.T) {
		for _, q := range []string{"", "?id=", "?id=abc", "?id=0", "?id=-3", "?id=1.5"} {
			if r := markOne(t1, q); r.status != 400 || r.json(t)["status"] != "error" {
				t.Errorf("mark %q: %d %s, want 400", q, r.status, r.body)
			}
		}
	})

	t.Run("mark all: only the caller's rows, returns the count, idempotent", func(t *testing.T) {
		before2 := page(t, t2, "")
		r := markAll(t1)
		if r.status != 200 || fmt.Sprint(r.json(t)["data"].(map[string]any)["updated"]) != "3" {
			t.Fatalf("mark all for parent 1: %d %s, want 3 updated", r.status, r.body)
		}
		r = markAll(t1)
		if r.status != 200 || fmt.Sprint(r.json(t)["data"].(map[string]any)["updated"]) != "0" {
			t.Errorf("mark all again: %d %s, want 0 updated", r.status, r.body)
		}
		if p := page(t, t1, ""); fmt.Sprint(p.unread) != "0" {
			t.Errorf("parent 1 unread_count %v, want 0", p.unread)
		}
		if after2 := page(t, t2, ""); fmt.Sprint(after2.unread) != fmt.Sprint(before2.unread) {
			t.Errorf("parent 1's mark-all changed parent 2's unread_count from %v to %v", before2.unread, after2.unread)
		}
	})

	t.Run("notifications sent after mark-all arrive unread", func(t *testing.T) {
		if r := a14Do(t, srv, "POST", "/api/admin/devices", a14Bearer(admin), map[string]any{"serial_number": "B2-DEV", "location_name": "Gate", "is_active": true}); r.status != 200 {
			t.Fatalf("device: %d", r.status)
		}
		if r := markAll(t2); r.status != 200 {
			t.Fatalf("mark all: %d", r.status)
		}
		before := len(idsOf(p2))
		a14Do(t, srv, "POST", "/iclock/cdata?SN=B2-DEV&table=ATTLOG", nil, "B2-2\t2026-09-23 07:15:00\t1\t1\n")
		deadline := time.Now().Add(5 * time.Second)
		for len(idsOf(p2)) == before && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		p := page(t, t2, "")
		if len(idsOf(p2)) != before+1 || p.read[p.ids[0]] || fmt.Sprint(p.unread) != "1" {
			t.Errorf("new check-in notification: stored %d, newest is_read %v, unread_count %v; want 1 new, unread, count 1", len(idsOf(p2))-before, p.read[p.ids[0]], p.unread)
		}
	})

	t.Run("past notifications stay with the parent they were sent to", func(t *testing.T) {
		mineBefore := len(idsOf(p2))
		r := a14Do(t, srv, "PUT", "/api/admin/students", a14Bearer(admin), map[string]any{
			"id": s2, "name": "B2 Parent Two Child", "parent_name": "B2 Parent Three", "parent_phone": "+9647000000923", "parent_pin": "Uf5&Hz9c!Mr3eJ7a",
		})
		if r.status != 200 {
			t.Fatalf("move student: %d %s", r.status, r.body)
		}
		if got := len(idsOf(p2)); got != mineBefore {
			t.Errorf("parent 2 has %d notifications after the student moved, want %d", got, mineBefore)
		}
	})

	t.Run("the routes need a valid parent token", func(t *testing.T) {
		expired, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"parent_id": p1, "role": "parent", "sv": 0, "exp": time.Now().Add(-time.Hour).Unix(), "iat": time.Now().Add(-2 * time.Hour).Unix(),
		}).SignedString([]byte("g3"))
		pinChanged := login("+9647000000929", "Kq7#vR2m!Tx9pW4z")
		var student any
		db.QueryRow(`SELECT id FROM students WHERE parent_id = $1`, p1).Scan(&student)
		if r := a14Do(t, srv, "PUT", "/api/admin/students", a14Bearer(admin), map[string]any{
			"id": student, "name": "B2 Parent One Child", "parent_name": "B2 Parent One", "parent_phone": "+9647000000929", "parent_pin": "Nc6?Fa3w@Ub8rZ5k",
		}); r.status != 200 {
			t.Fatalf("change PIN: %d %s", r.status, r.body)
		}
		for _, path := range []string{"/api/mobile/notifications/read?id=1", "/api/mobile/notifications/read-all"} {
			for _, c := range []struct {
				name   string
				header map[string]string
				want   int
			}{
				{"missing", nil, 401},
				{"malformed", a14Bearer("not-a-jwt"), 401},
				{"expired", a14Bearer(expired), 401},
				{"signed out by a PIN change", a14Bearer(pinChanged), 401},
				{"admin", a14Bearer(admin), 403},
			} {
				if r := a14Do(t, srv, "PUT", path, c.header, nil); r.status != c.want {
					t.Errorf("PUT %s with %s token: %d, want %d", path, c.name, r.status, c.want)
				}
			}
		}
	})

	t.Run("the list and the unread count use their indexes", func(t *testing.T) {
		if _, err := db.Exec(`INSERT INTO parents (id, full_name, phone_number, pin_code) SELECT g, 'Bulk', '+96470' || lpad(g::text, 8, '0'), 'x' FROM generate_series(20000001, 20000200) g`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO notifications (parent_id, parent_phone, title, body, is_read) SELECT 20000001 + g % 200, 'x', 't', 'b', g % 3 = 0 FROM generate_series(1, 20000) g`); err != nil {
			t.Fatal(err)
		}
		db.Exec(`ANALYZE notifications`)
		for query, index := range map[string]string{
			`EXPLAIN SELECT n.id FROM notifications n WHERE n.parent_id = 20000050 AND (NULL::bigint IS NULL OR n.id < NULL) ORDER BY n.id DESC LIMIT 101`: "idx_notifications_parent_id_id",
			`EXPLAIN SELECT COUNT(*) FROM notifications n WHERE n.parent_id = 20000050 AND n.is_read IS NOT TRUE`:                                          "idx_notifications_parent_unread",
		} {
			rows, err := db.Query(query)
			if err != nil {
				t.Fatal(err)
			}
			var plan []string
			for rows.Next() {
				var line string
				rows.Scan(&line)
				plan = append(plan, line)
			}
			rows.Close()
			if !strings.Contains(strings.Join(plan, "\n"), index) {
				t.Errorf("%s\n%s\nwant %s", query, strings.Join(plan, "\n"), index)
			}
		}
	})

	t.Run("deleting a parent deletes the parent's notifications", func(t *testing.T) {
		if _, err := db.Exec(`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (9299, 'B2 Gone', '+9647000000999', 'x')`); err != nil {
			t.Fatal(err)
		}
		insert(9299, "+9647000000999", 4, "gone")
		if _, err := db.Exec(`DELETE FROM parents WHERE id = 9299`); err != nil {
			t.Fatal(err)
		}
		var left int
		db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE parent_id = 9299 OR parent_phone = '+9647000000999'`).Scan(&left)
		if left != 0 {
			t.Errorf("%d notifications left after deleting their parent", left)
		}
	})
}

// TestB2InsertsWriteParentID verifies that the code itself stores parent_id on every
// notification, with the rollout trigger disabled: punch and absence notifications both do,
// the C3/W5 rules still decide which punches notify, and the B1 fan-out is unchanged.
func TestB2InsertsWriteParentID(t *testing.T) {
	capture := &w10Capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	db, _ := setupThrowawayDB(t, "b2insert")
	if _, err := db.Exec(`ALTER TABLE notifications DISABLE TRIGGER notifications_fill_parent_id`); err != nil {
		t.Fatalf("the rollout trigger is missing: %v", err)
	}
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
	a, b := b1Token("b2-a"), b1Token("b2-b")
	for _, q := range []string{
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('B2-DEV', 'Gate', true)`,
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (931, 'Punch Parent', '+9647000000931', 'x'), (932, 'Absent Parent', '+9647000000932', 'x')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES (931, 'Punch Child', 'B2-P1', 931), (932, 'Absent Child', 'B2-A1', 932)`,
		`INSERT INTO device_tokens (parent_id, token) VALUES (931, '` + a + `'), (931, '` + b + `')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	today := pinToday(t, "2026-09-24 13:00")
	punch := func(at string) {
		rec := httptest.NewRecorder()
		app.ADMSHandler(rec, httptest.NewRequest("POST", "/iclock/cdata?SN=B2-DEV&table=ATTLOG", strings.NewReader("B2-P1\t"+today+" "+at+"\t1\t1\n")))
		settle()
	}
	rows := func(parentPhone string) (withParent, without int) {
		db.QueryRow(`SELECT COUNT(*) FILTER (WHERE parent_id IS NOT NULL), COUNT(*) FILTER (WHERE parent_id IS NULL) FROM notifications WHERE parent_phone = $1`, parentPhone).Scan(&withParent, &without)
		return
	}

	t.Run("punch notifications store parent_id and phone; C3 rules and fan-out unchanged", func(t *testing.T) {
		punch("07:15:00")
		punch("07:20:00")
		punch("10:00:00")
		punch("12:30:00")
		with, without := rows("+9647000000931")
		if with != 2 || without != 0 {
			t.Errorf("punch notifications: %d with parent_id, %d without; want 2 (first check-in, first check-out) and 0", with, without)
		}
		sent := fcm.take()
		want := []string{a, a, b, b}
		sort.Strings(want)
		if strings.Join(sent, ",") != strings.Join(want, ",") {
			t.Errorf("pushes went to %v, want both phones twice", sent)
		}
	})

	t.Run("absence notifications store parent_id and phone", func(t *testing.T) {
		cron.ProcessDailyAbsences(db, app.FCMClient, bg)
		settle()
		if with, without := rows("+9647000000932"); with != 1 || without != 0 {
			t.Errorf("absence notification: %d with parent_id, %d without; want 1 and 0", with, without)
		}
	})
}

// TestB2NotificationsMigration verifies migration 000023: the backfill links rows by phone,
// keeps and reports rows matching no parent, the trigger links rows the old code inserts,
// re-running changes nothing, down and up again work, golang-migrate reaches it from versions
// 20, 21 and 22, and 000028 (phone normalisation) still applies after it, collision abort
// included.
func TestB2NotificationsMigration(t *testing.T) {
	db, dsn := setupThrowawayDB(t, "b2mig")
	down := a14Migration(t, "000023_key_notifications_by_parent.down.sql")
	sqlExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%v\n%.300s", err, q)
		}
	}
	psql, err := exec.LookPath("psql")
	if err != nil {
		t.Skip("psql not installed")
	}
	applyUp := func(t *testing.T) string {
		t.Helper()
		out, err := exec.Command(psql, dsn, "-X", "-q", "-v", "ON_ERROR_STOP=1", "-f", filepath.Join(testdb.MigrationsDir(), "000023_key_notifications_by_parent.up.sql")).CombinedOutput()
		if err != nil {
			t.Fatalf("000023 up: %v\n%s", err, out)
		}
		return string(out)
	}
	sqlExec(down)
	sqlExec(`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (941, 'One', '+9647000000941', 'x'), (942, 'Two', '+9647000000942', 'x')`)
	sqlExec(`INSERT INTO notifications (parent_phone, title, body) VALUES
		('+9647000000941', 't', 'one-a'), ('+9647000000941', 't', 'one-b'), ('+9647000000942', 't', 'two-a'),
		('+9647000000999', 't', 'orphan-a'), ('07000000941', 't', 'orphan-b')`)
	state := func() map[string]sql.NullInt64 {
		r, err := db.Query(`SELECT body, parent_id FROM notifications`)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		out := map[string]sql.NullInt64{}
		for r.Next() {
			var body string
			var p sql.NullInt64
			r.Scan(&body, &p)
			out[body] = p
		}
		return out
	}
	check := func(t *testing.T) {
		t.Helper()
		s := state()
		for body, want := range map[string]int64{"one-a": 941, "one-b": 941, "two-a": 942} {
			if !s[body].Valid || s[body].Int64 != want {
				t.Errorf("%s: parent_id %v, want %d", body, s[body], want)
			}
		}
		for _, body := range []string{"orphan-a", "orphan-b"} {
			if p, ok := s[body]; !ok || p.Valid {
				t.Errorf("%s: present %v, parent_id %v; want kept with NULL", body, ok, p)
			}
		}
	}

	t.Run("backfill links matched rows and reports unmatched ones", func(t *testing.T) {
		out := applyUp(t)
		check(t)
		if !strings.Contains(out, "notifications matching no parent: 2") {
			t.Errorf("no NOTICE with the unmatched count:\n%s", out)
		}
	})

	t.Run("rows the old code inserts without parent_id are linked by the trigger", func(t *testing.T) {
		sqlExec(`INSERT INTO notifications (parent_phone, title, body) VALUES ('+9647000000942', 't', 'old-code'), ('+9647000000998', 't', 'old-code-orphan')`)
		s := state()
		if !s["old-code"].Valid || s["old-code"].Int64 != 942 || s["old-code-orphan"].Valid {
			t.Errorf("trigger: old-code → %v (want 942), old-code-orphan → %v (want NULL)", s["old-code"], s["old-code-orphan"])
		}
	})

	t.Run("running it again changes nothing", func(t *testing.T) {
		before := fmt.Sprint(state())
		if out := applyUp(t); !strings.Contains(out, "notifications matching no parent: 3") {
			t.Errorf("second run NOTICE:\n%s", out)
		}
		if after := fmt.Sprint(state()); after != before {
			t.Errorf("second run changed rows:\n%s\n%s", before, after)
		}
	})

	t.Run("down and up again", func(t *testing.T) {
		sqlExec(down)
		var column bool
		db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'notifications' AND column_name = 'parent_id')`).Scan(&column)
		if column {
			t.Fatalf("down-migration left notifications.parent_id")
		}
		applyUp(t)
		check(t)
	})

	migrate, err := exec.LookPath("migrate")
	if err != nil {
		t.Skip("golang-migrate CLI not installed")
	}
	fresh := func(t *testing.T, label string) (*sql.DB, func(args ...string) (string, error)) {
		t.Helper()
		cli, cliDSN := setupThrowawayDB(t, label)
		for _, tbl := range []string{"banner_images", "device_tokens", "settings", "notifications", "school_closures", "broadcasts", "announcements", "weekly_schedules", "student_leaves", "banners", "admins", "attendance_logs", "devices", "students", "parents"} {
			if _, err := cli.Exec("DROP TABLE IF EXISTS " + tbl + " CASCADE"); err != nil {
				t.Fatal(err)
			}
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

	for _, from := range []string{"20", "21", "22"} {
		t.Run("golang-migrate from version "+from, func(t *testing.T) {
			cli, run := fresh(t, "b2v"+from)
			if out, err := run("goto", from); err != nil {
				t.Fatalf("goto %s: %v\n%s", from, err, out)
			}
			if _, err := cli.Exec(`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (951, 'P', '+9647000000951', 'x');
				INSERT INTO notifications (parent_phone, title, body) VALUES ('+9647000000951', 't', 'b'), ('+9647000000959', 't', 'orphan')`); err != nil {
				t.Fatal(err)
			}
			if out, err := run("goto", "23"); err != nil {
				t.Fatalf("goto 23: %v\n%s", err, out)
			}
			var linked, orphan int
			cli.QueryRow(`SELECT COUNT(*) FILTER (WHERE parent_id = 951), COUNT(*) FILTER (WHERE parent_id IS NULL) FROM notifications`).Scan(&linked, &orphan)
			if linked != 1 || orphan != 1 {
				t.Errorf("after 000023: %d linked, %d unmatched; want 1 and 1", linked, orphan)
			}
			if out, err := run("up"); err != nil {
				t.Fatalf("up (000028): %v\n%s", err, out)
			}
			if v, _ := run("version"); v != "28" {
				t.Errorf("version %q, want 28", v)
			}
		})
	}

	t.Run("000028 still aborts on collisions after 000023", func(t *testing.T) {
		cli, run := fresh(t, "b2coll")
		if out, err := run("goto", "23"); err != nil {
			t.Fatalf("goto 23: %v\n%s", err, out)
		}
		if _, err := cli.Exec(`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (961, 'A', '07000000961', 'x'), (962, 'B', '+9647000000961', 'x')`); err != nil {
			t.Fatal(err)
		}
		out, err := run("up")
		if err == nil || !strings.Contains(out, "[961, 962]") {
			t.Fatalf("000028 should abort listing [961, 962]: %v\n%s", err, out)
		}
		var originals bool
		cli.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'parents' AND column_name = 'phone_number_original')`).Scan(&originals)
		if originals {
			t.Errorf("the aborted 000028 left changes behind")
		}
	})
}
