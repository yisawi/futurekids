package warnings

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"future_kids/internal/config"

	"golang.org/x/crypto/bcrypt"
)

// p1Secret returns a random JWT_SECRET of the kind the runbook generates (openssl rand -base64 48).
func p1Secret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// p1Firebase returns a service-account JSON with every field the checks need, minus the keys
// given in drop.
func p1Firebase(t *testing.T, drop ...string) []byte {
	t.Helper()
	var sa map[string]any
	if err := json.Unmarshal([]byte(g3FirebaseJSON(t)), &sa); err != nil {
		t.Fatal(err)
	}
	for _, k := range drop {
		delete(sa, k)
	}
	b, _ := json.Marshal(sa)
	return b
}

// p1Lines returns the JSON log lines of out as maps.
func p1Lines(out string) []map[string]any {
	var lines []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			lines = append(lines, m)
		}
	}
	return lines
}

// p1Problems returns the "problem" of every startup-check line of out at level.
func p1Problems(out, level string) []string {
	var problems []string
	for _, l := range p1Lines(out) {
		if l["level"] == level && strings.HasPrefix(fmt.Sprint(l["msg"]), "Startup check failed") {
			problems = append(problems, fmt.Sprint(l["problem"]))
		}
	}
	return problems
}

const (
	p1JWTShort    = "JWT_SECRET is shorter than 32 characters (generate one with: openssl rand -base64 48)"
	p1JWTWord     = `JWT_SECRET contains a placeholder word such as "secret", "changeme" or "password"; use a random value (openssl rand -base64 48)`
	p1JWTDistinct = "JWT_SECRET has fewer than 12 distinct characters; use a random value (openssl rand -base64 48)"
	p1JWTUnset    = "JWT_SECRET is not set (generate one with: openssl rand -base64 48)"
	p1NoFirebase  = "No Firebase credentials: set FIREBASE_CREDENTIALS_JSON, or FIREBASE_CREDENTIALS_PATH to a readable service-account file"
	p1NoDatabase  = "DATABASE_URL is not set"
	p1Admin       = "An admin account still accepts the default password seeded by migration 000008; rotate it with db/scripts/rotate_admin_password.sql before deploying (docs/PRODUCTION_RUNBOOK.md, step 3)"
	p1AdminsGone  = "The admins table could not be read, so the default-password check could not run; apply all migrations first (migrate up)"
	p1NoAdmin     = "No admin account exists: nobody can sign in to the dashboard"
	p1Refusing    = "Refusing to start in production: startup checks failed; see docs/PRODUCTION_RUNBOOK.md"
	p1Passed      = "Production startup checks passed"
)

// TestP1StartupProblems checks every startup rule on its own and together, and that no message
// ever carries the secret or the credentials it judged.
func TestP1StartupProblems(t *testing.T) {
	good := p1Secret(t)
	fb := p1Firebase(t)
	src := config.FirebaseSourceJSON
	env := func(secret string, firebase []byte, source, dbURL string) config.StartupEnv {
		return config.StartupEnv{JWTSecret: secret, Firebase: firebase, FirebaseSource: source, DatabaseURL: dbURL}
	}
	const db = "postgres://u:p@db.example.com/app"
	for _, c := range []struct {
		name string
		env  config.StartupEnv
		want []string
	}{
		{"everything valid", env(good, fb, src, db), nil},
		{"valid from the file", env(good, fb, config.FirebaseSourceFile, db), nil},
		{"secret unset", env("", fb, src, db), []string{p1JWTUnset}},
		{"secret of 31 characters", env(good[:31], fb, src, db), []string{p1JWTShort}},
		{"secret of exactly 32 characters", env(good[:32], fb, src, db), nil},
		{"31 characters counted as runes", env(strings.Repeat("ب", 20)+"Kq7#vR2m!Tx", fb, src, db), []string{p1JWTShort}},
		{"placeholder word", env("my-jwt-SECRET-"+good[:30], fb, src, db), []string{p1JWTWord}},
		{"changeme in any case", env(good[:30]+"ChangeMe", fb, src, db), []string{p1JWTWord}},
		{"password", env(good[:30]+"password", fb, src, db), []string{p1JWTWord}},
		{"your-key", env("your-"+good[:40], fb, src, db), []string{p1JWTWord}},
		{"11 distinct characters", env(strings.Repeat("abcdefghijk", 4), fb, src, db), []string{p1JWTDistinct}},
		{"12 distinct characters", env(strings.Repeat("abcdefghijkl", 4), fb, src, db), nil},
		{"short, placeholder and repetitive", env("secretsecret", fb, src, db), []string{p1JWTShort, p1JWTWord, p1JWTDistinct}},
		{"no Firebase credentials", env(good, nil, "", db), []string{p1NoFirebase}},
		{"Firebase not JSON", env(good, []byte("not json"), src, db), []string{"FIREBASE_CREDENTIALS_JSON is not valid JSON"}},
		{"Firebase a JSON array", env(good, []byte("[1]"), src, db), []string{"FIREBASE_CREDENTIALS_JSON is not valid JSON"}},
		{"Firebase JSON null", env(good, []byte("null"), src, db), []string{"FIREBASE_CREDENTIALS_JSON is not valid JSON"}},
		{"the test fixture {\"type\":\"service_account\"}", env(good, []byte(`{"type":"service_account"}`), src, db), []string{
			`FIREBASE_CREDENTIALS_JSON: "project_id" is empty`, `FIREBASE_CREDENTIALS_JSON: "client_email" is empty`,
			`FIREBASE_CREDENTIALS_JSON: "private_key" is empty or not a PEM private key`}},
		{"wrong type, from the file", env(good, []byte(strings.Replace(string(fb), `"service_account"`, `"authorized_user"`, 1)), config.FirebaseSourceFile, db),
			[]string{`the file at FIREBASE_CREDENTIALS_PATH: "type" is not "service_account"`}},
		{"private key without the PEM header", env(good, []byte(`{"type":"service_account","project_id":"p","client_email":"c@p.iam.gserviceaccount.com","private_key":"MIIEvQIBADANBg"}`), src, db),
			[]string{`FIREBASE_CREDENTIALS_JSON: "private_key" is empty or not a PEM private key`}},
		{"blank project_id", env(good, []byte(strings.Replace(string(p1Firebase(t, "project_id")), "{", `{"project_id":"  ",`, 1)), src, db),
			[]string{`FIREBASE_CREDENTIALS_JSON: "project_id" is empty`}},
		{"no DATABASE_URL", env(good, fb, src, " "), []string{p1NoDatabase}},
		{"everything wrong at once", env("default1", []byte(`{"type":"x"}`), src, ""), []string{p1JWTShort, p1JWTWord, p1JWTDistinct,
			`FIREBASE_CREDENTIALS_JSON: "type" is not "service_account"`, `FIREBASE_CREDENTIALS_JSON: "project_id" is empty`,
			`FIREBASE_CREDENTIALS_JSON: "client_email" is empty`, `FIREBASE_CREDENTIALS_JSON: "private_key" is empty or not a PEM private key`, p1NoDatabase}},
	} {
		got := config.StartupProblems(c.env)
		if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
		for _, p := range got {
			if c.env.JWTSecret != "" && strings.Contains(p, c.env.JWTSecret) {
				t.Errorf("%s: a message contains the secret", c.name)
			}
			if strings.Contains(p, "PRIVATE KEY-----\n") || strings.Contains(p, "fk-test") || strings.Contains(p, "postgres://") {
				t.Errorf("%s: a message contains a credential: %q", c.name, p)
			}
		}
	}
}

// TestP1IsProduction pins how the production environment is recognised.
func TestP1IsProduction(t *testing.T) {
	for name, want := range map[string]bool{"production": true, " Production ": true, "PRODUCTION": true, "staging": false, "test": false, "": false, "production-2": false, "prod": false} {
		if got := config.IsProduction(name); got != want {
			t.Errorf("IsProduction(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestP1ReadStartupEnv checks where the checks read their input: FIREBASE_CREDENTIALS_JSON wins
// over the file, the file is read when it is unset, DB_URL backs up DATABASE_URL, and a missing
// file gives the no-credentials problem without naming its path.
func TestP1ReadStartupEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p1-service-account.json")
	if err := os.WriteFile(path, p1Firebase(t), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAILWAY_ENVIRONMENT_NAME", "production")
	t.Setenv("JWT_SECRET", "p1-short")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DB_URL", "postgres://u:p@localhost/x")
	t.Setenv("FIREBASE_CREDENTIALS_PATH", path)
	t.Setenv("FIREBASE_CREDENTIALS_JSON", `{"type":"service_account"}`)
	env := config.ReadStartupEnv()
	if !env.Production || env.JWTSecret != "p1-short" || env.DatabaseURL == "" || env.FirebaseSource != config.FirebaseSourceJSON || string(env.Firebase) != `{"type":"service_account"}` {
		t.Errorf("with both Firebase sources: %+v", env)
	}
	t.Setenv("FIREBASE_CREDENTIALS_JSON", "")
	env = config.ReadStartupEnv()
	if env.FirebaseSource != config.FirebaseSourceFile || len(config.StartupProblems(config.StartupEnv{JWTSecret: p1Secret(t), Firebase: env.Firebase, FirebaseSource: env.FirebaseSource, DatabaseURL: "x"})) != 0 {
		t.Errorf("the file was not read as the source: %q", env.FirebaseSource)
	}
	missing := filepath.Join(dir, "nowhere", "p1-missing.json")
	t.Setenv("FIREBASE_CREDENTIALS_PATH", missing)
	t.Setenv("RAILWAY_ENVIRONMENT_NAME", "staging")
	env = config.ReadStartupEnv()
	problems := config.StartupProblems(env)
	if env.Production || env.FirebaseSource != "" || !strings.Contains(strings.Join(problems, "\n"), p1NoFirebase) {
		t.Errorf("missing file: %+v %q", env, problems)
	}
	for _, p := range problems {
		if strings.Contains(p, missing) || strings.Contains(p, "p1-short") {
			t.Errorf("a message contains the path or the secret: %q", p)
		}
	}
}

// TestP1SeededAdmin checks the default-password check against a real database: a fresh one fails
// it, the rotation script fixes it, a renamed account is still caught, an empty table only warns
// and a missing table fails (the check could not run).
func TestP1SeededAdmin(t *testing.T) {
	db, _ := setupThrowawayDB(t, "p1admin")
	ctx := context.Background()
	check := func(name, wantProblem, wantWarning string) {
		t.Helper()
		problem, warning := config.SeededAdminProblem(ctx, db)
		if problem != wantProblem || warning != wantWarning {
			t.Errorf("%s: problem %q warning %q, want %q %q", name, problem, warning, wantProblem, wantWarning)
		}
	}
	check("fresh database", p1Admin, "")

	script, err := os.ReadFile(filepath.Join("..", "..", "db", "scripts", "rotate_admin_password.sql"))
	if err != nil {
		t.Fatal(err)
	}
	rotated := strings.ReplaceAll(string(script), "NEW_PASSWORD_HERE", "P1-rotated#Pw-7xQ2")
	if _, err := db.Exec(rotated); err != nil {
		t.Fatalf("rotation script: %v", err)
	}
	check("after the rotation script", "", "")
	var hash string
	db.QueryRow(`SELECT password_hash FROM admins WHERE username = 'admin'`).Scan(&hash)
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("P1-rotated#Pw-7xQ2")) != nil {
		t.Fatalf("the rotated hash does not verify with Go's bcrypt")
	}

	defaultHash, _ := bcrypt.GenerateFromPassword([]byte(config.SeededAdminPassword), bcrypt.MinCost)
	k1Exec(t, db, fmt.Sprintf(`INSERT INTO admins (username, password_hash) VALUES ('owner', '%s')`, defaultHash))
	check("a renamed account with the default password", p1Admin, "")
	k1Exec(t, db, `DELETE FROM admins`)
	check("no admin account", "", p1NoAdmin)
	k1Exec(t, db, `ALTER TABLE admins RENAME TO admins_p1`)
	check("admins table missing", p1AdminsGone, "")
}

// TestP1ProductionStartup runs the real binary: in production it refuses to start and lists
// every problem in one boot without printing a value; with a safe configuration it starts and
// says so; on Staging and locally the same problems are only warnings.
func TestP1ProductionStartup(t *testing.T) {
	const weak = "p1-weak-value"
	badFirebase := `{"type":"service_account"}`
	wantBad := []string{p1JWTShort, p1JWTDistinct, `FIREBASE_CREDENTIALS_JSON: "project_id" is empty`,
		`FIREBASE_CREDENTIALS_JSON: "client_email" is empty`, `FIREBASE_CREDENTIALS_JSON: "private_key" is empty or not a PEM private key`, p1Admin}
	noValues := func(t *testing.T, out string) {
		t.Helper()
		for _, v := range []string{weak, badFirebase, `\"type\":\"service_account\"`, "$2a$10$"} {
			if strings.Contains(out, v) {
				t.Errorf("the output contains a configuration value (%q)", v)
			}
		}
	}

	t.Run("production refuses an unsafe configuration and lists every problem", func(t *testing.T) {
		_, dsn := setupThrowawayDB(t, "p1prodbad")
		srv := g3Start(t, dsn, g3FreePort(t), false, "RAILWAY_ENVIRONMENT_NAME=production", "JWT_SECRET="+weak, "FIREBASE_CREDENTIALS_JSON="+badFirebase)
		code, _ := srv.wait(t, 20*time.Second)
		out := srv.out.String()
		if code != 1 {
			t.Fatalf("exit %d, want 1:\n%s", code, out)
		}
		if got := p1Problems(out, "ERROR"); strings.Join(got, "\n") != strings.Join(wantBad, "\n") {
			t.Errorf("problems logged:\n%q\nwant:\n%q", got, wantBad)
		}
		msgs := strings.Join(srv.out.messages(), "\n")
		if !strings.Contains(msgs, p1Refusing) || strings.Contains(msgs, "Server started") || strings.Contains(msgs, p1Passed) {
			t.Errorf("messages:\n%s", msgs)
		}
		noValues(t, out)
	})

	t.Run("production with FAKE_TODAY lists the checks with the configuration error", func(t *testing.T) {
		_, dsn := setupThrowawayDB(t, "p1prodfake")
		srv := g3Start(t, dsn, g3FreePort(t), false, "RAILWAY_ENVIRONMENT_NAME=production", "JWT_SECRET="+weak, "FAKE_TODAY=2026-09-24")
		code, _ := srv.wait(t, 20*time.Second)
		out := srv.out.String()
		if code != 1 || !strings.Contains(out, "FAKE_TODAY is for tests only and must not be set on Railway") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if got := p1Problems(out, "ERROR"); strings.Join(got, "\n") != p1JWTShort+"\n"+p1JWTDistinct {
			t.Errorf("problems logged with the configuration error: %q", got)
		}
		noValues(t, out)
	})

	t.Run("production without DATABASE_URL stops before connecting", func(t *testing.T) {
		srv := g3Start(t, "", g3FreePort(t), false, "RAILWAY_ENVIRONMENT_NAME=production", "JWT_SECRET="+p1Secret(t))
		code, _ := srv.wait(t, 20*time.Second)
		out := srv.out.String()
		msgs := strings.Join(srv.out.messages(), "\n")
		if code != 1 || strings.Join(p1Problems(out, "ERROR"), "\n") != p1NoDatabase || !strings.Contains(msgs, p1Refusing) || strings.Contains(msgs, "Failed to connect") {
			t.Errorf("exit %d:\n%s", code, out)
		}
	})

	t.Run("production starts with a safe configuration", func(t *testing.T) {
		db, dsn := setupThrowawayDB(t, "p1prodok")
		hash, _ := bcrypt.GenerateFromPassword([]byte(a14AdminPassword), bcrypt.DefaultCost)
		k1Exec(t, db, fmt.Sprintf(`UPDATE admins SET password_hash = '%s' WHERE username = 'admin'`, hash))
		secret := p1Secret(t)
		srv := g3Start(t, dsn, g3FreePort(t), true, "RAILWAY_ENVIRONMENT_NAME=production", "JWT_SECRET="+secret,
			"RAILWAY_DEPLOYMENT_DRAINING_SECONDS=30", "ABSENCE_CRON_SCHEDULE=0 0 1 1 *")
		if r := a14Do(t, srv, "POST", "/api/admin/login", nil, map[string]string{"username": "admin", "password": a14AdminPassword}); r.status != 200 {
			t.Errorf("admin login: %d %s", r.status, r.body)
		}
		out := srv.out.String()
		if p := append(p1Problems(out, "ERROR"), p1Problems(out, "WARN")...); len(p) != 0 {
			t.Errorf("problems logged: %q", p)
		}
		if srv.out.index(p1Passed) < 0 || srv.out.index(p1Passed) > srv.out.index("Server started") {
			t.Errorf("no %q before Server started:\n%s", p1Passed, out)
		}
		var proxyLevel string
		for _, l := range p1Lines(out) {
			if strings.HasPrefix(fmt.Sprint(l["msg"]), "TRUSTED_PROXY_CIDRS is not set") {
				proxyLevel = fmt.Sprint(l["level"])
				if !strings.HasSuffix(fmt.Sprint(l["msg"]), "(docs/PRODUCTION_RUNBOOK.md, step 5)") {
					t.Errorf("the proxy line has no runbook pointer: %v", l["msg"])
				}
			}
		}
		if proxyLevel != "ERROR" {
			t.Errorf("TRUSTED_PROXY_CIDRS line at level %q, want ERROR in production", proxyLevel)
		}
		if strings.Contains(out, secret) || strings.Contains(out, "Railway will SIGKILL") {
			t.Errorf("unexpected output:\n%s", out)
		}
	})

	t.Run("staging only warns about an unsafe configuration", func(t *testing.T) {
		// Credentials the Firebase SDK accepts but the checks reject: {"type":"service_account"}
		// alone makes the SDK itself stop the server, as it always has.
		_, dsn := setupThrowawayDB(t, "p1staging")
		srv := g3Start(t, dsn, g3FreePort(t), true, "RAILWAY_ENVIRONMENT_NAME=staging", "JWT_SECRET="+weak, "FIREBASE_CREDENTIALS_JSON="+string(p1Firebase(t, "client_email")),
			"RAILWAY_DEPLOYMENT_DRAINING_SECONDS=30", "ABSENCE_CRON_SCHEDULE=0 0 1 1 *")
		if r := a14Do(t, srv, "GET", "/health", nil, nil); r.status != 200 {
			t.Errorf("health %d", r.status)
		}
		out := srv.out.String()
		want := []string{p1JWTShort, p1JWTDistinct, `FIREBASE_CREDENTIALS_JSON: "client_email" is empty`, p1Admin}
		if got := p1Problems(out, "WARN"); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("warnings:\n%q\nwant:\n%q", got, want)
		}
		if strings.Contains(out, `"level":"ERROR"`) || srv.out.index(p1Passed) >= 0 {
			t.Errorf("staging logged an ERROR or claimed production checks:\n%s", out)
		}
		if !strings.Contains(out, `"level":"WARN","msg":"TRUSTED_PROXY_CIDRS is not set`) {
			t.Errorf("the proxy line on staging is not a WARN")
		}
		noValues(t, out)
	})

	t.Run("locally the default admin is a warning", func(t *testing.T) {
		_, dsn := setupThrowawayDB(t, "p1local")
		srv := g3Start(t, dsn, g3FreePort(t), true, "ABSENCE_CRON_SCHEDULE=0 0 1 1 *")
		out := srv.out.String()
		got := p1Problems(out, "WARN")
		if len(got) == 0 || got[len(got)-1] != p1Admin || strings.Contains(out, `"level":"ERROR"`) {
			t.Errorf("local warnings %q:\n%s", got, out)
		}
	})
}
