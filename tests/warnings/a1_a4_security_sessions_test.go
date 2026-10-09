package warnings

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"future_kids/internal/clientip"
	"future_kids/internal/config"
	"future_kids/internal/phone"
	"future_kids/internal/ratelimit"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	a14AdminPassword = "a14-admin-password"
	a14WrongPassword = "a14-wrong-password"
)

type a14Response struct {
	status int
	header http.Header
	body   []byte
	took   time.Duration
}

func (r a14Response) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("response is not JSON (%d %s): %s", r.status, r.header.Get("Content-Type"), r.body)
	}
	return m
}

func a14Do(t *testing.T, srv *g3Server, method, path string, header map[string]string, body any) a14Response {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, srv.url(path), rd)
	if err != nil {
		t.Fatal(err)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return a14Response{resp.StatusCode, resp.Header, raw, time.Since(start)}
}

func a14Bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// a14Server starts the real API (G3_API_BINARY selects another build) with an admin whose
// password is a14AdminPassword, trusting 127.0.0.1 as a proxy so tests can act as many clients
// through X-Real-IP.
func a14Server(t *testing.T, label string) (*g3Server, *sql.DB) {
	t.Helper()
	db, dsn := setupThrowawayDB(t, label)
	hash, _ := bcrypt.GenerateFromPassword([]byte(a14AdminPassword), bcrypt.DefaultCost)
	if _, err := db.Exec(`UPDATE admins SET password_hash = $1 WHERE username = 'admin'`, string(hash)); err != nil {
		t.Fatal(err)
	}
	return g3Start(t, dsn, g3FreePort(t), true, "TRUSTED_PROXY_CIDRS=127.0.0.1/32", "ABSENCE_CRON_SCHEDULE=0 0 1 1 *"), db
}

func a14AdminLogin(t *testing.T, srv *g3Server, ip, username, password string) a14Response {
	t.Helper()
	return a14Do(t, srv, "POST", "/api/admin/login", map[string]string{"X-Real-IP": ip}, map[string]string{"username": username, "password": password})
}

func a14AdminToken(t *testing.T, srv *g3Server) string {
	t.Helper()
	r := a14AdminLogin(t, srv, "198.51.100.1", "admin", a14AdminPassword)
	if r.status != 200 {
		t.Fatalf("admin login: %d %s", r.status, r.body)
	}
	data, _ := r.json(t)["data"].(map[string]any)
	return fmt.Sprint(data["token"])
}

func a14Retry(t *testing.T, r a14Response, max int) {
	t.Helper()
	n, err := strconv.Atoi(r.header.Get("Retry-After"))
	if err != nil || n < 1 || n > max {
		t.Errorf("Retry-After %q, want whole seconds in 1..%d", r.header.Get("Retry-After"), max)
	}
	if m := r.json(t); m["status"] != "error" || m["message"] != "Too many failed login attempts. Try again later." {
		t.Errorf("429 body %s, want the standard error envelope", r.body)
	}
}

// TestA1AdminLoginRateLimit verifies Task A1: failed admin logins are limited per username and
// client IP (5 per 15 minutes) and per client IP (20 per 15 minutes); only failures count, so
// the admin still logs in from an IP that has not failed; unknown users are indistinguishable
// from wrong passwords; rejections are logged without secrets.
func TestA1AdminLoginRateLimit(t *testing.T) {
	srv, _ := a14Server(t, "a1")

	t.Run("sixth failure for a username from one IP is 429, even with the right password", func(t *testing.T) {
		for i := 1; i <= 5; i++ {
			if r := a14AdminLogin(t, srv, "203.0.113.10", "admin", a14WrongPassword); r.status != 401 {
				t.Fatalf("failure %d: HTTP %d, want 401", i, r.status)
			}
		}
		r := a14AdminLogin(t, srv, "203.0.113.10", "admin", a14WrongPassword)
		if r.status != 429 {
			t.Fatalf("sixth failure: HTTP %d, want 429: %s", r.status, r.body)
		}
		a14Retry(t, r, 900)
		if r := a14AdminLogin(t, srv, "203.0.113.10", "admin", a14AdminPassword); r.status != 429 {
			t.Errorf("right password from the locked IP: HTTP %d, want 429", r.status)
		}
	})

	t.Run("the admin still logs in from another IP", func(t *testing.T) {
		if r := a14AdminLogin(t, srv, "203.0.113.20", "admin", a14AdminPassword); r.status != 200 {
			t.Errorf("HTTP %d, want 200: %s", r.status, r.body)
		}
	})

	t.Run("another username from another IP is unaffected", func(t *testing.T) {
		if r := a14AdminLogin(t, srv, "203.0.113.30", "other-admin", a14WrongPassword); r.status != 401 {
			t.Errorf("HTTP %d, want 401", r.status)
		}
	})

	t.Run("twenty failures from one IP across usernames block that IP", func(t *testing.T) {
		for i := 1; i <= 20; i++ {
			if r := a14AdminLogin(t, srv, "203.0.113.40", fmt.Sprintf("guess-%d", i), a14WrongPassword); r.status != 401 {
				t.Fatalf("failure %d: HTTP %d, want 401", i, r.status)
			}
		}
		r := a14AdminLogin(t, srv, "203.0.113.40", "guess-21", a14WrongPassword)
		if r.status != 429 {
			t.Fatalf("21st failure from the IP: HTTP %d, want 429", r.status)
		}
		a14Retry(t, r, 900)
	})

	t.Run("unknown username and wrong password are indistinguishable", func(t *testing.T) {
		var unknown, wrong []time.Duration
		var first [2]a14Response
		for i := 0; i < 6; i++ {
			u := a14AdminLogin(t, srv, fmt.Sprintf("192.0.2.%d", 10+i), "no-such-admin", a14WrongPassword)
			w := a14AdminLogin(t, srv, fmt.Sprintf("192.0.2.%d", 50+i), "admin", a14WrongPassword)
			if i == 0 {
				first = [2]a14Response{u, w}
			}
			unknown, wrong = append(unknown, u.took), append(wrong, w.took)
		}
		u, w := first[0], first[1]
		if u.status != w.status || !bytes.Equal(u.body, w.body) || u.header.Get("Content-Type") != w.header.Get("Content-Type") {
			t.Errorf("unknown user %d %q, wrong password %d %q: must be identical", u.status, u.body, w.status, w.body)
		}
		median := func(d []time.Duration) time.Duration {
			sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
			return d[len(d)/2]
		}
		mu, mw := median(unknown), median(wrong)
		t.Logf("median time: unknown user %v, wrong password %v", mu, mw)
		if ratio := float64(mu) / float64(mw); ratio < 0.6 || ratio > 1.6 {
			t.Errorf("unknown user takes %v, wrong password %v: the timing reveals which usernames exist", mu, mw)
		}
	})

	t.Run("concurrent failures never exceed the limit", func(t *testing.T) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		codes := map[int]int{}
		for i := 0; i < 30; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := a14AdminLogin(t, srv, "203.0.113.60", "admin", a14WrongPassword)
				mu.Lock()
				codes[r.status]++
				mu.Unlock()
			}()
		}
		wg.Wait()
		if codes[401] != 5 || codes[429] != 25 {
			t.Errorf("30 concurrent failures: %v, want exactly 5×401 and 25×429", codes)
		}
	})

	t.Run("rejections are logged at WARN without secrets", func(t *testing.T) {
		out := srv.out.String()
		var found bool
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, `"level":"WARN"`) && strings.Contains(line, `"msg":"AdminLoginHandler: login rate-limited"`) &&
				strings.Contains(line, `"username":"admin"`) && strings.Contains(line, `"ip":"203.0.113.10"`) && strings.Contains(line, `"path":"/api/admin/login"`) {
				found = true
			}
		}
		if !found {
			t.Errorf("no WARN log with username, ip and path for a rejected admin login")
		}
		for _, secret := range []string{a14AdminPassword, a14WrongPassword} {
			if strings.Contains(out, secret) {
				t.Errorf("the server log contains a password")
			}
		}
	})
}

// TestA1LimiterBoundsAndClientIP verifies the pieces behind A1: the limiter never tracks more
// than MaxKeys keys, fails closed instead of forgetting a lockout, recovers once entries expire,
// and is race-free; the client IP comes from forwarding headers only via a trusted proxy.
func TestA1LimiterBoundsAndClientIP(t *testing.T) {
	t.Run("limiter memory is bounded and fails closed", func(t *testing.T) {
		now := time.Unix(1_800_000_000, 0)
		l := ratelimit.NewLoginLimiter(5, 15*time.Minute)
		l.Now = func() time.Time { return now }
		l.MaxKeys = 100
		for i := 0; i < 5; i++ {
			l.Allow("victim")
			l.Finish("victim", ratelimit.Failed)
		}
		refused := 0
		for i := 0; i < 10_000; i++ {
			if ok, _ := l.Allow(fmt.Sprintf("flood-%d", i)); ok {
				l.Finish(fmt.Sprintf("flood-%d", i), ratelimit.Failed)
			} else {
				refused++
			}
		}
		if n := l.Len(); n > 100 {
			t.Errorf("limiter tracks %d keys, cap is 100", n)
		}
		if refused != 10_000-99 {
			t.Errorf("%d new keys refused while full, want %d", refused, 10_000-99)
		}
		if ok, _ := l.Allow("victim"); ok {
			t.Errorf("a flood of new keys erased the victim's lockout")
		}
		now = now.Add(16 * time.Minute)
		if ok, _ := l.Allow("after-expiry"); !ok {
			t.Errorf("new key refused after every entry expired")
		}
		if n := l.Len(); n > 2 {
			t.Errorf("%d keys tracked after expiry, want expired entries swept", n)
		}
	})

	t.Run("concurrent use is race-free and bounded", func(t *testing.T) {
		l := ratelimit.NewLoginLimiter(3, time.Minute)
		l.MaxKeys = 50
		var wg sync.WaitGroup
		for g := 0; g < 32; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < 500; i++ {
					key := fmt.Sprintf("k%d", (g*500+i)%80)
					if ok, _ := l.Allow(key); ok {
						l.Finish(key, ratelimit.Outcome(i%3))
					}
				}
			}(g)
		}
		wg.Wait()
		if n := l.Len(); n > 50 {
			t.Errorf("%d keys tracked, cap is 50", n)
		}
	})

	t.Run("client IP", func(t *testing.T) {
		trusted, err := clientip.ParsePrefixes("100.0.0.0/8, 10.1.2.3")
		if err != nil {
			t.Fatal(err)
		}
		r := clientip.Resolver{Trusted: trusted}
		cases := []struct {
			name, remote, realIP string
			xff                  []string
			want                 string
			identified           bool
		}{
			{"direct client", "198.51.100.7:5000", "", nil, "198.51.100.7", true},
			{"direct client cannot spoof headers", "198.51.100.7:5000", "1.2.3.4", []string{"1.2.3.4"}, "198.51.100.7", true},
			{"proxy with X-Real-IP", "100.64.0.9:443", "203.0.113.5", []string{"6.6.6.6, 203.0.113.5"}, "203.0.113.5", true},
			{"proxy, rightmost X-Forwarded-For wins over a spoofed left entry", "100.64.0.9:443", "", []string{"6.6.6.6, 203.0.113.6"}, "203.0.113.6", true},
			{"two trusted hops", "10.1.2.3:80", "", []string{"203.0.113.7, 100.70.0.1"}, "203.0.113.7", true},
			{"proxy that names no client", "100.64.0.9:443", "", nil, "100.64.0.9", false},
			{"X-Real-IP that is itself a proxy", "100.64.0.9:443", "100.64.0.10", nil, "100.64.0.9", false},
			{"IPv6 client", "[2001:db8::1]:443", "", nil, "2001:db8::1", true},
		}
		for _, c := range cases {
			req := httptest.NewRequest("POST", "/api/admin/login", nil)
			req.RemoteAddr = c.remote
			if c.realIP != "" {
				req.Header.Set("X-Real-IP", c.realIP)
			}
			for _, v := range c.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			if ip, ok := r.Resolve(req); ip != c.want || ok != c.identified {
				t.Errorf("%s: got (%s, %v), want (%s, %v)", c.name, ip, ok, c.want, c.identified)
			}
		}
		if _, err := clientip.ParsePrefixes("100.0.0.0/33"); err == nil {
			t.Errorf("an invalid prefix was accepted")
		}
	})
}

var a14PhoneVariants = []string{
	"07000000201", "+9647000000201", "009647000000201", "9647000000201", "+96407000000201",
	"0700 000 0201", "0700-000-0201", "(0700) 000 0201", " +964 700 000 0201 ",
	"٠٧٠٠٠٠٠٠٢٠١", "۰۷۰۰۰۰۰۰۲۰۱", "+٩٦٤٧٠٠٠٠٠٠٢٠١", "\u200e0700\u00a0000\u00a00201",
}

var a14InvalidPhones = []string{
	"", "12345", "7000000201", "+15551234567", "06000000201", "0700000020", "070000002011",
	"+9647000000201x", "07000000201+", "abc", "+964", "00964", "٠٧٠٠٠٠٠٠٢٠", "+96417000000201",
}

// TestA2PhoneNormalisation verifies Task A2: every accepted format of an Iraqi mobile number is
// one canonical number for login, the rate limiter, admin student create/update and storage;
// invalid numbers get 400 from the admin API and the unknown-phone 401 from login; and
// migration 000025 rewrites stored numbers exactly as the Go code does, failing loudly on
// numbers it cannot convert or that collide.
func TestA2PhoneNormalisation(t *testing.T) {
	t.Run("normaliser", func(t *testing.T) {
		for _, in := range a14PhoneVariants {
			if got, ok := phone.Normalize(in); !ok || got != "+9647000000201" {
				t.Errorf("Normalize(%q) = %q, %v; want +9647000000201", in, got, ok)
			}
		}
		for _, in := range a14InvalidPhones {
			if got, ok := phone.Normalize(in); ok {
				t.Errorf("Normalize(%q) = %q; want it rejected", in, got)
			}
		}
	})

	srv, db := a14Server(t, "a2")
	admin := a14AdminToken(t, srv)

	t.Run("admin create stores the canonical number", func(t *testing.T) {
		r := a14Do(t, srv, "POST", "/api/admin/students", a14Bearer(admin), map[string]any{
			"name": "A2 Student", "parent_name": "A2 Parent", "parent_phone": "٠٧٠٠ ٠٠٠ ٠٢٠١", "parent_pin": "Kq7#vR2m!Tx9pW4z", "rfid_tag": "A2-1",
		})
		if r.status != 200 {
			t.Fatalf("HTTP %d: %s", r.status, r.body)
		}
		data, _ := r.json(t)["data"].(map[string]any)
		if data["parent_phone"] != "+9647000000201" {
			t.Errorf("response parent_phone %v, want +9647000000201", data["parent_phone"])
		}
		r = a14Do(t, srv, "POST", "/api/admin/students", a14Bearer(admin), map[string]any{
			"name": "A2 Sibling", "parent_name": "A2 Parent", "parent_phone": "009647000000201", "rfid_tag": "A2-2",
		})
		if r.status != 200 {
			t.Fatalf("HTTP %d: %s", r.status, r.body)
		}
		var parents, children int
		db.QueryRow(`SELECT COUNT(*) FROM parents WHERE phone_number = '+9647000000201'`).Scan(&parents)
		db.QueryRow(`SELECT COUNT(*) FROM students s JOIN parents p ON p.id = s.parent_id WHERE p.phone_number = '+9647000000201'`).Scan(&children)
		if parents != 1 || children != 2 {
			t.Errorf("two formats of one number gave %d parents with %d children, want 1 parent with 2", parents, children)
		}
	})

	t.Run("admin create and update reject invalid numbers with 400", func(t *testing.T) {
		for _, bad := range []string{"12345", "+15551234567", "06000000201"} {
			r := a14Do(t, srv, "POST", "/api/admin/students", a14Bearer(admin), map[string]any{
				"name": "Bad", "parent_name": "Bad Parent", "parent_phone": bad, "rfid_tag": "A2-BAD-" + bad,
			})
			if r.status != 400 || !strings.Contains(fmt.Sprint(r.json(t)["message"]), "Iraqi mobile number") {
				t.Errorf("create with %q: HTTP %d %s, want 400 naming the expected format", bad, r.status, r.body)
			}
			r = a14Do(t, srv, "PUT", "/api/admin/students", a14Bearer(admin), map[string]any{
				"id": 1, "name": "Bad", "parent_name": "Bad Parent", "parent_phone": bad,
			})
			if r.status != 400 {
				t.Errorf("update with %q: HTTP %d, want 400", bad, r.status)
			}
		}
	})

	t.Run("every accepted format logs in as the same parent", func(t *testing.T) {
		var want any
		for _, in := range a14PhoneVariants {
			r := a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": in, "pin": "Kq7#vR2m!Tx9pW4z"})
			if r.status != 200 {
				t.Errorf("login with %q: HTTP %d %s", in, r.status, r.body)
				continue
			}
			data, _ := r.json(t)["data"].(map[string]any)
			parent, _ := data["parent"].(map[string]any)
			if parent["phone"] != "+9647000000201" {
				t.Errorf("login with %q returned phone %v, want +9647000000201", in, parent["phone"])
			}
			if want == nil {
				want = parent["id"]
			} else if parent["id"] != want {
				t.Errorf("login with %q returned parent %v, want %v", in, parent["id"], want)
			}
		}
	})

	t.Run("invalid numbers get the unknown-phone 401", func(t *testing.T) {
		unknown := a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": "07000000999", "pin": "Kq7#vR2m!Tx9pW4z"})
		for _, bad := range []string{"12345", "+15551234567", "abc", "06000000201"} {
			r := a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": bad, "pin": "Kq7#vR2m!Tx9pW4z"})
			if r.status != 401 || !bytes.Equal(r.body, unknown.body) {
				t.Errorf("login with %q: HTTP %d %s, want the unknown-phone answer %d %s", bad, r.status, r.body, unknown.status, unknown.body)
			}
		}
	})

	t.Run("all formats share one rate-limit counter", func(t *testing.T) {
		r := a14Do(t, srv, "POST", "/api/admin/students", a14Bearer(admin), map[string]any{
			"name": "A2 Limit", "parent_name": "A2 Limit Parent", "parent_phone": "+9647000000202", "parent_pin": "Ze4&uM9k?Ga2fC7x", "rfid_tag": "A2-3",
		})
		if r.status != 200 {
			t.Fatalf("HTTP %d: %s", r.status, r.body)
		}
		for i, in := range []string{"07000000202", "+9647000000202", "009647000000202", "0700 000 0202", "٠٧٠٠٠٠٠٠٢٠٢"} {
			if r := a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": in, "pin": "0000"}); r.status != 401 {
				t.Fatalf("failure %d (%q): HTTP %d, want 401", i+1, in, r.status)
			}
		}
		r = a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": "9647000000202", "pin": "Ze4&uM9k?Ga2fC7x"})
		if r.status != 429 {
			t.Fatalf("sixth attempt in a sixth format: HTTP %d, want 429", r.status)
		}
		a14Retry(t, r, 900)
	})

	t.Run("migration 000025", func(t *testing.T) {
		a14MigrationTest(t, db)
	})
}

func a14Migration(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func a14MigrationTest(t *testing.T, db *sql.DB) {
	up := a14Migration(t, "000025_normalize_phone_numbers.up.sql")
	down := a14Migration(t, "000025_normalize_phone_numbers.down.sql")
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exec := func(q string, args ...any) error {
		_, err := conn.ExecContext(ctx, q, args...)
		return err
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if err := exec(q, args...); err != nil {
			t.Fatalf("%v\n%.200s", err, q)
		}
	}
	reset := func() {
		t.Helper()
		mustExec(`DELETE FROM notifications`)
		mustExec(`UPDATE students SET parent_id = NULL`)
		mustExec(`DELETE FROM parents`)
		var applied bool
		conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'parents' AND column_name = 'phone_number_original')`).Scan(&applied)
		if applied {
			mustExec(down)
		}
	}
	reset()

	all := append(append([]string{}, a14PhoneVariants...), "07812345678", "+964 750 123 4567", "٠٧٧٠١٢٣٤٥٦٧")
	t.Run("rewrites every format exactly as the Go normaliser", func(t *testing.T) {
		reset()
		messy := []string{"07000000301", "+9647000000302", "009647000000303", "9647000000304", "0700 000 0305", "(0700) 000-0306", "٠٧٠٠٠٠٠٠٣٠٧", "۰۷۰۰۰۰۰۰۳۰۸", "+964 0700 000 0309", "\u200e0700\u00a0000\u00a00310"}
		for i, p := range messy {
			mustExec(`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES ($1, 'Messy', $2, 'h')`, 3000+i, p)
			mustExec(`INSERT INTO notifications (parent_phone, title, body) VALUES ($1, 't', 'b')`, p)
		}
		mustExec(`INSERT INTO notifications (parent_phone, title, body) VALUES ('not-a-phone', 't', 'b')`)
		mustExec(up)
		for i, p := range messy {
			want, _ := phone.Normalize(p)
			var got string
			var original sql.NullString
			conn.QueryRowContext(ctx, `SELECT phone_number, phone_number_original FROM parents WHERE id = $1`, 3000+i).Scan(&got, &original)
			if got != want {
				t.Errorf("parent %q: stored %q, Go normalises to %q", p, got, want)
			}
			if p != want && original.String != p {
				t.Errorf("parent %q: original kept as %q", p, original.String)
			}
		}
		var canonical, leftAlone int
		conn.QueryRowContext(ctx, `SELECT COUNT(*) FILTER (WHERE parent_phone ~ '^\+9647[0-9]{9}$'), COUNT(*) FILTER (WHERE parent_phone = 'not-a-phone') FROM notifications`).Scan(&canonical, &leftAlone)
		if canonical != len(messy) || leftAlone != 1 {
			t.Errorf("notifications: %d canonical, %d unparseable left alone; want %d and 1", canonical, leftAlone, len(messy))
		}
		mustExec(up)
		var changed int
		conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM parents WHERE phone_number !~ '^\+9647[0-9]{9}$'`).Scan(&changed)
		if changed != 0 {
			t.Errorf("re-running the migration left %d non-canonical numbers", changed)
		}
		mustExec(down)
		for i, p := range messy {
			var got string
			conn.QueryRowContext(ctx, `SELECT phone_number FROM parents WHERE id = $1`, 3000+i).Scan(&got)
			if got != p {
				t.Errorf("down-migration restored %q, want %q", got, p)
			}
		}
	})

	t.Run("SQL and Go agree on every test input", func(t *testing.T) {
		reset()
		inputs := append(append([]string{}, all...), a14InvalidPhones...)
		mustExec(up)
		for _, in := range inputs {
			var got sql.NullString
			if err := conn.QueryRowContext(ctx, `SELECT pg_temp.fk_iq_mobile($1)`, in).Scan(&got); err != nil {
				t.Fatal(err)
			}
			want, ok := phone.Normalize(in)
			if got.Valid != ok || got.String != want {
				t.Errorf("%q: SQL gives (%q, %v), Go gives (%q, %v)", in, got.String, got.Valid, want, ok)
			}
		}
	})

	t.Run("collisions fail loudly, listing the ids, and change nothing", func(t *testing.T) {
		reset()
		mustExec(`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (4001, 'One', '07000000401', 'h'), (4002, 'Two', '+9647000000401', 'h'), (4003, 'Three', '07000000402', 'h'), (4004, 'Four', '0700 000 0402', 'h'), (4005, 'Alone', '07000000403', 'h')`)
		err := exec(up)
		if err == nil || !strings.Contains(err.Error(), "[4001, 4002]") || !strings.Contains(err.Error(), "[4003, 4004]") {
			t.Fatalf("collision error %v, want both groups of ids", err)
		}
		var unchanged int
		conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM parents WHERE id = 4005 AND phone_number = '07000000403'`).Scan(&unchanged)
		if unchanged != 1 {
			t.Errorf("a failed migration changed data")
		}
	})

	t.Run("the read-only check script reports what the migration would do", func(t *testing.T) {
		reset()
		mustExec(`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (6001, 'A', '07000000601', 'h'), (6002, 'B', '+9647000000601', 'h'), (6003, 'C', '+96412345678', 'h'), (6004, 'D', '+9647000000604', 'h'), (6005, 'E', '0700 000 0605', 'h')`)
		script, err := os.ReadFile(filepath.Join("..", "..", "db", "scripts", "check_phone_formats.sql"))
		if err != nil {
			t.Fatal(err)
		}
		query := string(script)
		query = query[strings.Index(query, "WITH p AS"):strings.Index(query, "ROLLBACK;")]
		rows, err := conn.QueryContext(ctx, strings.TrimSuffix(strings.TrimSpace(query), ";"))
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for rows.Next() {
			var kind, detail, value string
			rows.Scan(&kind, &detail, &value)
			got[kind+"|"+detail] = value
		}
		rows.Close()
		want := map[string]string{
			"invalid parent ids|":            "6003",
			"collision groups (parent ids)|": "[6001, 6002]",
			"already canonical|":             "2",
			"would change|":                  "2",
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %q, want %q", k, got[k], v)
			}
		}
		if err := exec(up); err == nil || !strings.Contains(err.Error(), "6003") {
			t.Errorf("the migration should abort on the parent the script lists as invalid: %v", err)
		}
	})

	t.Run("invalid stored numbers fail loudly, listing the ids", func(t *testing.T) {
		reset()
		mustExec(`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (5001, 'Landline', '+96412345678', 'h'), (5002, 'Fine', '07000000501', 'h'), (5003, 'Foreign', '+15551234567', 'h')`)
		err := exec(up)
		if err == nil || !strings.Contains(err.Error(), "5001, 5003") || strings.Contains(err.Error(), "5002") {
			t.Fatalf("invalid-number error %v, want ids 5001, 5003 only", err)
		}
	})
	reset()
	mustExec(up)
}

// TestA3SignOutOnPINChange verifies Task A3: a parent's tokens stop working when an admin
// changes the PIN, even a token issued in the same second as the change; tokens issued
// afterwards work; legacy tokens without the sv claim and tokens of deleted parents get a clean
// 401; rotating the admin password with db/scripts/rotate_admin_password.sql signs out admin
// tokens; and the extra per-request lookup is an index scan.
func TestA3SignOutOnPINChange(t *testing.T) {
	srv, db := a14Server(t, "a3")
	admin := a14AdminToken(t, srv)

	create := a14Do(t, srv, "POST", "/api/admin/students", a14Bearer(admin), map[string]any{
		"name": "A3 Student", "parent_name": "A3 Parent", "parent_phone": "+9647000000601", "parent_pin": "Kq7#vR2m!Tx9pW4z", "rfid_tag": "A3-1",
	})
	if create.status != 200 {
		t.Fatalf("create: %d %s", create.status, create.body)
	}
	studentID := create.json(t)["data"].(map[string]any)["id"]
	login := func(pin string) (string, int) {
		r := a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": "07000000601", "pin": pin})
		if r.status != 200 {
			return "", r.status
		}
		return fmt.Sprint(r.json(t)["data"].(map[string]any)["token"]), r.status
	}
	students := func(token string) a14Response {
		return a14Do(t, srv, "GET", "/api/mobile/students", a14Bearer(token), nil)
	}
	changePIN := func(pin string) {
		r := a14Do(t, srv, "PUT", "/api/admin/students", a14Bearer(admin), map[string]any{
			"id": studentID, "name": "A3 Student", "parent_name": "A3 Parent", "parent_phone": "+9647000000601", "parent_pin": pin,
		})
		if r.status != 200 {
			t.Fatalf("change PIN: %d %s", r.status, r.body)
		}
	}

	t.Run("old token gets 401 after a PIN change; the new PIN works", func(t *testing.T) {
		old, _ := login("Kq7#vR2m!Tx9pW4z")
		if r := students(old); r.status != 200 {
			t.Fatalf("before the change: %d", r.status)
		}
		changePIN("Rt5!jX8q-Bn3vL6h")
		r := students(old)
		if r.status != 401 || r.json(t)["message"] != "Invalid or expired token" {
			t.Errorf("old token after the PIN change: %d %s, want 401 Invalid or expired token", r.status, r.body)
		}
		if _, code := login("Kq7#vR2m!Tx9pW4z"); code != 401 {
			t.Errorf("old PIN: HTTP %d, want 401", code)
		}
		fresh, code := login("Rt5!jX8q-Bn3vL6h")
		if code != 200 || students(fresh).status != 200 {
			t.Errorf("new PIN: login %d, then %d; want both 200", code, students(fresh).status)
		}
		if !strings.Contains(srv.out.String(), "AuthMiddleware: token issued before the parent's PIN changed") {
			t.Errorf("no WARN log for the rejected old token")
		}
	})

	t.Run("updating a student without a PIN keeps sessions", func(t *testing.T) {
		tok, _ := login("Rt5!jX8q-Bn3vL6h")
		r := a14Do(t, srv, "PUT", "/api/admin/students", a14Bearer(admin), map[string]any{
			"id": studentID, "name": "A3 Student Renamed", "parent_name": "A3 Parent", "parent_phone": "+9647000000601",
		})
		if r.status != 200 || students(tok).status != 200 {
			t.Errorf("token after an update without parent_pin: %d, want 200", students(tok).status)
		}
	})

	t.Run("same-second boundary is decided by the session version", func(t *testing.T) {
		iat := func(tok string) int64 {
			c := jwt.MapClaims{}
			jwt.NewParser().ParseUnverified(tok, c)
			f, _ := c["iat"].(float64)
			return int64(f)
		}
		pins := []string{"Wp2_Dg7y#Ks9mE4t", "Nc6?Fa3w@Ub8rZ5k", "Mx9%Pq4h!Vd2gT7e", "Ty3-Lk8b&Qs5nH2w", "Ej7#Rc4v?Ym9pA3u", "Gs2@Wn6t_Bx8kD5q"}
		current := "Rt5!jX8q-Bn3vL6h"
		for attempt := 0; attempt < len(pins)/2; attempt++ {
			time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second + 20*time.Millisecond)))
			before, _ := login(current)
			current = pins[attempt]
			changePIN(current)
			after, _ := login(current)
			if iat(before) != iat(after) {
				continue
			}
			if r := students(before); r.status != 401 {
				t.Errorf("token issued in the same second, before the change: %d, want 401", r.status)
			}
			if r := students(after); r.status != 200 {
				t.Errorf("token issued in the same second, after the change: %d, want 200", r.status)
			}
			return
		}
		t.Skip("could not issue both tokens within one second on this machine")
	})

	t.Run("legacy token without sv gets a clean 401", func(t *testing.T) {
		var parentID int
		db.QueryRow(`SELECT id FROM parents WHERE phone_number = '+9647000000601'`).Scan(&parentID)
		legacy, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"parent_id": parentID, "phone": "+9647000000601", "role": "parent",
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Add(-time.Minute).Unix(),
		}).SignedString([]byte("g3"))
		r := students(legacy)
		if r.status != 401 || r.json(t)["status"] != "error" {
			t.Errorf("legacy token: %d %s, want 401 with the error envelope", r.status, r.body)
		}
	})

	t.Run("token of a deleted parent gets 401", func(t *testing.T) {
		if _, err := db.Exec(`INSERT INTO parents (full_name, phone_number, pin_code, session_version) VALUES ('Gone', '+9647000000602', $1, 0)`, a14Hash(t, "7777")); err != nil {
			t.Fatal(err)
		}
		r := a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": "+9647000000602", "pin": "7777"})
		tok := fmt.Sprint(r.json(t)["data"].(map[string]any)["token"])
		db.Exec(`DELETE FROM parents WHERE phone_number = '+9647000000602'`)
		if r := students(tok); r.status != 401 {
			t.Errorf("deleted parent's token: %d, want 401", r.status)
		}
	})

	t.Run("rotating the admin password signs out admin tokens", func(t *testing.T) {
		script, err := os.ReadFile(filepath.Join("..", "..", "db", "scripts", "rotate_admin_password.sql"))
		if err != nil {
			t.Fatal(err)
		}
		if r := a14Do(t, srv, "GET", "/api/admin/dashboard", a14Bearer(admin), nil); r.status != 200 {
			t.Fatalf("admin token before rotation: %d", r.status)
		}
		if _, err := db.Exec(strings.ReplaceAll(string(script), "NEW_PASSWORD_HERE", "a3-rotated-password")); err != nil {
			t.Fatalf("rotation script: %v", err)
		}
		if r := a14Do(t, srv, "GET", "/api/admin/dashboard", a14Bearer(admin), nil); r.status != 401 {
			t.Errorf("admin token after rotation: %d, want 401", r.status)
		}
		r := a14AdminLogin(t, srv, "198.51.100.2", "admin", "a3-rotated-password")
		if r.status != 200 {
			t.Fatalf("login with the rotated password: %d %s", r.status, r.body)
		}
		tok := fmt.Sprint(r.json(t)["data"].(map[string]any)["token"])
		if r := a14Do(t, srv, "GET", "/api/admin/dashboard", a14Bearer(tok), nil); r.status != 200 {
			t.Errorf("new admin token: %d, want 200", r.status)
		}
	})

	t.Run("session lookup is one index scan", func(t *testing.T) {
		if _, err := db.Exec(`INSERT INTO parents (full_name, phone_number, pin_code) SELECT 'Bulk', '+96470' || lpad(g::text, 8, '0'), 'h' FROM generate_series(10000000, 10020000) g`); err != nil {
			t.Fatal(err)
		}
		db.Exec(`ANALYZE parents`)
		for _, c := range []struct{ query, index string }{
			{`EXPLAIN SELECT session_version FROM parents WHERE id = 12345`, "parents_pkey"},
			{`EXPLAIN SELECT session_version FROM admins WHERE username = 'admin'`, "admins_username_key"},
		} {
			rows, _ := db.Query(c.query)
			var plan []string
			for rows.Next() {
				var line string
				rows.Scan(&line)
				plan = append(plan, line)
			}
			rows.Close()
			if !strings.Contains(strings.Join(plan, "\n"), c.index) && !strings.Contains(strings.Join(plan, "\n"), "Seq Scan on admins") {
				t.Errorf("%s:\n%s\nwant an index scan on %s", c.query, strings.Join(plan, "\n"), c.index)
			}
		}
		var durations []time.Duration
		for i := 0; i < 2000; i++ {
			start := time.Now()
			var v int
			db.QueryRow(`SELECT session_version FROM parents WHERE id = $1`, 1+i%20000).Scan(&v)
			durations = append(durations, time.Since(start))
		}
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		p50, p99 := durations[len(durations)/2], durations[len(durations)*99/100]
		t.Logf("session lookup over 20,000 parents: p50 %v, p99 %v per request", p50, p99)
		if p50 > 5*time.Millisecond {
			t.Errorf("session lookup p50 %v is too slow for every request", p50)
		}
	})
}

func a14Hash(t *testing.T, pin string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pin), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

// TestA4RouteErrors verifies Task A4: an unknown path gets a JSON 404, and a wrong method on a
// known path gets a JSON 405 with an Allow header before authentication. /health and the ADMS
// routes keep exactly the plain-text answers devices have always received (RULES.md §5).
func TestA4RouteErrors(t *testing.T) {
	srv, _ := a14Server(t, "a4")
	envelope := func(t *testing.T, r a14Response, status int, message string) {
		t.Helper()
		if r.status != status || r.header.Get("Content-Type") != "application/json" {
			t.Fatalf("HTTP %d %q %s, want %d application/json", r.status, r.header.Get("Content-Type"), r.body, status)
		}
		if m := r.json(t); m["status"] != "error" || m["message"] != message || len(m) != 2 {
			t.Errorf("body %s, want {\"status\":\"error\",\"message\":%q}", r.body, message)
		}
	}

	t.Run("unknown path is a JSON 404", func(t *testing.T) {
		for _, p := range []string{"/api/nope", "/api/mobile/unknown", "/", "/healthz", "/api/admin/students/7"} {
			envelope(t, a14Do(t, srv, "GET", p, nil, nil), 404, "Not found")
		}
	})

	t.Run("wrong method is a JSON 405 with Allow, before authentication", func(t *testing.T) {
		for _, c := range []struct{ method, path, allow string }{
			{"PUT", "/api/mobile/students", "GET, HEAD"},
			{"POST", "/api/admin/dashboard", "GET, HEAD"},
			{"DELETE", "/api/admin/login", "POST"},
			{"GET", "/api/mobile/login", "POST"},
			{"PATCH", "/api/admin/students", "DELETE, GET, HEAD, POST, PUT"},
			{"DELETE", "/api/admin/settings", "GET, HEAD, PUT"},
		} {
			r := a14Do(t, srv, c.method, c.path, nil, nil)
			envelope(t, r, 405, "Method not allowed")
			if got := r.header.Get("Allow"); got != c.allow {
				t.Errorf("%s %s: Allow %q, want %q", c.method, c.path, got, c.allow)
			}
		}
	})

	t.Run("hardware routes and /health are unchanged", func(t *testing.T) {
		for _, c := range []struct {
			method, path string
			status       int
			ctype, body  string
			allow        string
		}{
			{"GET", "/iclock/cdata?SN=X", 200, "text/plain", "OK", ""},
			{"GET", "/iclock/getrequest?SN=X", 200, "text/plain", "OK", ""},
			{"GET", "/api/attendance/push?SN=X", 200, "text/plain", "OK", ""},
			{"PUT", "/api/attendance/push?SN=X", 200, "text/plain", "OK", ""},
			{"PUT", "/iclock/cdata", 405, "text/plain; charset=utf-8", "Method Not Allowed\n", "GET, HEAD, POST"},
			{"POST", "/iclock/getrequest", 405, "text/plain; charset=utf-8", "Method Not Allowed\n", "GET, HEAD"},
			{"GET", "/iclock/devicecmd", 404, "text/plain; charset=utf-8", "404 page not found\n", ""},
			{"POST", "/health", 405, "text/plain; charset=utf-8", "Method Not Allowed\n", "GET, HEAD"},
			{"GET", "/health", 200, "application/json", `{"message":"Server is healthy and running!","status":"success"}` + "\n", ""},
			{"GET", "/api/attendance/push/json", 401, "application/json", `{"message":"Device SN is required","status":"error"}` + "\n", ""},
		} {
			r := a14Do(t, srv, c.method, c.path, nil, nil)
			if r.status != c.status || r.header.Get("Content-Type") != c.ctype || string(r.body) != c.body || r.header.Get("Allow") != c.allow {
				t.Errorf("%s %s: %d %q %q Allow=%q; want %d %q %q Allow=%q", c.method, c.path, r.status, r.header.Get("Content-Type"), r.body, r.header.Get("Allow"), c.status, c.ctype, c.body, c.allow)
			}
		}
	})
}

// TestA1TrustedProxyDefault verifies that no proxy is trusted unless TRUSTED_PROXY_CIDRS is
// set, on Railway too, and that the server logs what the first requests' connections and
// forwarding headers look like, so the value can be chosen from real traffic.
func TestA1TrustedProxyDefault(t *testing.T) {
	t.Run("config trusts nothing by default, even on Railway", func(t *testing.T) {
		t.Setenv("RAILWAY_ENVIRONMENT_NAME", "staging")
		t.Setenv("TRUSTED_PROXY_CIDRS", "")
		os.Unsetenv("TRUSTED_PROXY_CIDRS")
		cfg, err := config.LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.OnRailway || len(cfg.TrustedProxies) != 0 {
			t.Errorf("OnRailway=%v TrustedProxies=%v; want Railway detected and no trusted proxy", cfg.OnRailway, cfg.TrustedProxies)
		}
	})

	t.Run("server warns and logs the observed addresses of the first requests", func(t *testing.T) {
		_, dsn := setupThrowawayDB(t, "a1proxy")
		srv := g3Start(t, dsn, g3FreePort(t), true, "RAILWAY_ENVIRONMENT_NAME=test", "ABSENCE_CRON_SCHEDULE=0 0 1 1 *")
		for i := 0; i < 30; i++ {
			a14Do(t, srv, "GET", "/api/mobile/settings", map[string]string{"X-Real-IP": "203.0.113.99", "X-Forwarded-For": "198.51.100.9"}, nil)
		}
		out := srv.out.String()
		if !strings.Contains(out, "TRUSTED_PROXY_CIDRS is not set: every client appears as Railway's proxy address") {
			t.Errorf("no startup warning about the unset TRUSTED_PROXY_CIDRS on Railway")
		}
		observed := 0
		for _, line := range strings.Split(out, "\n") {
			if !strings.Contains(line, `"msg":"Observed client address"`) {
				continue
			}
			observed++
			if !strings.Contains(line, `"x_real_ip":"203.0.113.99"`) && !strings.Contains(line, `"path":"/health"`) {
				t.Errorf("observation without the X-Real-IP header: %s", line)
			}
			if strings.Contains(line, `"path":"/api/mobile/settings"`) && (!strings.Contains(line, `"x_forwarded_for":"198.51.100.9"`) || !strings.Contains(line, `"client_ip":"127.0.0.1"`) || !strings.Contains(line, `"remote_addr":"127.0.0.1:`)) {
				t.Errorf("observation should show the raw headers and ignore them for client_ip: %s", line)
			}
		}
		if observed != 20 {
			t.Errorf("%d observation log lines, want exactly 20", observed)
		}
	})
}

// TestA14BeforePhoneNormalisation runs the new code on a database migrated to 000024 (session
// versions, device tokens, notifications by parent, banner images) without 000025 (phone
// normalisation), the state Staging is in between
// the steps of the README deploy notes. Sessions, rate limits and route errors work; a parent
// whose number is still stored in another format cannot log in until 000025 runs, which then
// fixes it.
func TestA14BeforePhoneNormalisation(t *testing.T) {
	db, dsn := setupThrowawayDB(t, "a14v22")
	if _, err := db.Exec(a14Migration(t, "000025_normalize_phone_numbers.down.sql")); err != nil {
		t.Fatal(err)
	}
	var phoneColumns, sessionColumns int
	var deviceTokens, bannerImages bool
	db.QueryRow(`SELECT COUNT(*) FILTER (WHERE column_name IN ('phone_number_original', 'parent_phone_original')), COUNT(*) FILTER (WHERE column_name = 'session_version'), to_regclass('device_tokens') IS NOT NULL, to_regclass('banner_images') IS NOT NULL FROM information_schema.columns`).Scan(&phoneColumns, &sessionColumns, &deviceTokens, &bannerImages)
	if phoneColumns != 0 || sessionColumns != 2 || !deviceTokens || !bannerImages {
		t.Fatalf("schema has %d phone-normalisation and %d session_version columns, device_tokens %v, banner_images %v; want 0, 2, true, true", phoneColumns, sessionColumns, deviceTokens, bannerImages)
	}
	adminHash, _ := bcrypt.GenerateFromPassword([]byte(a14AdminPassword), bcrypt.DefaultCost)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE admins SET password_hash = $1 WHERE username = 'admin'`, []any{string(adminHash)}},
		{`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (701, 'Canonical Parent', '+9647000000701', $1), (702, 'Legacy Parent', '07000000702', $2)`, []any{a14Hash(t, "482193"), a14Hash(t, "593047")}},
		{`INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES (701, 'Canonical Child', 'V21-1', 701), (702, 'Legacy Child', 'V21-2', 702)`, nil},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	srv := g3Start(t, dsn, g3FreePort(t), true, "TRUSTED_PROXY_CIDRS=127.0.0.1/32", "ABSENCE_CRON_SCHEDULE=0 0 1 1 *")
	admin := a14AdminToken(t, srv)
	login := func(phoneNumber, pin string) (string, int) {
		r := a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": phoneNumber, "pin": pin})
		if r.status != 200 {
			return "", r.status
		}
		return fmt.Sprint(r.json(t)["data"].(map[string]any)["token"]), r.status
	}

	t.Run("a canonical parent logs in with any format", func(t *testing.T) {
		for _, in := range []string{"+9647000000701", "0700 000 0701", "٠٧٠٠٠٠٠٠٧٠١"} {
			tok, code := login(in, "482193")
			if code != 200 || a14Do(t, srv, "GET", "/api/mobile/students", a14Bearer(tok), nil).status != 200 {
				t.Errorf("login with %q: %d", in, code)
			}
		}
	})

	t.Run("a PIN change signs the parent out", func(t *testing.T) {
		old, _ := login("+9647000000701", "482193")
		r := a14Do(t, srv, "PUT", "/api/admin/students", a14Bearer(admin), map[string]any{
			"id": 701, "name": "Canonical Child", "parent_name": "Canonical Parent", "parent_phone": "+9647000000701", "parent_pin": "Hb8@nP3e%Yw6sJ5d",
		})
		if r.status != 200 {
			t.Fatalf("PIN change: %d %s", r.status, r.body)
		}
		if got := a14Do(t, srv, "GET", "/api/mobile/students", a14Bearer(old), nil).status; got != 401 {
			t.Errorf("old token: %d, want 401", got)
		}
		if _, code := login("07000000701", "Hb8@nP3e%Yw6sJ5d"); code != 200 {
			t.Errorf("new PIN: %d, want 200", code)
		}
	})

	t.Run("device tokens register before phone normalisation", func(t *testing.T) {
		tok, _ := login("+9647000000701", "Hb8@nP3e%Yw6sJ5d")
		r := a14Do(t, srv, "PUT", "/api/mobile/device-token", a14Bearer(tok), map[string]string{"token": "test-device-v22-000000000000"})
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM device_tokens WHERE parent_id = 701`).Scan(&n)
		if r.status != 200 || n != 1 {
			t.Errorf("register: %d, %d tokens stored; want 200 and 1", r.status, n)
		}
	})

	t.Run("admin rate limit and route errors work", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			a14AdminLogin(t, srv, "203.0.113.70", "admin", a14WrongPassword)
		}
		if r := a14AdminLogin(t, srv, "203.0.113.70", "admin", a14WrongPassword); r.status != 429 {
			t.Errorf("sixth admin failure: %d, want 429", r.status)
		}
		if r := a14Do(t, srv, "GET", "/api/nope", nil, nil); r.status != 404 || r.header.Get("Content-Type") != "application/json" {
			t.Errorf("unknown path: %d %s", r.status, r.header.Get("Content-Type"))
		}
	})

	t.Run("a parent stored in another format cannot log in until 000025 runs", func(t *testing.T) {
		for _, in := range []string{"07000000702", "+9647000000702"} {
			if _, code := login(in, "593047"); code != 401 {
				t.Errorf("legacy-format parent with %q: %d, want 401 until 000025 runs", in, code)
			}
		}
	})

	t.Run("adding a student for that parent creates a canonical duplicate that 000025 reports", func(t *testing.T) {
		r := a14Do(t, srv, "POST", "/api/admin/students", a14Bearer(admin), map[string]any{
			"name": "Legacy Sibling", "parent_name": "Legacy Parent", "parent_phone": "07000000702", "parent_pin": "Uf5&Hz9c!Mr3eJ7a", "rfid_tag": "V21-3",
		})
		if r.status != 200 {
			t.Fatalf("create: %d %s", r.status, r.body)
		}
		var dupID int
		db.QueryRow(`SELECT id FROM parents WHERE phone_number = '+9647000000702'`).Scan(&dupID)
		if dupID == 0 {
			t.Fatalf("no canonical duplicate was created")
		}
		_, err := db.Exec(a14Migration(t, "000025_normalize_phone_numbers.up.sql"))
		ids := []int{702, dupID}
		sort.Ints(ids)
		if want := fmt.Sprintf("[%d, %d]", ids[0], ids[1]); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("000025 error %v, want the collision group %s", err, want)
		}
		if _, err := db.Exec(`UPDATE students SET parent_id = 702 WHERE parent_id = $1`, dupID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`DELETE FROM parents WHERE id = $1`, dupID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("after 000025 the parent logs in with any format", func(t *testing.T) {
		if _, err := db.Exec(a14Migration(t, "000025_normalize_phone_numbers.up.sql")); err != nil {
			t.Fatalf("000025: %v", err)
		}
		for _, in := range []string{"07000000702", "+9647000000702", "009647000000702"} {
			if _, code := login(in, "593047"); code != 200 {
				t.Errorf("login with %q after 000025: %d, want 200", in, code)
			}
		}
	})
}
