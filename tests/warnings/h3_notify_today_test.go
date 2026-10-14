package warnings

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"future_kids/internal/background"
	"future_kids/internal/handlers"
)

const (
	h3Warn   = "ADMSHandler: punches not dated today were stored without notifications"
	h3TagA   = "8301772"
	h3TagB   = "8302773"
	h3NameA  = "H3 Kid Zephyrine"
	h3NameB  = "H3 Kid Quillon"
	h3Device = "H3-DEV"
)

// h3App is an in-process app with a fake FCM and a log capture: two children with their own
// parent and phone, and a device.
type h3App struct {
	app  *handlers.AppEnv
	db   *sql.DB
	fcm  *b1FakeFCM
	bg   *background.Group
	logs *w10Capture
}

func newH3App(t *testing.T, label string) *h3App {
	t.Helper()
	a := &h3App{logs: &w10Capture{}, bg: &background.Group{}, fcm: newB1FakeFCM(t)}
	prev := slog.Default()
	slog.SetDefault(slog.New(a.logs))
	t.Cleanup(func() { slog.SetDefault(prev) })
	a.db, _ = setupThrowawayDB(t, label)
	k1Exec(t, a.db,
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (8301, 'H3 Parent Alpha', '+9647000008301', 'x'), (8302, 'H3 Parent Beta', '+9647000008302', 'x')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id, grade, section, created_at) VALUES
			(8301, '`+h3NameA+`', '`+h3TagA+`', 8301, 'G1', 'A', '2026-01-01 08:00'),
			(8302, '`+h3NameB+`', '`+h3TagB+`', 8302, 'G1', 'A', '2026-01-01 08:00')`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('`+h3Device+`', 'Gate', true)`,
		fmt.Sprintf(`INSERT INTO device_tokens (parent_id, token) VALUES (8301, '%s'), (8302, '%s')`, b1Token("h3-alpha"), b1Token("h3-beta")))
	a.app = &handlers.AppEnv{DB: a.db, FCMClient: a.fcm.client(t), Background: a.bg}
	return a
}

// punch sends one ATTLOG request with a line per "tag\tYYYY-MM-DD HH:MM:SS" and waits for the
// background work; the device must get 200 "OK".
func (a *h3App) punch(t *testing.T, lines ...string) {
	t.Helper()
	body := strings.Join(lines, "\t1\t1\n") + "\t1\t1\n"
	rec := httptest.NewRecorder()
	a.app.ADMSHandler(rec, httptest.NewRequest("POST", "/iclock/cdata?SN="+h3Device+"&table=ATTLOG", strings.NewReader(body)))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if pending, err := a.bg.Wait(ctx); err != nil {
		t.Fatalf("background work still running: %d", pending)
	}
	if rec.Code != 200 || rec.Body.String() != "OK" || rec.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("device response %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
}

// notes returns every notification as "title|body", oldest first.
func (a *h3App) notes(t *testing.T) []string {
	t.Helper()
	rows, err := a.db.Query(`SELECT title || '|' || body FROM notifications ORDER BY id`)
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

// warns returns the stale-punch WARNs logged so far as "stale=N future=N device=SN today=D".
func (a *h3App) warns() []string {
	var out []string
	for _, l := range a.logs.snapshot() {
		if l.Msg == h3Warn {
			if l.Level != slog.LevelWarn {
				out = append(out, "wrong level "+l.Level.String())
			}
			out = append(out, fmt.Sprintf("stale=%s future=%s device=%s today=%s", l.Attrs["stale"], l.Attrs["future"], l.Attrs["device_sn"], l.Attrs["today"]))
		}
	}
	return out
}

func (a *h3App) stored(t *testing.T) int {
	t.Helper()
	return k1Count(t, a.db, `SELECT COUNT(*) FROM attendance_logs`)
}

// TestH3NotifyOnlyToday verifies that a device punch is notified only when it is dated today
// (Asia/Baghdad, handlers.Clock): older and future punches are stored exactly as before but not
// notified, and each request that stores such punches logs one WARN with the two counts.
func TestH3NotifyOnlyToday(t *testing.T) {
	a := newH3App(t, "h3today")
	pinToday(t, k1Today+" 13:00")
	checkIn := func(name, at string) string {
		return "إشعار دخول|تم تسجيل دخول الطالب " + name + " الساعة " + at
	}
	expect := func(t *testing.T, what string, got, want []string) {
		t.Helper()
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s:\n got %q\nwant %q", what, got, want)
		}
	}
	var notes, warns []string

	t.Run("today's first punch is notified as before", func(t *testing.T) {
		a.punch(t, h3TagA+"\t"+k1Today+" 07:40:00")
		notes = append(notes, checkIn(h3NameA, "07:40"))
		expect(t, "notifications", a.notes(t), notes)
		expect(t, "pushes", a.fcm.take(), []string{b1Token("h3-alpha")})
		expect(t, "warnings", a.warns(), warns)
	})

	t.Run("yesterday and 30 days ago are stored, not notified", func(t *testing.T) {
		before := a.stored(t)
		a.punch(t, h3TagB+"\t2026-09-23 07:40:00", h3TagB+"\t2026-08-25 07:41:00")
		if a.stored(t) != before+2 {
			t.Errorf("stored %d punches, want 2", a.stored(t)-before)
		}
		expect(t, "notifications", a.notes(t), notes)
		expect(t, "pushes", a.fcm.take(), nil)
		warns = append(warns, "stale=2 future=0 device="+h3Device+" today="+k1Today)
		expect(t, "warnings", a.warns(), warns)
	})

	t.Run("a future punch is stored, not notified", func(t *testing.T) {
		before := a.stored(t)
		a.punch(t, h3TagB+"\t2026-09-27 07:40:00")
		if a.stored(t) != before+1 {
			t.Errorf("future punch not stored")
		}
		expect(t, "notifications", a.notes(t), notes)
		expect(t, "pushes", a.fcm.take(), nil)
		warns = append(warns, "stale=0 future=1 device="+h3Device+" today="+k1Today)
		expect(t, "warnings", a.warns(), warns)
	})

	t.Run("a mixed batch notifies only today's punch", func(t *testing.T) {
		before := a.stored(t)
		a.punch(t, h3TagB+"\t2026-09-22 12:00:00", h3TagB+"\t"+k1Today+" 07:45:00", h3TagB+"\t2026-09-28 12:05:00", h3TagB+"\t2026-09-21 07:30:00")
		if a.stored(t) != before+4 {
			t.Errorf("stored %d of the 4 punches", a.stored(t)-before)
		}
		notes = append(notes, checkIn(h3NameB, "07:45"))
		expect(t, "notifications", a.notes(t), notes)
		expect(t, "pushes", a.fcm.take(), []string{b1Token("h3-beta")})
		warns = append(warns, "stale=2 future=1 device="+h3Device+" today="+k1Today)
		expect(t, "warnings", a.warns(), warns)
	})

	t.Run("a resent backlog stores nothing and logs nothing", func(t *testing.T) {
		before := a.stored(t)
		a.punch(t, h3TagB+"\t2026-09-23 07:40:00", h3TagB+"\t2026-08-25 07:41:00")
		if a.stored(t) != before {
			t.Errorf("duplicates stored")
		}
		expect(t, "notifications", a.notes(t), notes)
		expect(t, "warnings", a.warns(), warns)
	})

	t.Run("midnight boundary", func(t *testing.T) {
		before := a.stored(t)
		a.punch(t, h3TagA+"\t2026-09-23 23:59:59", h3TagA+"\t"+k1Today+" 00:00:00", h3TagA+"\t"+k1Today+" 23:59:59", h3TagA+"\t2026-09-25 00:00:00")
		if a.stored(t) != before+4 {
			t.Errorf("stored %d of the 4 punches", a.stored(t)-before)
		}
		// 00:00:00 and 23:59:59 today count as today; outside both windows, the unchanged
		// first-of-window rule notifies neither, exactly as before.
		warns = append(warns, "stale=1 future=1 device="+h3Device+" today="+k1Today)
		expect(t, "warnings", a.warns(), warns)
		expect(t, "notifications", a.notes(t), notes)

		// The boundary moves with the clock: on Sunday 2026-09-27 (a school day) a punch of that
		// day is notified and one of the day before "today" is not.
		pinToday(t, "2026-09-27 13:00")
		a.punch(t, h3TagA+"\t2026-09-27 07:00:00", h3TagA+"\t2026-09-26 23:59:59")
		notes = append(notes, checkIn(h3NameA, "07:00"))
		expect(t, "notifications once today moves to 2026-09-27", a.notes(t), notes)
		warns = append(warns, "stale=1 future=0 device="+h3Device+" today=2026-09-27")
		expect(t, "warnings", a.warns(), warns)
		a.fcm.take()
	})

	t.Run("no log line names a student, PIN or token", func(t *testing.T) {
		for _, l := range a.logs.snapshot() {
			line := l.Msg + fmt.Sprint(l.Attrs)
			for _, secret := range []string{h3NameA, h3NameB, "Zephyrine", "Quillon", h3TagA, h3TagB, b1Token("h3-alpha"), b1Token("h3-beta"), "H3 Parent", "+96470000083"} {
				if strings.Contains(line, secret) {
					t.Errorf("log line contains %q: %s", secret, line)
				}
			}
		}
	})
}

// h3Exchange is one device request and what the server answered.
type h3Exchange struct {
	name, method, path string
	body               any
}

// TestH3DifferentialAgainstReference replays the hardware scenarios against the reference binary
// (H3_REFERENCE_BINARY, built from the commit before this change) and the current one, each on its
// own identically seeded database with FAKE_TODAY=2026-09-24. Device responses and stored punches
// must be byte-identical for every request; notifications must be identical for same-day punches,
// and only the reference notifies old or future ones.
func TestH3DifferentialAgainstReference(t *testing.T) {
	ref := os.Getenv("H3_REFERENCE_BINARY")
	if ref == "" {
		t.Skip("set H3_REFERENCE_BINARY to a build of the previous commit to run the differential test")
	}
	type side struct {
		srv *g3Server
		db  *sql.DB
	}
	start := func(label, binary string) side {
		db, dsn := setupThrowawayDB(t, label)
		k1Exec(t, db,
			`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1, 'PA', '+9647000008401', 'x'), (2, 'PB', '+9647000008402', 'x'), (3, 'PC', '+9647000008403', 'x'), (4, 'PD', '+9647000008404', 'x')`,
			`INSERT INTO students (id, full_name, rfid_tag, parent_id, grade, section, created_at) VALUES
				(1, 'Kid A', '8401', 1, 'G1', 'A', '2026-01-01 08:00'), (2, 'Kid B', '8402', 2, 'G1', 'A', '2026-01-01 08:00'),
				(3, 'Kid C', '8403', 3, 'G1', 'B', '2026-01-01 08:00'), (4, 'Kid D', '8404', 4, 'G2', 'A', '2026-01-01 08:00'),
				(5, 'Kid E', '8405', NULL, 'G2', 'B', '2026-01-01 08:00'), (6, 'Kid F', '8406', 1, 'G1', 'A', '2026-01-01 08:00')`,
			`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('H3-DEV', 'Gate', true), ('H3-OFF', 'Old gate', false)`)
		t.Setenv("G3_API_BINARY", binary)
		return side{g3Start(t, dsn, g3FreePort(t), true, "FAKE_TODAY="+k1Today, "ABSENCE_CRON_SCHEDULE=0 0 1 1 *"), db}
	}
	old := start("h3ref", ref)
	cur := start("h3cur", "")

	lines := func(ls ...string) string { return strings.Join(ls, "\t1\t1\n") + "\t1\t1\n" }
	adms := "/iclock/cdata?SN=H3-DEV&table=ATTLOG"
	sameDay := []h3Exchange{
		{"first-of-window batch", "POST", adms, lines("8404\t2026-09-24 07:05:00", "8401\t2026-09-24 07:15:00", "8401\t2026-09-24 07:18:00", "8401\t2026-09-24 10:30:00",
			"8401\t2026-09-24 12:05:00", "8401\t2026-09-24 12:15:00", "8402\t2026-09-24 07:03:00", "8402\t2026-09-24 07:03:40",
			"8403\t2026-09-24 07:20:00", "8403\t2026-09-24 07:10:00", "8404\t2026-09-24 07:25:00", "8405\t2026-09-24 07:26:00")},
		{"the same batch resent", "POST", adms, lines("8401\t2026-09-24 07:15:00", "8402\t2026-09-24 07:03:00")},
		{"malformed lines", "POST", adms, "garbage\n8406\tnot-a-date\t1\t1\n\n8406\t2026-09-24 07:30:00\t1\t1\n"},
		{"unknown PIN", "POST", adms, lines("999999\t2026-09-24 07:31:00")},
		{"non-ATTLOG table", "POST", "/iclock/cdata?SN=H3-DEV&table=OPERLOG", "OPLOG 4\t0\t2026-09-24 08:00:00\n"},
		{"unregistered device", "POST", "/iclock/cdata?SN=H3-NOPE&table=ATTLOG", lines("8406\t2026-09-24 07:32:00")},
		{"disabled device", "POST", "/iclock/cdata?SN=H3-OFF&table=ATTLOG", lines("8406\t2026-09-24 07:33:00")},
		{"missing SN", "POST", "/iclock/cdata?table=ATTLOG", lines("8406\t2026-09-24 07:34:00")},
		{"empty batch", "POST", adms, ""},
		{"handshake", "GET", "/iclock/cdata?SN=H3-DEV&options=all", nil},
		{"command poll", "GET", "/iclock/getrequest?SN=H3-DEV", nil},
		{"oversized body", "POST", adms, strings.Repeat("x", int(handlers.MaxADMSBodyBytes)+1)},
		{"ADMS alias route", "POST", "/api/attendance/push?SN=H3-DEV&table=ATTLOG", lines("8406\t2026-09-24 12:10:00")},
		{"JSON push route", "POST", "/api/attendance/push/json?SN=H3-DEV", map[string]string{"device_sn": "H3-DEV", "rfid_tag": "8403", "push_time": "2026-09-24 12:20:00"}},
	}
	otherDays := []h3Exchange{
		{"yesterday and 30 days ago", "POST", adms, lines("8401\t2026-09-23 07:15:00", "8402\t2026-08-25 07:16:00")},
		{"future", "POST", adms, lines("8403\t2026-09-27 07:15:00")},
		{"mixed batch", "POST", adms, lines("8404\t2026-09-22 12:00:00", "8406\t2026-09-24 12:06:00", "8405\t2026-09-28 07:20:00")},
	}

	replay := func(xs []h3Exchange) {
		for _, x := range xs {
			a, b := a14Do(t, old.srv, x.method, x.path, nil, x.body), a14Do(t, cur.srv, x.method, x.path, nil, x.body)
			ha, hb := a.header.Clone(), b.header.Clone()
			ha.Del("Date")
			hb.Del("Date")
			if a.status != b.status || string(a.body) != string(b.body) || fmt.Sprint(ha) != fmt.Sprint(hb) {
				t.Errorf("%s: reference %d %v %q, current %d %v %q", x.name, a.status, ha, a.body, b.status, hb, b.body)
			}
		}
	}
	dump := func(db *sql.DB, q string) []string {
		rows, err := db.Query(q)
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
	const punchesSQL = `SELECT concat_ws('|', student_id, COALESCE(device_sn, '-'), check_time, source) FROM attendance_logs ORDER BY student_id, check_time`
	const notesSQL = `SELECT concat_ws('|', parent_id, title, body) FROM notifications ORDER BY 1`
	settled := func(db *sql.DB) []string {
		var last []string
		for deadline, stable := time.Now().Add(5*time.Second), time.Now(); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			n := dump(db, notesSQL)
			if strings.Join(n, "\n") != strings.Join(last, "\n") {
				last, stable = n, time.Now()
			} else if time.Since(stable) > 500*time.Millisecond {
				break
			}
		}
		return last
	}
	// same compares two row lists in Go's byte order (the database orders by its collation).
	same := func(what string, a, b []string) {
		a, b = append([]string{}, a...), append([]string{}, b...)
		sort.Strings(a)
		sort.Strings(b)
		if strings.Join(a, "\n") != strings.Join(b, "\n") {
			t.Errorf("%s differ:\n got %q\nwant %q", what, a, b)
		}
	}

	replay(sameDay)
	refNotes, curNotes := settled(old.db), settled(cur.db)
	same("same-day stored punches", dump(old.db, punchesSQL), dump(cur.db, punchesSQL))
	same("same-day notifications", refNotes, curNotes)
	if len(curNotes) < 5 {
		t.Errorf("only %d same-day notifications; the scenario should produce at least 5: %q", len(curNotes), curNotes)
	}
	t.Logf("same day: %d punches and %d notifications identical", len(dump(cur.db, punchesSQL)), len(curNotes))

	replay(otherDays)
	refAfter, curAfter := settled(old.db), settled(cur.db)
	same("stored punches after old and future batches", dump(old.db, punchesSQL), dump(cur.db, punchesSQL))
	wantCur := append(append([]string{}, curNotes...), "1|إشعار خروج|تم تسجيل خروج الطالب Kid F الساعة 12:06")
	sort.Strings(wantCur)
	same("current notifications after old and future batches (only today's 12:06 punch is new)", curAfter, wantCur)
	// The reference's extra notifications, as a multiset difference: the text has no date, so an
	// old day's punch can read exactly like today's.
	left := map[string]int{}
	for _, n := range curAfter {
		left[n]++
	}
	var extra []string
	for _, n := range refAfter {
		if left[n] > 0 {
			left[n]--
			continue
		}
		extra = append(extra, n)
	}
	sort.Strings(extra)
	wantExtra := []string{"1|إشعار دخول|تم تسجيل دخول الطالب Kid A الساعة 07:15", "2|إشعار دخول|تم تسجيل دخول الطالب Kid B الساعة 07:16",
		"3|إشعار دخول|تم تسجيل دخول الطالب Kid C الساعة 07:15", "4|إشعار خروج|تم تسجيل خروج الطالب Kid D الساعة 12:00"}
	same("the reference's notifications for old and future punches", extra, wantExtra)
	t.Logf("old and future days: punches identical; reference sent %d extra notification(s) the current binary does not: %q", len(extra), extra)
	if strings.Contains(old.srv.out.String(), h3Warn) || strings.Count(cur.srv.out.String(), h3Warn) != 3 {
		t.Errorf("WARN count: reference %d, current %d; want 0 and 3", strings.Count(old.srv.out.String(), h3Warn), strings.Count(cur.srv.out.String(), h3Warn))
	}
}
