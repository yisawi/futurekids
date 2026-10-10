package warnings

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"future_kids/internal/auth"
	"future_kids/internal/background"
	"future_kids/internal/config"
	"future_kids/internal/handlers"
	"future_kids/internal/notify"
	"future_kids/internal/testdb"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
)

// j1Seed creates the J1 families. Children: 3001 has G1/A and G1/B, 3002 two children in G1/A,
// 3003 G2/A, 3004 only a deactivated child in G1/A, 3005 no children, 3006 g1/A (lower case).
func j1Seed(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES
			(3001, 'J1 Parent One', '+9647000003001', 'x'), (3002, 'J1 Parent Two', '+9647000003002', 'x'),
			(3003, 'J1 Parent Three', '+9647000003003', 'x'), (3004, 'J1 Parent Four', '+9647000003004', 'x'),
			(3005, 'J1 Parent Five', '+9647000003005', 'x'), (3006, 'J1 Parent Six', '+9647000003006', 'x')`,
		`SELECT setval('parents_id_seq', 4000)`,
		`INSERT INTO students (full_name, rfid_tag, parent_id, grade, section, is_active) VALUES
			('J1 Kid 1a', 'J1-1A', 3001, 'G1', 'A', true), ('J1 Kid 1b', 'J1-1B', 3001, 'G1', 'B', true),
			('J1 Kid 2a', 'J1-2A', 3002, 'G1', 'A', true), ('J1 Kid 2b', 'J1-2B', 3002, 'G1', 'A', true),
			('J1 Kid 3', 'J1-3', 3003, 'G2', 'A', true), ('J1 Kid 4', 'J1-4', 3004, 'G1', 'A', false),
			('J1 Kid 6', 'J1-6', 3006, 'g1', 'A', true)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
}

func j1Body(title, body string, audience map[string]any, extra ...any) map[string]any {
	m := map[string]any{"title": title, "body": body, "audience": audience}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i].(string)] = extra[i+1]
	}
	return m
}

func j1All() map[string]any { return map[string]any{"type": "all"} }

func j1Parent(phone string) map[string]any {
	return map[string]any{"type": "parent", "parent_phone": phone}
}

func j1Class(grade, section any) map[string]any {
	m := map[string]any{"type": "class"}
	if grade != nil {
		m["grade"] = grade
	}
	if section != nil {
		m["section"] = section
	}
	return m
}

// j1Snapshot lists every broadcast and every notification row.
func j1Snapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id || '|' || title || '|' || body || '|' || audience_type || '|' || COALESCE(audience_grade, '-') || '|' || COALESCE(audience_section, '-') || '|' || COALESCE(audience_parent_id::text, '-') || '|' || recipient_count FROM broadcasts ORDER BY id`,
		`SELECT id || '|' || COALESCE(parent_id::text, '-') || '|' || COALESCE(broadcast_id::text, '-') || '|' || title || '|' || body || '|' || COALESCE(is_read::text, '-') FROM notifications ORDER BY id`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			rows.Scan(&s)
			b.WriteString(s + "\n")
		}
		rows.Close()
		b.WriteString("--\n")
	}
	return b.String()
}

func j1ParentToken(t *testing.T, id int, phone string) map[string]string {
	t.Helper()
	return a14Bearer(e1Sign(t, jwt.MapClaims{"parent_id": id, "phone": phone, "role": "parent", "sv": 0, "exp": time.Now().Add(time.Hour).Unix()}))
}

// TestJ1Broadcasts verifies Task J1 through the real API: POST /api/admin/broadcasts
// validates the request and its audience, previews with dry_run, writes one broadcast and one
// unread notification per recipient parent, refuses an identical send within a minute, rolls
// back on failure, and GET /api/admin/broadcasts lists the sent log with read counts.
func TestJ1Broadcasts(t *testing.T) {
	srv, db := a14Server(t, "j1")
	j1Seed(t, db)
	admin := a14Bearer(a14AdminToken(t, srv))
	send := func(body any) a14Response {
		t.Helper()
		return a14Do(t, srv, "POST", "/api/admin/broadcasts", admin, body)
	}
	dryCount := func(t *testing.T, audience map[string]any) int {
		t.Helper()
		r := send(j1Body("Preview", "Preview body", audience, "dry_run", true))
		if r.status != 200 {
			t.Fatalf("dry run %v: %d %s", audience, r.status, r.body)
		}
		var resp struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Data    struct {
				ID             *int `json:"id"`
				RecipientCount int  `json:"recipient_count"`
				DryRun         bool `json:"dry_run"`
			} `json:"data"`
		}
		json.Unmarshal(r.body, &resp)
		if resp.Status != "success" || resp.Message != "Preview only" || resp.Data.ID != nil || !resp.Data.DryRun {
			t.Errorf("dry run envelope %s", r.body)
		}
		return resp.Data.RecipientCount
	}
	sent := func(t *testing.T, body any) (int, int) {
		t.Helper()
		r := send(body)
		if r.status != 200 {
			t.Fatalf("send: %d %s", r.status, r.body)
		}
		var resp struct {
			Message string `json:"message"`
			Data    struct {
				ID             *int `json:"id"`
				RecipientCount int  `json:"recipient_count"`
				DryRun         bool `json:"dry_run"`
			} `json:"data"`
		}
		json.Unmarshal(r.body, &resp)
		if resp.Message != "Broadcast sent" || resp.Data.ID == nil || resp.Data.DryRun {
			t.Fatalf("send envelope %s", r.body)
		}
		return *resp.Data.ID, resp.Data.RecipientCount
	}
	unread := func(t *testing.T, id int) int {
		t.Helper()
		r := a14Do(t, srv, "GET", "/api/mobile/notifications", j1ParentToken(t, id, fmt.Sprintf("+964700000%04d", id)), nil)
		if r.status != 200 {
			t.Fatalf("parent %d notifications: %d %s", id, r.status, r.body)
		}
		return int(r.json(t)["unread_count"].(float64))
	}
	parents := []int{3001, 3002, 3003, 3004, 3005, 3006}

	t.Run("invalid requests are 400 or 404 naming the problem and write nothing", func(t *testing.T) {
		before := j1Snapshot(t, db)
		ar := func(n int) string { return strings.Repeat("ع", n) }
		for _, tc := range []struct {
			name   string
			body   any
			status int
			msg    string
		}{
			{"title over 100", j1Body(ar(101), "Body", j1All()), 400, "title must be at most 100 characters"},
			{"body over 500", j1Body("Title", ar(501), j1All()), 400, "body must be at most 500 characters"},
			{"blank title", j1Body("   ", "Body", j1All()), 400, "title is required"},
			{"blank body", j1Body("Title", "\t\n", j1All()), 400, "body is required"},
			{"missing title", map[string]any{"body": "Body", "audience": j1All()}, 400, "title is required"},
			{"null body", map[string]any{"title": "T", "body": nil, "audience": j1All()}, 400, "body is required"},
			{"title not a string", map[string]any{"title": 7, "body": "Body", "audience": j1All()}, 400, "title must be a string"},
			{"NUL in title", j1Body("Ti\x00tle", "Body", j1All()), 400, "title must be text"},
			{"NUL in body", j1Body("Title", "Bo\x00dy", j1All()), 400, "body must be text"},
			{"invalid UTF-8 in title", `{"title":"bad ` + "\xff\xfe" + `","body":"Body","audience":{"type":"all"}}`, 400, "title must be text"},
			{"invalid UTF-8 in body", `{"title":"Title","body":"` + "\xc3\x28" + `","audience":{"type":"all"}}`, 400, "body must be text"},
			{"dry_run a string", j1Body("T", "B", j1All(), "dry_run", "true"), 400, "dry_run must be true or false"},
			{"dry_run a number", j1Body("T", "B", j1All(), "dry_run", 1), 400, "dry_run must be true or false"},
			{"no audience", map[string]any{"title": "T", "body": "B"}, 400, "audience is required"},
			{"no audience type", j1Body("T", "B", map[string]any{"grade": "G1"}), 400, "audience.type is required"},
			{"unknown type", j1Body("T", "B", map[string]any{"type": "everyone"}), 400, "audience.type must be all, parent or class"},
			{"type in another case", j1Body("T", "B", map[string]any{"type": "All"}), 400, "audience.type must be all, parent or class"},
			{"all with a grade", j1Body("T", "B", map[string]any{"type": "all", "grade": "G1"}), 400, "audience.grade, audience.section and audience.parent_phone must not be sent when audience.type is all"},
			{"all with a section", j1Body("T", "B", map[string]any{"type": "all", "section": "A"}), 400, "audience.grade, audience.section and audience.parent_phone must not be sent when audience.type is all"},
			{"all with a phone", j1Body("T", "B", map[string]any{"type": "all", "parent_phone": "07000003001"}), 400, "audience.grade, audience.section and audience.parent_phone must not be sent when audience.type is all"},
			{"all with an empty grade", j1Body("T", "B", map[string]any{"type": "all", "grade": ""}), 400, "audience.grade, audience.section and audience.parent_phone must not be sent when audience.type is all"},
			{"parent with a section", j1Body("T", "B", map[string]any{"type": "parent", "parent_phone": "07000003001", "section": "A"}), 400, "audience.grade and audience.section must not be sent when audience.type is parent"},
			{"parent without a phone", j1Body("T", "B", map[string]any{"type": "parent"}), 400, "audience.parent_phone is required when audience.type is parent"},
			{"parent with an invalid phone", j1Body("T", "B", j1Parent("12345")), 400, "audience." + handlers.InvalidParentPhoneMessage},
			{"parent with an unknown phone", j1Body("T", "B", j1Parent("07000003999")), 404, "No parent has this phone number"},
			{"unknown phone in a dry run", j1Body("T", "B", j1Parent("07000003999"), "dry_run", true), 404, "No parent has this phone number"},
			{"class with a phone", j1Body("T", "B", map[string]any{"type": "class", "grade": "G1", "parent_phone": "07000003001"}), 400, "audience.parent_phone must not be sent when audience.type is class"},
			{"class with neither", j1Body("T", "B", j1Class(nil, nil)), 400, "audience.grade or audience.section is required when audience.type is class"},
			{"class with null grade and section", j1Body("T", "B", map[string]any{"type": "class", "grade": nil, "section": nil}), 400, "audience.grade or audience.section is required when audience.type is class"},
			{"class with a blank grade", j1Body("T", "B", j1Class("  ", "A")), 400, "audience.grade must not be blank"},
			{"class with a grade over 50", j1Body("T", "B", j1Class(ar(51), nil)), 400, "audience.grade must be at most 50 characters"},
			{"class with a section over 50", j1Body("T", "B", j1Class(nil, ar(51))), 400, "audience.section must be at most 50 characters"},
			{"no parent matches", j1Body("T", "B", j1Class("G9", nil)), 400, "No parents match this audience"},
			{"no parent matches in a dry run", j1Body("T", "B", j1Class("G9", nil), "dry_run", true), 400, "No parents match this audience"},
			{"no such class", j1Body("T", "B", j1Class("G1", "Z")), 400, "No parents match this audience"},
			{"malformed JSON", "{", 400, "Invalid request body"},
			{"oversized body", `{"title":"` + strings.Repeat("a", int(handlers.MaxJSONBodyBytes)) + `"}`, 413, "Request body too large"},
		} {
			e1Error(t, send(tc.body), tc.status, tc.msg)
		}
		if after := j1Snapshot(t, db); after != before {
			t.Errorf("rejected requests wrote rows\nbefore:\n%s\nafter:\n%s", before, after)
		}
		for _, tc := range []struct {
			name        string
			title, body string
		}{{"title of 100 and body of 500", ar(100), ar(500)}, {"padded", "  " + ar(100) + "  ", "\n" + ar(500) + " "}} {
			if r := send(j1Body(tc.title, tc.body, j1All(), "dry_run", true)); r.status != 200 {
				t.Errorf("%s: %d %s", tc.name, r.status, r.body)
			}
		}
		if after := j1Snapshot(t, db); after != before {
			t.Errorf("dry runs wrote rows")
		}
	})

	t.Run("audiences reach the right parents", func(t *testing.T) {
		before := j1Snapshot(t, db)
		for _, tc := range []struct {
			name     string
			audience map[string]any
			want     int
		}{
			{"all parents, also without active children", j1All(), 6},
			{"all with null fields", map[string]any{"type": "all", "grade": nil, "section": nil, "parent_phone": nil}, 6},
			{"one parent, 07 form", j1Parent("07000003001"), 1},
			{"one parent, +964 form", j1Parent("+9647000003001"), 1},
			{"one parent, 00964 form", j1Parent("009647000003001"), 1},
			{"one parent, Arabic-Indic digits and spaces", j1Parent("٠٧٠٠ ٠٠٠ ٣٠٠١"), 1},
			{"one parent without children", j1Parent("07000003005"), 1},
			{"grade G1, every section", j1Class("G1", nil), 2},
			{"grade trimmed", j1Class("  G1 ", nil), 2},
			{"section A, every grade", j1Class(nil, "A"), 4},
			{"class G1/A, two children of one parent count once", j1Class("G1", "A"), 2},
			{"class G1/B", j1Class("G1", "B"), 1},
			{"case-sensitive grade", j1Class("g1", nil), 1},
		} {
			if got := dryCount(t, tc.audience); got != tc.want {
				t.Errorf("%s: %d recipients, want %d", tc.name, got, tc.want)
			}
		}
		if after := j1Snapshot(t, db); after != before {
			t.Errorf("dry runs wrote rows\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	var classID int
	t.Run("a send writes one unread notification per recipient", func(t *testing.T) {
		unreadBefore := map[int]int{}
		for _, p := range parents {
			unreadBefore[p] = unread(t, p)
		}
		id, n := 0, 0
		id, n = sent(t, j1Body(" اجتماع أولياء الأمور ", " يوم الخميس الساعة العاشرة. ", j1Class("G1", "A")))
		classID = id
		if n != 2 {
			t.Fatalf("recipient_count %d, want 2", n)
		}
		var a string
		db.QueryRow(`SELECT title || '|' || body || '|' || audience_type || '|' || audience_grade || '|' || audience_section || '|' || (audience_parent_id IS NULL) || '|' || recipient_count FROM broadcasts WHERE id = $1`, id).Scan(&a)
		if a != "اجتماع أولياء الأمور|يوم الخميس الساعة العاشرة.|class|G1|A|true|2" {
			t.Errorf("broadcast row %q", a)
		}
		rows, _ := db.Query(`SELECT parent_id, parent_phone, title, body, COALESCE(is_read, false) FROM notifications WHERE broadcast_id = $1 ORDER BY parent_id`, id)
		var got []string
		for rows.Next() {
			var pid int
			var phone, title, body string
			var read bool
			rows.Scan(&pid, &phone, &title, &body, &read)
			got = append(got, fmt.Sprintf("%d|%s|%s|%s|%v", pid, phone, title, body, read))
		}
		rows.Close()
		want := []string{
			"3001|+9647000003001|اجتماع أولياء الأمور|يوم الخميس الساعة العاشرة.|false",
			"3002|+9647000003002|اجتماع أولياء الأمور|يوم الخميس الساعة العاشرة.|false",
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("notifications\n got %v\nwant %v", got, want)
		}
		for _, p := range parents {
			wantUnread := unreadBefore[p]
			if p == 3001 || p == 3002 {
				wantUnread++
			}
			if u := unread(t, p); u != wantUnread {
				t.Errorf("parent %d unread_count %d, want %d", p, u, wantUnread)
			}
		}
		r := a14Do(t, srv, "GET", "/api/mobile/notifications", j1ParentToken(t, 3002, "+9647000003002"), nil)
		var list struct {
			Data       []map[string]any `json:"data"`
			HasMore    bool             `json:"has_more"`
			NextBefore *int             `json:"next_before"`
		}
		json.Unmarshal(r.body, &list)
		if len(list.Data) == 0 || list.Data[0]["title"] != "اجتماع أولياء الأمور" || list.Data[0]["is_read"] != false || len(list.Data[0]) != 5 {
			t.Errorf("parent list %s", r.body)
		}
		var keys []string
		for k := range r.json(t) {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != "data,has_more,next_before,status,unread_count" {
			t.Errorf("parent list keys %v", keys)
		}

		if _, n := sent(t, j1Body("عطلة", "لا دوام غداً.", j1All())); n != 6 {
			t.Errorf("all: %d recipients, want 6 (including parents without active children)", n)
		}
		if _, n := sent(t, j1Body("ملاحظة", "يرجى مراجعة الإدارة.", j1Parent("0700 000 3005"))); n != 1 {
			t.Errorf("one parent: %d recipients, want 1", n)
		}
		notify.SaveNotificationHistory(db, 3003, "+9647000003003", "إشعار حضور", "الطالب وصل")
		var nulls, total int
		db.QueryRow(`SELECT COUNT(*) FILTER (WHERE broadcast_id IS NULL), COUNT(*) FROM notifications WHERE title = 'إشعار حضور'`).Scan(&nulls, &total)
		if nulls != 1 || total != 1 {
			t.Errorf("a punch or absence notification has broadcast_id set: %d of %d NULL", nulls, total)
		}
	})

	t.Run("the same broadcast within a minute is refused", func(t *testing.T) {
		body := j1Body("تذكير", "إحضار الكتب.", j1Class(nil, "A"))
		if _, n := sent(t, body); n != 4 {
			t.Fatalf("first send: %d", n)
		}
		before := j1Snapshot(t, db)
		e1Error(t, send(body), 409, "The same broadcast was sent to this audience less than a minute ago")
		if after := j1Snapshot(t, db); after != before {
			t.Errorf("a refused duplicate wrote rows")
		}
		if r := send(j1Body("تذكير", "إحضار الكتب.", j1Class(nil, "A"), "dry_run", true)); r.status != 200 {
			t.Errorf("a dry run is never blocked: %d %s", r.status, r.body)
		}
		for _, other := range []map[string]any{
			j1Body("تذكير", "إحضار الدفاتر.", j1Class(nil, "A")),
			j1Body("تذكير", "إحضار الكتب.", j1Class("G2", "A")),
			j1Body("تذكير", "إحضار الكتب.", j1Class(nil, "B")),
		} {
			if r := send(other); r.status != 200 {
				t.Errorf("a different broadcast %v: %d %s", other, r.status, r.body)
			}
		}
		if _, err := db.Exec(`UPDATE broadcasts SET created_at = created_at - INTERVAL '61 seconds' WHERE title = 'تذكير'`); err != nil {
			t.Fatal(err)
		}
		if r := send(body); r.status != 200 {
			t.Errorf("after a minute the same broadcast must be allowed: %d %s", r.status, r.body)
		}

		concurrent := j1Body("تنبيه متزامن", "رسالة واحدة فقط.", j1All())
		var wg sync.WaitGroup
		start := make(chan struct{})
		statuses := make([]int, 20)
		for i := range statuses {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				status, _, err := e1Send(srv, "POST", "/api/admin/broadcasts", strings.TrimPrefix(admin["Authorization"], "Bearer "), concurrent)
				if err != nil {
					status = -1
				}
				statuses[i] = status
			}()
		}
		close(start)
		wg.Wait()
		count := map[int]int{}
		for _, s := range statuses {
			count[s]++
		}
		if count[200] != 1 || count[409] != 19 {
			t.Errorf("20 concurrent identical sends: %v, want one 200 and nineteen 409", count)
		}
		var rows, notes int
		db.QueryRow(`SELECT COUNT(*), (SELECT COUNT(*) FROM notifications n JOIN broadcasts a ON a.id = n.broadcast_id WHERE a.title = 'تنبيه متزامن') FROM broadcasts WHERE title = 'تنبيه متزامن'`).Scan(&rows, &notes)
		if rows != 1 || notes != 6 {
			t.Errorf("concurrent sends stored %d broadcasts and %d notifications, want 1 and 6", rows, notes)
		}
	})

	t.Run("a database failure writes nothing", func(t *testing.T) {
		before := j1Snapshot(t, db)
		if _, err := db.Exec(`ALTER TABLE notifications ADD CONSTRAINT j1_refuse_poison CHECK (body <> 'Poison')`); err != nil {
			t.Fatal(err)
		}
		defer db.Exec(`ALTER TABLE notifications DROP CONSTRAINT j1_refuse_poison`)
		e1Error(t, send(j1Body("Poison test", "Poison", j1All())), 500, "Failed to send broadcast")
		if after := j1Snapshot(t, db); after != before {
			t.Errorf("a failed send changed the tables\nbefore:\n%s\nafter:\n%s", before, after)
		}
		if srv.out.index("AdminBroadcastsHandler: send failed") < 0 {
			t.Errorf("the 500 was not logged")
		}
	})

	t.Run("the sent log lists broadcasts newest first with audiences and read counts", func(t *testing.T) {
		parentID, _ := sent(t, j1Body("لقاء خاص", "نرجو الحضور.", j1Parent("07000003005")))
		var nid int
		db.QueryRow(`SELECT id FROM notifications WHERE broadcast_id = $1 AND parent_id = 3001`, classID).Scan(&nid)
		if r := a14Do(t, srv, "PUT", fmt.Sprintf("/api/mobile/notifications/read?id=%d", nid), j1ParentToken(t, 3001, "+9647000003001"), nil); r.status != 200 {
			t.Fatalf("mark read: %d %s", r.status, r.body)
		}
		if _, err := db.Exec(`INSERT INTO broadcasts (title, body, audience_type, recipient_count, created_at)
			SELECT 'Old ' || g, 'Old body', 'all', 0, TIMESTAMP '2026-01-01 10:00' + g * INTERVAL '1 minute' FROM generate_series(1, 60) g`); err != nil {
			t.Fatal(err)
		}
		type item struct {
			ID       int    `json:"id"`
			Title    string `json:"title"`
			Body     string `json:"body"`
			Audience struct {
				Type        string  `json:"type"`
				Grade       *string `json:"grade"`
				Section     *string `json:"section"`
				ParentName  *string `json:"parent_name"`
				ParentPhone *string `json:"parent_phone"`
			} `json:"audience"`
			RecipientCount int    `json:"recipient_count"`
			ReadCount      int    `json:"read_count"`
			CreatedAt      string `json:"created_at"`
		}
		type page struct {
			Status     string `json:"status"`
			Data       []item `json:"data"`
			HasMore    bool   `json:"has_more"`
			NextBefore *int   `json:"next_before"`
		}
		get := func(query string) page {
			t.Helper()
			r := a14Do(t, srv, "GET", "/api/admin/broadcasts"+query, admin, nil)
			if r.status != 200 {
				t.Fatalf("log%s: %d %s", query, r.status, r.body)
			}
			var p page
			json.Unmarshal(r.body, &p)
			return p
		}
		var all []item
		for q := ""; ; {
			p := get(q)
			if len(p.Data) > 50 {
				t.Fatalf("page of %d", len(p.Data))
			}
			all = append(all, p.Data...)
			if !p.HasMore {
				if p.NextBefore != nil {
					t.Errorf("last page has next_before %d", *p.NextBefore)
				}
				break
			}
			if len(p.Data) != 50 || p.NextBefore == nil || *p.NextBefore != p.Data[49].ID {
				t.Fatalf("page: %d items, next_before %v", len(p.Data), p.NextBefore)
			}
			q = fmt.Sprintf("?before=%d", *p.NextBefore)
		}
		var total int
		db.QueryRow(`SELECT COUNT(*) FROM broadcasts`).Scan(&total)
		if len(all) != total {
			t.Errorf("log has %d items, the table %d", len(all), total)
		}
		for i := 1; i < len(all); i++ {
			if all[i].ID >= all[i-1].ID {
				t.Fatalf("not newest first at %d", i)
			}
		}
		byID := map[int]item{}
		for _, it := range all {
			byID[it.ID] = it
			if _, err := time.Parse(time.RFC3339, it.CreatedAt); err != nil || !strings.HasSuffix(it.CreatedAt, "+03:00") {
				t.Errorf("created_at %q", it.CreatedAt)
			}
			var want int
			db.QueryRow(`SELECT COUNT(*) FILTER (WHERE is_read) FROM notifications WHERE broadcast_id = $1`, it.ID).Scan(&want)
			if it.ReadCount != want {
				t.Errorf("broadcast %d read_count %d, want %d", it.ID, it.ReadCount, want)
			}
		}
		c := byID[classID]
		if c.ReadCount != 1 || c.RecipientCount != 2 || c.Audience.Type != "class" || c.Audience.Grade == nil || *c.Audience.Grade != "G1" || *c.Audience.Section != "A" || c.Audience.ParentName != nil || c.Audience.ParentPhone != nil {
			t.Errorf("class item %+v", c)
		}
		pi := byID[parentID]
		if pi.Audience.Type != "parent" || pi.Audience.ParentName == nil || *pi.Audience.ParentName != "J1 Parent Five" || *pi.Audience.ParentPhone != "+9647000003005" || pi.Audience.Grade != nil || pi.Audience.Section != nil {
			t.Errorf("parent item %+v", pi.Audience)
		}
		for _, it := range all {
			if it.Audience.Type == "all" && (it.Audience.Grade != nil || it.Audience.Section != nil || it.Audience.ParentName != nil || it.Audience.ParentPhone != nil) {
				t.Errorf("all item with audience fields %+v", it.Audience)
			}
		}
		if _, err := db.Exec(`DELETE FROM parents WHERE id = 3005`); err != nil {
			t.Fatal(err)
		}
		for _, it := range get(fmt.Sprintf("?before=%d", parentID+1)).Data {
			if it.ID == parentID && (it.Audience.Type != "parent" || it.Audience.ParentName != nil || it.Audience.ParentPhone != nil) {
				t.Errorf("after the parent is deleted: %+v", it.Audience)
			}
		}
		for _, bad := range []string{"abc", "0", "-1", "1.5", "99999999999999999999"} {
			e1Error(t, a14Do(t, srv, "GET", "/api/admin/broadcasts?before="+url.QueryEscape(bad), admin, nil), 400, "before must be a positive broadcast id")
		}
	})

	t.Run("each send is logged without its text", func(t *testing.T) {
		out := srv.out.String()
		if !strings.Contains(out, `"msg":"Broadcast sent"`) || strings.Contains(out, "اجتماع أولياء الأمور") || strings.Contains(out, "إحضار الكتب") {
			t.Errorf("send logs must exist and must not contain the text")
		}
	})

	t.Run("authentication", func(t *testing.T) {
		later, earlier := time.Now().Add(time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
		expired := e1Sign(t, jwt.MapClaims{"username": "admin", "role": "admin", "sv": 0, "exp": earlier})
		parent := e1Sign(t, jwt.MapClaims{"parent_id": 3001, "phone": "+9647000003001", "role": "parent", "sv": 0, "exp": later})
		body := j1Body("Auth", "Auth body", j1All())
		before := j1Snapshot(t, db)
		calls := []string{"POST", "GET"}
		for _, m := range calls {
			for _, tc := range []struct {
				name   string
				header map[string]string
				status int
				msg    string
			}{
				{"no token", nil, 401, "Unauthorized"},
				{"malformed token", a14Bearer("not-a-jwt"), 401, "Unauthorized"},
				{"wrong scheme", map[string]string{"Authorization": "Basic x"}, 401, "Unauthorized"},
				{"expired token", a14Bearer(expired), 401, "Unauthorized"},
				{"parent token", a14Bearer(parent), 403, "Forbidden"},
			} {
				r := a14Do(t, srv, m, "/api/admin/broadcasts", tc.header, body)
				if r.status != tc.status || r.json(t)["message"] != tc.msg {
					t.Errorf("%s with %s: %d %s", m, tc.name, r.status, r.body)
				}
			}
		}
		script, err := os.ReadFile(filepath.Join("..", "..", "db", "scripts", "rotate_admin_password.sql"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(strings.ReplaceAll(string(script), "NEW_PASSWORD_HERE", "j1-rotated-password")); err != nil {
			t.Fatal(err)
		}
		for _, m := range calls {
			if r := a14Do(t, srv, m, "/api/admin/broadcasts", admin, body); r.status != 401 {
				t.Errorf("%s with a token from before the rotation: %d %s", m, r.status, r.body)
			}
		}
		if after := j1Snapshot(t, db); after != before {
			t.Errorf("rejected tokens wrote rows")
		}
		if r := a14Do(t, srv, "DELETE", "/api/admin/broadcasts", nil, nil); r.status != 405 || r.header.Get("Allow") != "GET, HEAD, POST" {
			t.Errorf("DELETE: %d Allow=%q", r.status, r.header.Get("Allow"))
		}
	})
}

// j1Counter counts the statements a database handle sends.
type j1Counter struct{ n atomic.Int64 }

type j1Connector struct {
	dsn string
	c   *j1Counter
}

func (k j1Connector) Connect(context.Context) (driver.Conn, error) {
	conn, err := stdlib.GetDefaultDriver().Open(k.dsn)
	if err != nil {
		return nil, err
	}
	return &j1Conn{Conn: conn, c: k.c}, nil
}

func (k j1Connector) Driver() driver.Driver { return stdlib.GetDefaultDriver() }

type j1Conn struct {
	driver.Conn
	c *j1Counter
}

func (c *j1Conn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.c.n.Add(1)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}

func (c *j1Conn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.c.n.Add(1)
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}

func (c *j1Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *j1Conn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, q)
}

func (c *j1Conn) CheckNamedValue(nv *driver.NamedValue) error {
	return c.Conn.(driver.NamedValueChecker).CheckNamedValue(nv)
}

func (c *j1Conn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *j1Conn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

// j1InProcess returns an app on a throwaway database whose statements are counted, an admin
// token, and the counter.
func j1InProcess(t *testing.T, label string) (*handlers.AppEnv, *sql.DB, string, *j1Counter) {
	t.Helper()
	db, dsn := setupThrowawayDB(t, label)
	counter := &j1Counter{}
	counted := sql.OpenDB(j1Connector{dsn: dsn, c: counter})
	t.Cleanup(func() { counted.Close() })
	auth.InitAuth("j1-test-secret")
	token, _ := auth.GenerateAdminToken("admin", 0)
	return &handlers.AppEnv{DB: counted, Background: &background.Group{}}, db, token, counter
}

func j1Post(t *testing.T, app *handlers.AppEnv, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	return serve(t, app.AdminMiddleware(app.AdminBroadcastsHandler), "POST", "/api/admin/broadcasts", token, string(raw))
}

// TestJ1ScaleAndQueries checks a send to 2,000 parents is one bulk insert finished within a few
// seconds, using as many statements as a send to three, and that the sent log uses two queries
// whatever the page size.
func TestJ1ScaleAndQueries(t *testing.T) {
	app, db, token, counter := j1InProcess(t, "j1scale")
	if _, err := db.Exec(`INSERT INTO parents (full_name, phone_number, pin_code) SELECT 'J1 Bulk ' || g, '+96470000' || lpad(g::text, 5, '0'), 'x' FROM generate_series(10001, 12000) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO students (full_name, rfid_tag, parent_id, grade, section) SELECT 'J1 Small ' || id, 'J1-S-' || id, id, 'S1', 'A' FROM parents ORDER BY id LIMIT 3`); err != nil {
		t.Fatal(err)
	}
	statements := func(body any) (int64, time.Duration, *httptest.ResponseRecorder) {
		before := counter.n.Load()
		start := time.Now()
		rec := j1Post(t, app, token, body)
		return counter.n.Load() - before, time.Since(start), rec
	}
	small, _, rec := statements(j1Body("Small", "Three parents", j1Class("S1", "A")))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"recipient_count":3`) {
		t.Fatalf("small send: %d %s", rec.Code, rec.Body.String())
	}
	big, took, rec := statements(j1Body("Big", "Two thousand parents", j1All()))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"recipient_count":2000`) {
		t.Fatalf("big send: %d %s", rec.Code, rec.Body.String())
	}
	if big != small {
		t.Errorf("a send to 2000 parents used %d statements, a send to 3 used %d; want the same (one bulk insert)", big, small)
	}
	if took > 3*time.Second {
		t.Errorf("a send to 2000 parents took %v", took)
	}
	var n int
	db.QueryRow(`SELECT COUNT(DISTINCT parent_id) FROM notifications n JOIN broadcasts a ON a.id = n.broadcast_id WHERE a.title = 'Big'`).Scan(&n)
	if n != 2000 {
		t.Errorf("%d notification rows, want 2000", n)
	}

	list := app.AdminMiddleware(app.AdminBroadcastsHandler)
	for _, size := range []int{1, 10, 50} {
		if _, err := db.Exec(`DELETE FROM broadcasts`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO broadcasts (title, body, audience_type, recipient_count) SELECT 'Log ' || g, 'b', 'all', 0 FROM generate_series(1, $1) g`, size); err != nil {
			t.Fatal(err)
		}
		before := counter.n.Load()
		rec := serve(t, list, "GET", "/api/admin/broadcasts", token, "")
		used := counter.n.Load() - before
		if rec.Code != 200 || strings.Count(rec.Body.String(), `"read_count"`) != size {
			t.Fatalf("log of %d: %d %s", size, rec.Code, rec.Body.String())
		}
		if used != 3 {
			t.Errorf("a page of %d used %d statements, want 3 (session check, page, read counts)", size, used)
		}
	}
}

type j1FakeFCM struct {
	*b1FakeFCM
	delay time.Duration
}

// newJ1SlowFCM answers like b1FakeFCM after delay, or stops when the request is cancelled.
func newJ1SlowFCM(t *testing.T, delay time.Duration) *j1FakeFCM {
	t.Helper()
	f := &j1FakeFCM{b1FakeFCM: &b1FakeFCM{}, delay: delay}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			return
		}
		var req struct {
			Message struct {
				Token string `json:"token"`
			} `json:"message"`
		}
		json.Unmarshal(body, &req)
		f.mu.Lock()
		f.sent = append(f.sent, req.Message.Token)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"name":"projects/fk-test/messages/1"}`)
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// TestJ1Push verifies the post-commit push: every device of every recipient, nothing else, in
// batches of at most 500, dead tokens deleted alone, no Firebase client tolerated, the response
// never waits for FCM, shutdown waits for the push, a stuck batch ends at PushBatchTimeout, and
// no log line holds a token.
func TestJ1Push(t *testing.T) {
	capture := &w10Capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	app, db, token, _ := j1InProcess(t, "j1push")
	fcm := newB1FakeFCM(t)
	app.FCMClient = fcm.client(t)
	settle := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if pending, err := app.Background.Wait(ctx); err != nil {
			t.Fatalf("background work still running: %d", pending)
		}
	}
	for _, q := range []string{
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (3101, 'Push A', '+9647000003101', 'x'), (3102, 'Push B', '+9647000003102', 'x'), (3103, 'Push C', '+9647000003103', 'x')`,
		`INSERT INTO students (full_name, rfid_tag, parent_id, grade, section) VALUES ('Push Kid A', 'J1-PA', 3101, 'P1', 'A'), ('Push Kid B', 'J1-PB', 3102, 'P1', 'A'), ('Push Kid C', 'J1-PC', 3103, 'P2', 'A')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	a1, a2, dead, bad, other := b1Token("j1-a1"), b1Token("j1-a2"), b1Token("dead-j1"), b1Token("bad-j1"), b1Token("j1-other")
	if _, err := db.Exec(`INSERT INTO device_tokens (parent_id, token) VALUES (3101, $1), (3101, $2), (3102, $3), (3102, $4), (3103, $5)`, a1, a2, dead, bad, other); err != nil {
		t.Fatal(err)
	}
	tokensLeft := func() string {
		rows, _ := db.Query(`SELECT token FROM device_tokens ORDER BY token`)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			rows.Scan(&s)
			out = append(out, s)
		}
		return strings.Join(out, ",")
	}

	t.Run("every device of every recipient, and only those", func(t *testing.T) {
		if rec := j1Post(t, app, token, j1Body("Push", "To class P1/A", j1Class("P1", "A"))); rec.Code != 200 {
			t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
		}
		settle()
		want := []string{a1, a2, bad, dead}
		sort.Strings(want)
		if got := fcm.take(); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("FCM was asked for %v, want %v", got, want)
		}
		wantLeft := []string{a1, a2, bad, other}
		sort.Strings(wantLeft)
		if got := tokensLeft(); got != strings.Join(wantLeft, ",") {
			t.Errorf("tokens left %s; only the unregistered one may be deleted", got)
		}
	})

	t.Run("1,200 devices go out in batches of at most 500", func(t *testing.T) {
		if _, err := db.Exec(`INSERT INTO parents (id, full_name, phone_number, pin_code) SELECT g, 'Many ' || g, '+96470000' || lpad(g::text, 5, '0'), 'x' FROM generate_series(20001, 20120) g;
			INSERT INTO device_tokens (parent_id, token) SELECT p, 'test-device-many-' || p || '-' || k || '-000000000000' FROM generate_series(20001, 20120) p, generate_series(1, 10) k`); err != nil {
			t.Fatal(err)
		}
		marker := len(capture.snapshot())
		if rec := j1Post(t, app, token, j1Body("Many", "Many devices", j1All())); rec.Code != 200 {
			t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
		}
		settle()
		sentTo := fcm.take()
		var batches []string
		total := 0
		for _, l := range capture.snapshot()[marker:] {
			if l.Msg == "Broadcast push batch" {
				batches = append(batches, l.Attrs["batch_size"])
				var n int
				fmt.Sscan(l.Attrs["batch_size"], &n)
				if n > notify.PushBatchSize {
					t.Errorf("a batch of %d tokens", n)
				}
				total += n
			}
		}
		if len(sentTo) != total || total != 1200+4 {
			t.Errorf("sent %d tokens in batches %v (total %d), want 1204", len(sentTo), batches, total)
		}
		if strings.Join(batches, ",") != "500,500,204" {
			t.Errorf("batches %v, want 500,500,204", batches)
		}
	})

	t.Run("no Firebase client still sends the broadcast", func(t *testing.T) {
		noFCM := &handlers.AppEnv{DB: app.DB, Background: app.Background}
		marker := len(capture.snapshot())
		rec := j1Post(t, noFCM, token, j1Body("No FCM", "Rows only", j1Class("P2", "A")))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"recipient_count":1`) {
			t.Fatalf("send without FCM: %d %s", rec.Code, rec.Body.String())
		}
		settle()
		skipped := 0
		for _, l := range capture.snapshot()[marker:] {
			if l.Msg == "Broadcast push skipped: Firebase is not configured" && l.Level == slog.LevelInfo {
				skipped++
			}
		}
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM notifications n JOIN broadcasts a ON a.id = n.broadcast_id WHERE a.title = 'No FCM'`).Scan(&n)
		if skipped != 1 || n != 1 || len(fcm.take()) != 0 {
			t.Errorf("skip lines %d, notification rows %d", skipped, n)
		}
	})

	t.Run("a slow FCM never delays the response, and shutdown waits for the push", func(t *testing.T) {
		slow := newJ1SlowFCM(t, 2*time.Second)
		slowApp := &handlers.AppEnv{DB: app.DB, Background: &background.Group{}, FCMClient: slow.b1FakeFCM.client(t)}
		start := time.Now()
		rec := j1Post(t, slowApp, token, j1Body("Slow", "Slow FCM", j1Class("P2", "A")))
		if took := time.Since(start); rec.Code != 200 || took > time.Second {
			t.Fatalf("response took %v (%d)", took, rec.Code)
		}
		if slowApp.Background.Running() == 0 {
			t.Fatalf("the push is not tracked by the background group")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := slowApp.Background.Wait(ctx); err != nil {
			t.Fatalf("wait: %v", err)
		}
		if got := slow.take(); len(got) != 1 || got[0] != other {
			t.Errorf("slow FCM received %v", got)
		}
		if waited := time.Since(start); waited < 2*time.Second {
			t.Errorf("Wait returned after %v, before the push finished", waited)
		}
	})

	t.Run("a stuck batch ends at PushBatchTimeout, inside the shutdown window", func(t *testing.T) {
		if notify.PushBatchTimeout != 20*time.Second || notify.PushBatchTimeout >= config.DefaultShutdownTimeout {
			t.Fatalf("PushBatchTimeout %v, want 20s and below the %v shutdown window", notify.PushBatchTimeout, config.DefaultShutdownTimeout)
		}
		prevTimeout := notify.PushBatchTimeout
		notify.PushBatchTimeout = 300 * time.Millisecond
		defer func() { notify.PushBatchTimeout = prevTimeout }()
		stuck := newJ1SlowFCM(t, time.Hour)
		stuckApp := &handlers.AppEnv{DB: app.DB, Background: &background.Group{}, FCMClient: stuck.b1FakeFCM.client(t)}
		marker := len(capture.snapshot())
		if rec := j1Post(t, stuckApp, token, j1Body("Stuck", "Stuck FCM", j1Class("P2", "A"))); rec.Code != 200 {
			t.Fatalf("send: %d", rec.Code)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if pending, err := stuckApp.Background.Wait(ctx); err != nil {
			t.Fatalf("a stuck batch kept %d pushes running past its timeout", pending)
		}
		failed := false
		for _, l := range capture.snapshot()[marker:] {
			if l.Msg == "Broadcast push finished" && l.Attrs["failed"] == "1" && l.Attrs["sent"] == "0" {
				failed = true
			}
		}
		if !failed {
			t.Errorf("the timed-out push was not reported as failed")
		}
	})

	t.Run("no log line holds a full token", func(t *testing.T) {
		rows, _ := db.Query(`SELECT token FROM device_tokens UNION SELECT $1 UNION SELECT $2`, dead, bad)
		var tokens []string
		for rows.Next() {
			var s string
			rows.Scan(&s)
			tokens = append(tokens, s)
		}
		rows.Close()
		for _, l := range capture.snapshot() {
			line := l.Msg + fmt.Sprint(l.Attrs)
			for _, tok := range tokens {
				if strings.Contains(line, tok) {
					t.Fatalf("log %q contains a token", line)
				}
			}
		}
	})
}

// TestJ1BeforePhoneNormalisation runs the new code on a database at 000026 without 000027, the
// state Staging is in between the steps of the README deploy notes.
func TestJ1BeforePhoneNormalisation(t *testing.T) {
	db, dsn := setupThrowawayDB(t, "j1v25")
	if _, err := db.Exec(a14Migration(t, "000027_normalize_phone_numbers.down.sql")); err != nil {
		t.Fatal(err)
	}
	j1Seed(t, db)
	hash, _ := bcrypt.GenerateFromPassword([]byte(a14AdminPassword), bcrypt.DefaultCost)
	if _, err := db.Exec(`UPDATE admins SET password_hash = $1 WHERE username = 'admin'`, string(hash)); err != nil {
		t.Fatal(err)
	}
	srv := g3Start(t, dsn, g3FreePort(t), true, "TRUSTED_PROXY_CIDRS=127.0.0.1/32", "ABSENCE_CRON_SCHEDULE=0 0 1 1 *")
	admin := a14Bearer(a14AdminToken(t, srv))
	for _, body := range []map[string]any{
		j1Body("Before 000027", "Class", j1Class("G1", nil)),
		j1Body("Before 000027", "One parent", j1Parent("07000003003")),
	} {
		if r := a14Do(t, srv, "POST", "/api/admin/broadcasts", admin, body); r.status != 200 {
			t.Errorf("send on 000026: %d %s", r.status, r.body)
		}
	}
	if r := a14Do(t, srv, "GET", "/api/admin/broadcasts", admin, nil); r.status != 200 || strings.Count(string(r.body), `"id"`) != 2 {
		t.Errorf("log on 000026: %d %s", r.status, r.body)
	}
}

// TestJ1BroadcastsMigration verifies the broadcast migrations: 000025 (applied on Staging, under
// its original names) applies alone on a database at 000024, rolls back keeping every
// notification and applies again; 000026 renames everything to broadcasts keeping rows, links,
// read state, constraints and the sequence, also from a partly renamed state, and rolls back to
// the original names; golang-migrate reaches 26 from versions 20 to 25; and phone normalisation
// (now 000027) still applies last, collision abort included.
func TestJ1BroadcastsMigration(t *testing.T) {
	migrate, err := exec.LookPath("migrate")
	if err != nil {
		t.Skip("golang-migrate CLI not installed")
	}
	fresh := func(t *testing.T, label string) (*sql.DB, func(args ...string) (string, error)) {
		t.Helper()
		cli, cliDSN := setupThrowawayDB(t, label)
		for _, tbl := range []string{"broadcasts", "announcements", "banner_images", "device_tokens", "settings", "notifications", "weekly_schedules", "student_leaves", "banners", "admins", "attendance_logs", "devices", "students", "parents"} {
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
	oldState := func(db *sql.DB) (table, column, phoneColumns bool) {
		db.QueryRow(`SELECT to_regclass('announcements') IS NOT NULL,
			EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'notifications' AND column_name = 'announcement_id'),
			EXISTS (SELECT 1 FROM information_schema.columns WHERE column_name = 'phone_number_original')`).Scan(&table, &column, &phoneColumns)
		return
	}
	// names lists every table, sequence, column, constraint and index whose name holds word.
	names := func(t *testing.T, db *sql.DB, word string) []string {
		t.Helper()
		rows, err := db.Query(`
			SELECT 'table ' || relname FROM pg_class WHERE relkind = 'r' AND relname LIKE '%' || $1 || '%'
			UNION ALL SELECT 'sequence ' || relname FROM pg_class WHERE relkind = 'S' AND relname LIKE '%' || $1 || '%'
			UNION ALL SELECT 'index ' || relname FROM pg_class WHERE relkind = 'i' AND relname LIKE '%' || $1 || '%'
			UNION ALL SELECT 'column ' || table_name || '.' || column_name FROM information_schema.columns WHERE table_schema = 'public' AND column_name LIKE '%' || $1 || '%'
			UNION ALL SELECT 'constraint ' || conname FROM pg_constraint WHERE conname LIKE '%' || $1 || '%'
			ORDER BY 1`, word)
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
		sort.Strings(out)
		return out
	}
	renamed := func(list []string, from, to string) []string {
		out := make([]string, len(list))
		for i, s := range list {
			out[i] = strings.ReplaceAll(s, from, to)
		}
		sort.Strings(out)
		return out
	}

	t.Run("000024 to 000025 adds only announcements; down keeps notifications; up again", func(t *testing.T) {
		db, run := fresh(t, "j1m24")
		if out, err := run("goto", "24"); err != nil {
			t.Fatalf("goto 24: %v\n%s", err, out)
		}
		if table, column, _ := oldState(db); table || column {
			t.Fatalf("announcements exist at 24")
		}
		if _, err := db.Exec(`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (3201, 'M', '+9647000003201', 'x');
			INSERT INTO notifications (parent_id, parent_phone, title, body) VALUES (3201, '+9647000003201', 'Old', 'Old body')`); err != nil {
			t.Fatal(err)
		}
		if out, err := run("goto", "25"); err != nil {
			t.Fatalf("goto 25: %v\n%s", err, out)
		}
		if v, _ := run("version"); v != "25" {
			t.Errorf("version %q, want 25", v)
		}
		if table, column, phone := oldState(db); !table || !column || phone {
			t.Fatalf("at 25: table %v, column %v, phone columns %v", table, column, phone)
		}
		var oldLink sql.NullInt64
		db.QueryRow(`SELECT announcement_id FROM notifications WHERE title = 'Old'`).Scan(&oldLink)
		if oldLink.Valid {
			t.Errorf("an existing notification got an announcement_id")
		}
		if _, err := db.Exec(`INSERT INTO notifications (parent_phone, title, body) VALUES ('+9647000003201', 'Old code', 'Insert without the new column')`); err != nil {
			t.Errorf("the old code's insert fails on 25: %v", err)
		}
		for _, bad := range []string{
			`INSERT INTO announcements (title, body, audience_type, audience_grade, recipient_count) VALUES ('t', 'b', 'all', 'G1', 0)`,
			`INSERT INTO announcements (title, body, audience_type, audience_section, recipient_count) VALUES ('t', 'b', 'parent', 'A', 0)`,
			`INSERT INTO announcements (title, body, audience_type, audience_parent_id, audience_grade, recipient_count) VALUES ('t', 'b', 'class', 3201, 'G1', 0)`,
			`INSERT INTO announcements (title, body, audience_type, recipient_count) VALUES ('t', 'b', 'class', 0)`,
			`INSERT INTO announcements (title, body, audience_type, recipient_count) VALUES ('t', 'b', 'everyone', 0)`,
			`INSERT INTO announcements (title, body, audience_type, recipient_count) VALUES ('', 'b', 'all', 0)`,
			`INSERT INTO announcements (title, body, audience_type, recipient_count) VALUES (repeat('t', 101), 'b', 'all', 0)`,
			`INSERT INTO announcements (title, body, audience_type, recipient_count) VALUES ('t', repeat('b', 501), 'all', 0)`,
			`INSERT INTO announcements (title, body, audience_type, audience_grade, recipient_count) VALUES ('t', 'b', 'class', repeat('g', 51), 0)`,
			`INSERT INTO announcements (title, body, audience_type, recipient_count) VALUES ('t', 'b', 'all', -1)`,
		} {
			if _, err := db.Exec(bad); err == nil {
				t.Errorf("accepted: %s", bad)
			}
		}
		if _, err := db.Exec(`INSERT INTO announcements (id, title, body, audience_type, audience_parent_id, recipient_count) VALUES (1, 't', 'b', 'parent', 3201, 1);
			UPDATE notifications SET announcement_id = 1 WHERE title = 'Old'`); err != nil {
			t.Fatal(err)
		}
		var before int
		db.QueryRow(`SELECT COUNT(*) FROM notifications`).Scan(&before)
		if out, err := run("down", "1"); err != nil {
			t.Fatalf("down 1: %v\n%s", err, out)
		}
		var after int
		db.QueryRow(`SELECT COUNT(*) FROM notifications`).Scan(&after)
		if table, column, _ := oldState(db); table || column || after != before {
			t.Errorf("after down: table %v, column %v, notifications %d of %d", table, column, after, before)
		}
		if out, err := run("up", "1"); err != nil {
			t.Fatalf("up 1: %v\n%s", err, out)
		}
		if table, column, phone := oldState(db); !table || !column || phone {
			t.Errorf("after up again: table %v, column %v, phone %v", table, column, phone)
		}
	})

	t.Run("000025 to 000026 renames everything to broadcasts and keeps the data; down restores the old names", func(t *testing.T) {
		db, run := fresh(t, "j1m25")
		if out, err := run("goto", "25"); err != nil {
			t.Fatalf("goto 25: %v\n%s", err, out)
		}
		oldNames := names(t, db, "announcement")
		if len(oldNames) != 15 {
			t.Fatalf("at 25: %d announcement names, want 15 (table, sequence, column, 2 indexes, 10 constraints): %v", len(oldNames), oldNames)
		}
		var sent, read, plain int
		if err := db.QueryRow(`INSERT INTO parents (full_name, phone_number, pin_code) VALUES ('Rename', '+9647000003301', 'x') RETURNING id`).Scan(&plain); err != nil {
			t.Fatal(err)
		}
		parent := plain
		if err := db.QueryRow(`INSERT INTO announcements (title, body, audience_type, audience_parent_id, recipient_count) VALUES ('Staging test', 'Body', 'parent', $1, 1) RETURNING id`, parent).Scan(&sent); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`INSERT INTO notifications (parent_id, parent_phone, title, body, announcement_id, is_read) VALUES ($1, '+9647000003301', 'Staging test', 'Body', $2, true) RETURNING id`, parent, sent).Scan(&read); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`INSERT INTO notifications (parent_id, parent_phone, title, body) VALUES ($1, '+9647000003301', 'Punch', 'Arrived') RETURNING id`, parent).Scan(&plain); err != nil {
			t.Fatal(err)
		}
		snapshot := func(table, column string) string {
			var s string
			db.QueryRow(`SELECT (SELECT string_agg(id || '|' || title || '|' || audience_type || '|' || COALESCE(audience_parent_id::text, '-') || '|' || recipient_count || '|' || created_at, ',' ORDER BY id) FROM ` + table + `)
				|| ' / ' || (SELECT string_agg(id || '|' || COALESCE(` + column + `::text, '-') || '|' || COALESCE(is_read::text, '-') || '|' || title, ',' ORDER BY id) FROM notifications)`).Scan(&s)
			return s
		}
		before := snapshot("announcements", "announcement_id")

		if out, err := run("up", "1"); err != nil {
			t.Fatalf("up 1: %v\n%s", err, out)
		}
		if v, _ := run("version"); v != "26" {
			t.Errorf("version %q, want 26", v)
		}
		if left := names(t, db, "announcement"); len(left) != 0 {
			t.Errorf("old names left after 000026: %v", left)
		}
		if got, want := names(t, db, "broadcast"), renamed(oldNames, "announcement", "broadcast"); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("names after 000026\n got %v\nwant %v", got, want)
		}
		if after := snapshot("broadcasts", "broadcast_id"); after != before {
			t.Errorf("000026 changed data\nbefore %s\nafter  %s", before, after)
		}
		var link sql.NullInt64
		var isRead bool
		db.QueryRow(`SELECT broadcast_id, is_read FROM notifications WHERE id = $1`, read).Scan(&link, &isRead)
		if !link.Valid || int(link.Int64) != sent || !isRead {
			t.Errorf("the linked read notification became %v read=%v", link, isRead)
		}
		db.QueryRow(`SELECT broadcast_id FROM notifications WHERE id = $1`, plain).Scan(&link)
		if link.Valid {
			t.Errorf("the plain notification got broadcast_id %d", link.Int64)
		}
		var indexDef string
		db.QueryRow(`SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_notifications_broadcast_id'`).Scan(&indexDef)
		if !strings.Contains(indexDef, "(broadcast_id)") || !strings.Contains(indexDef, "WHERE (broadcast_id IS NOT NULL)") {
			t.Errorf("partial index %q", indexDef)
		}
		if _, err := db.Exec(`INSERT INTO notifications (parent_id, parent_phone, title, body, broadcast_id) VALUES ($1, '+9647000003301', 'x', 'y', 999999)`, parent); err == nil {
			t.Errorf("the foreign key to broadcasts is gone")
		}
		var next, max int
		if err := db.QueryRow(`INSERT INTO broadcasts (title, body, audience_type, recipient_count) VALUES ('Next', 'b', 'all', 0) RETURNING id`).Scan(&next); err != nil {
			t.Fatal(err)
		}
		db.QueryRow(`SELECT MAX(id) FROM broadcasts WHERE id <> $1`, next).Scan(&max)
		if next != max+1 {
			t.Errorf("the sequence gave %d after %d", next, max)
		}
		if _, err := db.Exec(`DELETE FROM broadcasts WHERE id = $1`, next); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO broadcasts (title, body, audience_type, audience_grade, recipient_count) VALUES ('t', 'b', 'all', 'G1', 0)`); err == nil {
			t.Errorf("the audience CHECK is gone")
		}
		if _, err := db.Exec(`INSERT INTO notifications (parent_phone, title, body) VALUES ('+9647000003301', 'Old code', 'Punch insert')`); err != nil {
			t.Errorf("a punch or absence insert fails on 26: %v", err)
		}
		if _, err := db.Exec(`DELETE FROM notifications WHERE title = 'Old code'`); err != nil {
			t.Fatal(err)
		}

		if out, err := run("down", "1"); err != nil {
			t.Fatalf("down 1: %v\n%s", err, out)
		}
		if left := names(t, db, "broadcast"); len(left) != 0 {
			t.Errorf("new names left after down: %v", left)
		}
		if got := names(t, db, "announcement"); strings.Join(got, ",") != strings.Join(oldNames, ",") {
			t.Errorf("names after down\n got %v\nwant %v", got, oldNames)
		}
		if after := snapshot("announcements", "announcement_id"); after != before {
			t.Errorf("down changed data\nbefore %s\nafter  %s", before, after)
		}
		if out, err := run("up", "1"); err != nil {
			t.Fatalf("up again: %v\n%s", err, out)
		}
		if after := snapshot("broadcasts", "broadcast_id"); after != before || len(names(t, db, "announcement")) != 0 {
			t.Errorf("after up again: %s", after)
		}
	})

	t.Run("000026 finishes a partly renamed database", func(t *testing.T) {
		db, run := fresh(t, "j1mpart")
		if out, err := run("goto", "25"); err != nil {
			t.Fatalf("goto 25: %v\n%s", err, out)
		}
		oldNames := names(t, db, "announcement")
		if _, err := db.Exec(`ALTER TABLE announcements RENAME TO broadcasts; ALTER TABLE notifications RENAME COLUMN announcement_id TO broadcast_id`); err != nil {
			t.Fatal(err)
		}
		if out, err := run("up", "1"); err != nil {
			t.Fatalf("up 1 on a partly renamed database: %v\n%s", err, out)
		}
		if left := names(t, db, "announcement"); len(left) != 0 {
			t.Errorf("old names left: %v", left)
		}
		if got, want := names(t, db, "broadcast"), renamed(oldNames, "announcement", "broadcast"); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("names\n got %v\nwant %v", got, want)
		}
	})

	for _, from := range []string{"20", "21", "22", "23", "24", "25"} {
		t.Run("golang-migrate from version "+from+" through 26 to 27", func(t *testing.T) {
			db, run := fresh(t, "j1v"+from)
			if out, err := run("goto", from); err != nil {
				t.Fatalf("goto %s: %v\n%s", from, err, out)
			}
			if out, err := run("goto", "26"); err != nil {
				t.Fatalf("goto 26: %v\n%s", err, out)
			}
			if old := names(t, db, "announcement"); len(old) != 0 || len(names(t, db, "broadcast")) != 15 {
				t.Errorf("at 26: old names %v, %d broadcast names", old, len(names(t, db, "broadcast")))
			}
			if _, _, phone := oldState(db); phone {
				t.Errorf("phone normalisation ran before 27")
			}
			if out, err := run("up"); err != nil {
				t.Fatalf("up: %v\n%s", err, out)
			}
			if v, _ := run("version"); v != "27" {
				t.Errorf("version %q, want 27", v)
			}
			if _, _, phone := oldState(db); !phone {
				t.Errorf("at 27: phone columns missing")
			}
		})
	}

	t.Run("000027 still aborts on collisions after 000026", func(t *testing.T) {
		db, run := fresh(t, "j1coll")
		if out, err := run("goto", "26"); err != nil {
			t.Fatalf("goto 26: %v\n%s", err, out)
		}
		if _, err := db.Exec(`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (981, 'A', '07000000981', 'x'), (982, 'B', '+9647000000981', 'x')`); err != nil {
			t.Fatal(err)
		}
		out, err := run("up")
		if err == nil || !strings.Contains(out, "[981, 982]") {
			t.Fatalf("000027 should abort listing [981, 982]: %v\n%s", err, out)
		}
		if _, _, phone := oldState(db); phone || len(names(t, db, "broadcast")) != 15 {
			t.Errorf("after the aborted 000027: phone columns %v, broadcast names %d", phone, len(names(t, db, "broadcast")))
		}
	})
}
