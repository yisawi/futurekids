package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// Startup checks. In Production (IsProduction) every problem stops the server; elsewhere each
// one is only logged as a WARN, so Staging and local runs never fail on them. Messages name the
// variable and the rule, never a value, path, hash or token.

// MinJWTSecretLength and MinJWTSecretDistinct are the JWT_SECRET rules.
const (
	MinJWTSecretLength   = 32
	MinJWTSecretDistinct = 12
)

// jwtPlaceholders are fragments of placeholder secrets, matched case-insensitively anywhere in
// JWT_SECRET. A random value never contains them in practice.
var jwtPlaceholders = []string{"secret", "changeme", "change-me", "change_me", "password", "placeholder", "example", "default", "your-"}

// SeededAdminPassword is the password migration 000008 seeds for the admin account (its header
// comment documents it). Production refuses to start while any admin account still accepts it.
const SeededAdminPassword = "admin123"

// MaxAdminsChecked bounds the bcrypt comparisons of the seeded-admin check.
const MaxAdminsChecked = 20

const adminsUnreadable = "The admins table could not be read, so the default-password check could not run; apply all migrations first (migrate up)"

// Firebase credential sources, as named in the messages.
const (
	FirebaseSourceJSON = "FIREBASE_CREDENTIALS_JSON"
	FirebaseSourceFile = "the file at FIREBASE_CREDENTIALS_PATH"
)

// StartupEnv is what the startup checks read from the environment.
type StartupEnv struct {
	Production     bool
	JWTSecret      string
	DatabaseURL    string
	Firebase       []byte // the service-account JSON, from FirebaseSource
	FirebaseSource string // FirebaseSourceJSON, FirebaseSourceFile, or "" when there is none
}

// IsProduction reports whether RAILWAY_ENVIRONMENT_NAME names the production environment.
func IsProduction(environmentName string) bool {
	return strings.EqualFold(strings.TrimSpace(environmentName), "production")
}

// ReadStartupEnv reads the variables the startup checks need and the Firebase credentials,
// with the same precedence as the server: FIREBASE_CREDENTIALS_JSON, else the file at
// FIREBASE_CREDENTIALS_PATH (default firebase-credentials.json).
func ReadStartupEnv() StartupEnv {
	env := StartupEnv{
		Production:  IsProduction(os.Getenv("RAILWAY_ENVIRONMENT_NAME")),
		JWTSecret:   os.Getenv("JWT_SECRET"),
		DatabaseURL: getEnvFirstMatch("DATABASE_URL", "DB_URL"),
	}
	if v := os.Getenv("FIREBASE_CREDENTIALS_JSON"); v != "" {
		env.Firebase, env.FirebaseSource = []byte(v), FirebaseSourceJSON
	} else if b, err := os.ReadFile(firebaseCredentialsPath()); err == nil {
		env.Firebase, env.FirebaseSource = b, FirebaseSourceFile
	}
	return env
}

// StartupProblems returns every configuration problem of env, in a fixed order.
func StartupProblems(env StartupEnv) []string {
	var problems []string
	problems = append(problems, jwtSecretProblems(env.JWTSecret)...)
	problems = append(problems, firebaseProblems(env.Firebase, env.FirebaseSource)...)
	if strings.TrimSpace(env.DatabaseURL) == "" {
		problems = append(problems, "DATABASE_URL is not set")
	}
	return problems
}

func jwtSecretProblems(secret string) []string {
	const generate = "openssl rand -base64 48"
	if secret == "" {
		return []string{"JWT_SECRET is not set (generate one with: " + generate + ")"}
	}
	var problems []string
	if utf8.RuneCountInString(secret) < MinJWTSecretLength {
		problems = append(problems, "JWT_SECRET is shorter than 32 characters (generate one with: "+generate+")")
	}
	lower := strings.ToLower(secret)
	for _, word := range jwtPlaceholders {
		if strings.Contains(lower, word) {
			problems = append(problems, `JWT_SECRET contains a placeholder word such as "secret", "changeme" or "password"; use a random value (`+generate+")")
			break
		}
	}
	distinct := map[rune]bool{}
	for _, r := range secret {
		distinct[r] = true
	}
	if len(distinct) < MinJWTSecretDistinct {
		problems = append(problems, "JWT_SECRET has fewer than 12 distinct characters; use a random value ("+generate+")")
	}
	return problems
}

func firebaseProblems(raw []byte, source string) []string {
	if source == "" {
		return []string{"No Firebase credentials: set FIREBASE_CREDENTIALS_JSON, or FIREBASE_CREDENTIALS_PATH to a readable service-account file"}
	}
	var sa map[string]any
	if err := json.Unmarshal(raw, &sa); err != nil || sa == nil {
		return []string{source + " is not valid JSON"}
	}
	text := func(key string) string {
		if s, ok := sa[key].(string); ok {
			return strings.TrimSpace(s)
		}
		return ""
	}
	var problems []string
	if text("type") != "service_account" {
		problems = append(problems, source+`: "type" is not "service_account"`)
	}
	for _, key := range []string{"project_id", "client_email"} {
		if text(key) == "" {
			problems = append(problems, source+`: "`+key+`" is empty`)
		}
	}
	if key := text("private_key"); !strings.Contains(key, "-----BEGIN") || !strings.Contains(key, "PRIVATE KEY-----") {
		problems = append(problems, source+`: "private_key" is empty or not a PEM private key`)
	}
	return problems
}

// SeededAdminProblem checks whether any admin account, whatever its username, still accepts
// SeededAdminPassword. It returns a problem when one does or when the admins table cannot be read
// (the check could not run), and only a warning when there is no admin account at all.
func SeededAdminProblem(ctx context.Context, db *sql.DB) (problem, warning string) {
	rows, err := db.QueryContext(ctx, `SELECT password_hash FROM admins ORDER BY id LIMIT $1`, MaxAdminsChecked)
	if err != nil {
		return adminsUnreadable, ""
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return adminsUnreadable, ""
		}
		hashes = append(hashes, h)
	}
	if rows.Err() != nil {
		return adminsUnreadable, ""
	}
	if len(hashes) == 0 {
		return "", "No admin account exists: nobody can sign in to the dashboard"
	}
	for _, h := range hashes {
		if bcrypt.CompareHashAndPassword([]byte(h), []byte(SeededAdminPassword)) == nil {
			return "An admin account still accepts the default password seeded by migration 000008; rotate it with db/scripts/rotate_admin_password.sql before deploying (docs/PRODUCTION_RUNBOOK.md, step 3)", ""
		}
	}
	return "", ""
}
