package warnings

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"future_kids/internal/auth"
	"future_kids/internal/handlers"

	"github.com/golang-jwt/jwt/v5"
)

const w8Secret = "w8-test-secret"

// w8Token signs claims with the test secret. Base claims are valid; overrides replace or,
// with a nil value marker (w8Delete), remove them.
var w8Delete = struct{}{}

func w8Token(t *testing.T, method jwt.SigningMethod, base, overrides map[string]any) string {
	t.Helper()
	claims := jwt.MapClaims{}
	for k, v := range base {
		claims[k] = v
	}
	for k, v := range overrides {
		if v == w8Delete {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	signed, err := jwt.NewWithClaims(method, claims).SignedString([]byte(w8Secret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

// TestW8TypeAssertions verifies audit warning W8: every JWT claim the server reads is
// type-checked, so a validly signed token with missing or mistyped claims gets 401 with a
// WARN log naming the claim — never a panic — and expiry/issued-at/algorithm are enforced.
func TestW8TypeAssertions(t *testing.T) {
	capture := &w10Capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })
	auth.InitAuth(w8Secret)

	now := time.Now()
	parentBase := map[string]any{"parent_id": 7, "phone": "+9647700000801", "role": "parent", "exp": now.Add(time.Hour).Unix(), "iat": now.Unix()}
	adminBase := map[string]any{"username": "admin", "role": "admin", "exp": now.Add(time.Hour).Unix(), "iat": now.Unix()}

	var seenParentID atomic.Int64
	parentRoute := handlers.AuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		id, _ := r.Context().Value(handlers.ParentIDKey).(int)
		seenParentID.Store(int64(id))
		w.WriteHeader(http.StatusOK)
	})
	adminRoute := handlers.AdminMiddleware(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	// call runs h and converts a panic into a result instead of crashing the test binary.
	call := func(h http.HandlerFunc, token string) (code int, panicked any, logs []w10Log) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		before := len(capture.snapshot())
		rec := httptest.NewRecorder()
		func() {
			defer func() { panicked = recover() }()
			h(rec, req)
		}()
		return rec.Code, panicked, capture.snapshot()[before:]
	}

	cases := []struct {
		name      string
		route     string
		method    jwt.SigningMethod
		overrides map[string]any
		want      int
		claim     string // claim named in the WARN log ("" = library-level rejection)
		reason    string // substring of the logged error
	}{
		{"valid parent token", "parent", jwt.SigningMethodHS256, nil, 200, "", ""},
		{"phone as number (claim is never read)", "parent", jwt.SigningMethodHS256, map[string]any{"phone": 12345}, 200, "", ""},
		{"legacy token without parent_id", "parent", jwt.SigningMethodHS256, map[string]any{"parent_id": w8Delete}, 401, "parent_id", "is missing"},
		{"parent_id as string", "parent", jwt.SigningMethodHS256, map[string]any{"parent_id": "7"}, 401, "parent_id", "has type string, want number"},
		{"parent_id as bool", "parent", jwt.SigningMethodHS256, map[string]any{"parent_id": true}, 401, "parent_id", "has type bool"},
		{"parent_id null", "parent", jwt.SigningMethodHS256, map[string]any{"parent_id": nil}, 401, "parent_id", "has type <nil>"},
		{"parent_id as object", "parent", jwt.SigningMethodHS256, map[string]any{"parent_id": map[string]any{"id": 7}}, 401, "parent_id", "has type map"},
		{"parent_id as array", "parent", jwt.SigningMethodHS256, map[string]any{"parent_id": []any{7}}, 401, "parent_id", "has type []interface"},
		{"parent_id fractional", "parent", jwt.SigningMethodHS256, map[string]any{"parent_id": 7.5}, 401, "parent_id", "want a positive integer"},
		{"parent_id zero", "parent", jwt.SigningMethodHS256, map[string]any{"parent_id": 0}, 401, "parent_id", "want a positive integer"},
		{"parent_id negative", "parent", jwt.SigningMethodHS256, map[string]any{"parent_id": -3}, 401, "parent_id", "want a positive integer"},
		{"parent_id huge", "parent", jwt.SigningMethodHS256, map[string]any{"parent_id": 1e20}, 401, "parent_id", "want a positive integer"},
		{"role missing", "parent", jwt.SigningMethodHS256, map[string]any{"role": w8Delete}, 401, "role", "is missing"},
		{"role as number", "parent", jwt.SigningMethodHS256, map[string]any{"role": 1}, 401, "role", "has type float64, want string"},
		{"role empty", "parent", jwt.SigningMethodHS256, map[string]any{"role": ""}, 401, "role", "is empty"},
		{"admin token on parent route", "parent", jwt.SigningMethodHS256, map[string]any{"role": "admin"}, 403, "", ""},
		{"exp as string", "parent", jwt.SigningMethodHS256, map[string]any{"exp": "tomorrow"}, 401, "", ""},
		{"exp missing (never-expiring token)", "parent", jwt.SigningMethodHS256, map[string]any{"exp": w8Delete}, 401, "", ""},
		{"exp in the past", "parent", jwt.SigningMethodHS256, map[string]any{"exp": now.Add(-time.Minute).Unix()}, 401, "", ""},
		{"iat as string", "parent", jwt.SigningMethodHS256, map[string]any{"iat": "now"}, 401, "", ""},
		{"iat in the future", "parent", jwt.SigningMethodHS256, map[string]any{"iat": now.Add(time.Hour).Unix()}, 401, "", ""},
		{"HS512 instead of HS256", "parent", jwt.SigningMethodHS512, nil, 401, "", ""},

		{"valid admin token", "admin", jwt.SigningMethodHS256, nil, 200, "", ""},
		{"username as number (claim is never read)", "admin", jwt.SigningMethodHS256, map[string]any{"username": 42}, 200, "", ""},
		{"admin role missing", "admin", jwt.SigningMethodHS256, map[string]any{"role": w8Delete}, 401, "role", "is missing"},
		{"admin role as array", "admin", jwt.SigningMethodHS256, map[string]any{"role": []any{"admin"}}, 401, "role", "has type []interface"},
		{"parent token on admin route", "admin", jwt.SigningMethodHS256, map[string]any{"role": "parent"}, 403, "", ""},
		{"admin exp missing", "admin", jwt.SigningMethodHS256, map[string]any{"exp": w8Delete}, 401, "", ""},
	}

	for _, c := range cases {
		t.Run(c.route+"/"+c.name, func(t *testing.T) {
			h, base, middleware := parentRoute, parentBase, "AuthMiddleware"
			if c.route == "admin" {
				h, base, middleware = adminRoute, adminBase, "AdminMiddleware"
			}
			seenParentID.Store(0)
			code, panicked, logs := call(h, w8Token(t, c.method, base, c.overrides))
			if panicked != nil {
				t.Fatalf("PANIC: %v", panicked)
			}
			if code != c.want {
				t.Errorf("HTTP %d, want %d", code, c.want)
			}
			if c.want == 200 && c.route == "parent" && seenParentID.Load() != 7 {
				t.Errorf("handler saw parent_id %d, want 7", seenParentID.Load())
			}
			if c.want == 200 {
				return
			}
			var warn *w10Log
			for i := range logs {
				if logs[i].Level == slog.LevelWarn && strings.HasPrefix(logs[i].Msg, middleware+":") {
					warn = &logs[i]
				}
			}
			if warn == nil {
				t.Fatalf("rejection without a WARN log from %s (logs %v)", middleware, logs)
			}
			if c.claim != "" && (warn.Attrs["claim"] != c.claim || !strings.Contains(warn.Attrs["error"], c.reason)) {
				t.Errorf("WARN %q claim=%q error=%q, want claim %q and error containing %q", warn.Msg, warn.Attrs["claim"], warn.Attrs["error"], c.claim, c.reason)
			}
			if warn.Attrs["path"] == "" || warn.Attrs["remote_addr"] == "" {
				t.Errorf("WARN missing request context: %v", warn.Attrs)
			}
		})
	}

	t.Run("concurrent malformed tokens never panic", func(t *testing.T) {
		tokens := []struct {
			token string
			want  int
		}{
			{w8Token(t, jwt.SigningMethodHS256, parentBase, nil), 200},
			{w8Token(t, jwt.SigningMethodHS256, parentBase, map[string]any{"parent_id": w8Delete}), 401},
			{w8Token(t, jwt.SigningMethodHS256, parentBase, map[string]any{"parent_id": "7"}), 401},
			{w8Token(t, jwt.SigningMethodHS256, parentBase, map[string]any{"role": 1}), 401},
			{w8Token(t, jwt.SigningMethodHS256, parentBase, map[string]any{"exp": w8Delete}), 401},
		}
		var wg sync.WaitGroup
		var panics, wrong atomic.Int32
		for i := 0; i < 250; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				tk := tokens[i%len(tokens)]
				code, panicked, _ := call(parentRoute, tk.token)
				if panicked != nil {
					panics.Add(1)
				} else if code != tk.want {
					wrong.Add(1)
				}
			}(i)
		}
		wg.Wait()
		if panics.Load() != 0 || wrong.Load() != 0 {
			t.Errorf("250 concurrent requests: %d panics, %d wrong status codes", panics.Load(), wrong.Load())
		}
	})

	t.Run("no panic recorded in logs", func(t *testing.T) {
		for _, l := range capture.snapshot() {
			if strings.Contains(strings.ToLower(l.Msg+fmt.Sprint(l.Attrs)), "panic") {
				t.Errorf("panic in logs: %s %v", l.Msg, l.Attrs)
			}
		}
	})

	t.Run("source: no unchecked type assertion in non-test code", func(t *testing.T) {
		unchecked := regexp.MustCompile(`[\])]\.\([A-Za-z0-9_.*\[\]]+\)`)
		checked := regexp.MustCompile(`, ok :?= |, ok\)`)
		files, _ := filepath.Glob(filepath.Join("..", "..", "internal", "*", "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(src), "\n") {
				if unchecked.MatchString(line) && !checked.MatchString(line) && !strings.Contains(line, ".(type)") {
					t.Errorf("%s:%d: type assertion without the two-value form: %s", filepath.Base(f), i+1, strings.TrimSpace(line))
				}
			}
		}
	})

	if t.Failed() {
		t.Log("FAIL: Malformed JWT claims are not handled safely (see subtest errors above)")
	} else {
		t.Log("PASS: Every JWT claim is type-checked; malformed tokens get 401, never a panic")
	}
}
