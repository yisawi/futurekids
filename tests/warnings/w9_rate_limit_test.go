package warnings

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"future_kids/internal/auth"
	"future_kids/internal/handlers"
	"future_kids/internal/ratelimit"

	"golang.org/x/crypto/bcrypt"
)

const (
	w9Max    = 5
	w9Window = 15 * time.Minute
)

// fakeClock is a manually advanced clock for LoginLimiter.Now.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestLimiter() (*ratelimit.LoginLimiter, *fakeClock) {
	clock := &fakeClock{now: time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)}
	l := ratelimit.NewLoginLimiter(w9Max, w9Window)
	l.Now = clock.Now
	return l, clock
}

// fail reserves and fails n attempts for key, failing the test if any is refused.
func fail(t *testing.T, l *ratelimit.LoginLimiter, key string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if ok, _ := l.Allow(key); !ok {
			t.Fatalf("%s: attempt %d refused, want allowed", key, i)
		}
		l.Finish(key, ratelimit.Failed)
	}
}

func assertLocked(t *testing.T, l *ratelimit.LoginLimiter, key string, wantRetry time.Duration) {
	t.Helper()
	ok, retry := l.Allow(key)
	if ok {
		l.Finish(key, ratelimit.Released)
		t.Fatalf("%s: attempt allowed, want locked", key)
	}
	if retry != wantRetry {
		t.Errorf("%s: retry-after = %v, want %v", key, retry, wantRetry)
	}
}

// TestW9RateLimit verifies brute-force protection on parent login (audit warning W9).
func TestW9RateLimit(t *testing.T) {
	t.Run("limiter/tracks failures and locks at the limit", func(t *testing.T) {
		l, _ := newTestLimiter()
		fail(t, l, "p", w9Max)
		assertLocked(t, l, "p", w9Window)
		assertLocked(t, l, "p", w9Window)
	})

	t.Run("limiter/lock lifts exactly when the window ends", func(t *testing.T) {
		l, clock := newTestLimiter()
		fail(t, l, "p", w9Max)
		clock.Advance(10 * time.Minute)
		assertLocked(t, l, "p", 5*time.Minute)
		clock.Advance(5*time.Minute - time.Nanosecond)
		assertLocked(t, l, "p", time.Nanosecond)
		clock.Advance(time.Nanosecond)
		fail(t, l, "p", w9Max)
		assertLocked(t, l, "p", w9Window)
	})

	t.Run("limiter/failures from an expired window do not carry over", func(t *testing.T) {
		l, clock := newTestLimiter()
		fail(t, l, "p", w9Max-1)
		clock.Advance(w9Window)
		fail(t, l, "p", w9Max)
		assertLocked(t, l, "p", w9Window)
	})

	t.Run("limiter/success resets the counter", func(t *testing.T) {
		l, _ := newTestLimiter()
		fail(t, l, "p", w9Max-1)
		if ok, _ := l.Allow("p"); !ok {
			t.Fatal("attempt before limit refused")
		}
		l.Finish("p", ratelimit.Succeeded)
		fail(t, l, "p", w9Max)
		assertLocked(t, l, "p", w9Window)
	})

	t.Run("limiter/released attempts are not counted", func(t *testing.T) {
		l, _ := newTestLimiter()
		for i := 0; i < 3*w9Max; i++ {
			if ok, _ := l.Allow("p"); !ok {
				t.Fatalf("released attempt %d refused", i+1)
			}
			l.Finish("p", ratelimit.Released)
		}
		fail(t, l, "p", w9Max)
		assertLocked(t, l, "p", w9Window)
	})

	t.Run("limiter/keys are independent", func(t *testing.T) {
		l, _ := newTestLimiter()
		fail(t, l, "a", w9Max)
		assertLocked(t, l, "a", w9Window)
		fail(t, l, "b", w9Max)
	})

	t.Run("limiter/in-flight reservations count toward the limit", func(t *testing.T) {
		l, _ := newTestLimiter()
		for i := 0; i < w9Max; i++ {
			if ok, _ := l.Allow("p"); !ok {
				t.Fatalf("reservation %d refused", i+1)
			}
		}
		assertLocked(t, l, "p", w9Window)
		l.Finish("p", ratelimit.Released)
		if ok, _ := l.Allow("p"); !ok {
			t.Fatal("released slot not reusable")
		}
	})

	t.Run("limiter/expired idle entries are swept", func(t *testing.T) {
		l, clock := newTestLimiter()
		for i := 0; i < 100; i++ {
			fail(t, l, fmt.Sprintf("k%d", i), 1)
		}
		if ok, _ := l.Allow("busy"); !ok {
			t.Fatal("busy reservation refused")
		}
		if n := l.Len(); n != 101 {
			t.Fatalf("tracked keys = %d, want 101", n)
		}
		clock.Advance(w9Window)
		fail(t, l, "trigger", 1)
		if n := l.Len(); n != 2 {
			t.Errorf("after window, tracked keys = %d, want 2 (in-flight 'busy' and 'trigger')", n)
		}
		l.Finish("busy", ratelimit.Released)
	})

	t.Run("limiter/concurrent attempts never exceed the limit", func(t *testing.T) {
		l, _ := newTestLimiter()
		var allowed atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 200; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if ok, _ := l.Allow("p"); ok {
					allowed.Add(1)
					time.Sleep(time.Millisecond)
					l.Finish("p", ratelimit.Failed)
				}
			}()
		}
		wg.Wait()
		if n := allowed.Load(); n != w9Max {
			t.Errorf("concurrent attempts allowed = %d, want exactly %d", n, w9Max)
		}

		outcomes := []ratelimit.Outcome{ratelimit.Failed, ratelimit.Succeeded, ratelimit.Released}
		for i := 0; i < 400; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				key := fmt.Sprintf("k%d", i%20)
				if ok, _ := l.Allow(key); ok {
					l.Finish(key, outcomes[i%len(outcomes)])
				}
				l.Len()
			}(i)
		}
		wg.Wait()
	})

	db, _ := setupThrowawayDB(t, "w9")
	auth.InitAuth("w9-test-secret")
	hash, err := bcrypt.GenerateFromPassword([]byte("4321"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash PIN: %v", err)
	}
	for _, phone := range []string{"+9647000000401", "+9647000000402", "+9647000000403"} {
		if _, err := db.Exec(`INSERT INTO parents (full_name, phone_number, pin_code) VALUES ('W9 Parent', $1, $2)`, phone, string(hash)); err != nil {
			t.Fatalf("seed parent %s: %v", phone, err)
		}
	}

	login := func(t *testing.T, app *handlers.AppEnv, phone, pin string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"phone": phone, "pin": pin})
		rec := serve(t, app.MobileLoginHandler, http.MethodPost, "/api/mobile/login", "", string(body))
		return rec.Code, rec.Header().Get("Retry-After")
	}
	expect := func(t *testing.T, app *handlers.AppEnv, phone, pin string, n, want int) {
		t.Helper()
		for i := 1; i <= n; i++ {
			if code, _ := login(t, app, phone, pin); code != want {
				t.Fatalf("login %q attempt %d: HTTP %d, want %d", phone, i, code, want)
			}
		}
	}

	t.Run("http/wrong PINs lock the number, correct PIN works after the window", func(t *testing.T) {
		l, clock := newTestLimiter()
		app := &handlers.AppEnv{DB: db, LoginLimiter: l}
		expect(t, app, "+9647000000401", "0000", w9Max, http.StatusUnauthorized)
		code, retry := login(t, app, "+9647000000401", "0000")
		if code != http.StatusTooManyRequests || retry != "900" {
			t.Errorf("attempt %d: HTTP %d Retry-After %q, want 429 and 900", w9Max+1, code, retry)
		}
		if code, _ := login(t, app, "+9647000000401", "4321"); code != http.StatusTooManyRequests {
			t.Errorf("correct PIN while locked: HTTP %d, want 429", code)
		}
		clock.Advance(w9Window)
		expect(t, app, "+9647000000401", "4321", 1, http.StatusOK)
		expect(t, app, "+9647000000401", "0000", w9Max, http.StatusUnauthorized)
		expect(t, app, "+9647000000401", "0000", 1, http.StatusTooManyRequests)
	})

	t.Run("http/successful login resets the counter", func(t *testing.T) {
		l, _ := newTestLimiter()
		app := &handlers.AppEnv{DB: db, LoginLimiter: l}
		expect(t, app, "+9647000000402", "0000", w9Max-1, http.StatusUnauthorized)
		expect(t, app, "+9647000000402", "4321", 1, http.StatusOK)
		expect(t, app, "+9647000000402", "0000", w9Max, http.StatusUnauthorized)
		expect(t, app, "+9647000000402", "0000", 1, http.StatusTooManyRequests)
	})

	t.Run("http/unregistered numbers are limited the same way", func(t *testing.T) {
		l, _ := newTestLimiter()
		app := &handlers.AppEnv{DB: db, LoginLimiter: l}
		expect(t, app, "+9647099999999", "0000", w9Max, http.StatusUnauthorized)
		expect(t, app, "+9647099999999", "0000", 1, http.StatusTooManyRequests)
	})

	t.Run("http/whitespace variants share one counter", func(t *testing.T) {
		l, _ := newTestLimiter()
		app := &handlers.AppEnv{DB: db, LoginLimiter: l}
		expect(t, app, "+9647000000401", "0000", 3, http.StatusUnauthorized)
		expect(t, app, "  +9647000000401  ", "0000", 2, http.StatusUnauthorized)
		expect(t, app, " +9647000000401", "4321", 1, http.StatusTooManyRequests)
	})

	t.Run("http/concurrent burst gets exactly the limit", func(t *testing.T) {
		l, _ := newTestLimiter()
		app := &handlers.AppEnv{DB: db, LoginLimiter: l}
		var unauthorized, limited, other atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				switch code, _ := login(t, app, "+9647000000403", "0000"); code {
				case http.StatusUnauthorized:
					unauthorized.Add(1)
				case http.StatusTooManyRequests:
					limited.Add(1)
				default:
					other.Add(1)
				}
			}()
		}
		wg.Wait()
		if unauthorized.Load() != w9Max || limited.Load() != 20-w9Max || other.Load() != 0 {
			t.Errorf("burst of 20: %d×401, %d×429, %d other; want %d×401, %d×429",
				unauthorized.Load(), limited.Load(), other.Load(), w9Max, 20-w9Max)
		}
	})

	t.Run("http/server errors are not counted", func(t *testing.T) {
		l, _ := newTestLimiter()
		closed, err := sql.Open("pgx", "postgres://localhost:1/none")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		closed.Close()
		app := &handlers.AppEnv{DB: closed, LoginLimiter: l}
		expect(t, app, "+9647000000401", "0000", 2*w9Max, http.StatusInternalServerError)
		app.DB = db
		expect(t, app, "+9647000000401", "4321", 1, http.StatusOK)
	})

	t.Run("http/missing limiter fails closed", func(t *testing.T) {
		app := &handlers.AppEnv{DB: db}
		expect(t, app, "+9647000000401", "4321", 1, http.StatusInternalServerError)
	})

	if t.Failed() {
		t.Log("FAIL: Login rate limiting not enforced (see subtest errors above)")
	} else {
		t.Log("PASS: Login rate limiting enforced")
	}
}
