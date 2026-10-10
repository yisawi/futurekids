package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"future_kids/internal/clientip"
	"future_kids/internal/database"

	"github.com/joho/godotenv"
)

type Config struct {
	Port      string
	DBUrl     string
	JWTSecret string
	DBPool    database.Pool
	// FirebaseCredentialsJSON (FIREBASE_CREDENTIALS_JSON) holds the service-account JSON itself
	// and takes precedence; otherwise it is read from FirebaseCredentialsPath
	// (FIREBASE_CREDENTIALS_PATH, default "firebase-credentials.json").
	FirebaseCredentialsJSON string
	FirebaseCredentialsPath string

	// ShutdownTimeout (SHUTDOWN_TIMEOUT) bounds the whole graceful shutdown after SIGTERM/SIGINT.
	ShutdownTimeout time.Duration
	// AbsenceCronSchedule (ABSENCE_CRON_SCHEDULE) is when the daily absence job runs, in Asia/Baghdad.
	AbsenceCronSchedule string
	// OnRailway is set when Railway's environment variables are present; RailwayDraining is
	// RAILWAY_DEPLOYMENT_DRAINING_SECONDS, the time Railway waits after SIGTERM before SIGKILL
	// (0 when unset, which is Railway's default).
	OnRailway       bool
	RailwayDraining time.Duration

	// TrustedProxies (TRUSTED_PROXY_CIDRS) are the proxy addresses whose X-Real-IP and
	// X-Forwarded-For headers are believed. Empty unless set, on Railway too.
	TrustedProxies []netip.Prefix

	// FakeToday (FAKE_TODAY, YYYY-MM-DD) pins the date the server takes as today, for tests only.
	// It is refused on Railway.
	FakeToday string
}

// Defaults for the lifecycle settings. 25s of shutdown fits inside a 30s Railway draining period.
const (
	DefaultShutdownTimeout     = 25 * time.Second
	DefaultAbsenceCronSchedule = "0 12 * * 0-4"
)

// LoadConfig reads the environment. It fails on an invalid sslmode or pool setting so a
// misconfiguration stops startup instead of weakening the database connection.
func LoadConfig() (*Config, error) {
	// godotenv.Load is a no-op when .env is absent; the warning is expected in
	// production environments (e.g. Railway) that inject vars at the OS level.
	if err := godotenv.Load(); err != nil {
		slog.Warn("No .env file found, relying on system environment variables")
	}

	// Railway injects DATABASE_URL; DB_URL is kept as a local-dev fallback.
	dbURL, err := BuildDBURL(getEnvFirstMatch("DATABASE_URL", "DB_URL"), os.Getenv("DB_SSL_MODE"))
	if err != nil {
		return nil, err
	}
	pool, err := loadPool()
	if err != nil {
		return nil, err
	}
	shutdownTimeout := DefaultShutdownTimeout
	if v := strings.TrimSpace(os.Getenv("SHUTDOWN_TIMEOUT")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid SHUTDOWN_TIMEOUT %q: want a positive duration such as 25s", v)
		}
		shutdownTimeout = d
	}
	var railwayDraining time.Duration
	if v := strings.TrimSpace(os.Getenv("RAILWAY_DEPLOYMENT_DRAINING_SECONDS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid RAILWAY_DEPLOYMENT_DRAINING_SECONDS %q: want whole seconds >= 0", v)
		}
		railwayDraining = time.Duration(n) * time.Second
	}
	onRailway := getEnvFirstMatch("RAILWAY_ENVIRONMENT_NAME", "RAILWAY_ENVIRONMENT", "RAILWAY_PROJECT_ID") != ""
	trusted, err := clientip.ParsePrefixes(os.Getenv("TRUSTED_PROXY_CIDRS"))
	if err != nil {
		return nil, fmt.Errorf("invalid TRUSTED_PROXY_CIDRS: %w", err)
	}
	fakeToday := strings.TrimSpace(os.Getenv("FAKE_TODAY"))
	if fakeToday != "" {
		if onRailway {
			return nil, fmt.Errorf("FAKE_TODAY is for tests only and must not be set on Railway")
		}
		d, err := time.Parse("2006-01-02", fakeToday)
		if err != nil || d.Format("2006-01-02") != fakeToday || d.Year() < 2000 || d.Year() > 2100 {
			return nil, fmt.Errorf("invalid FAKE_TODAY %q: want a date YYYY-MM-DD from 2000 to 2100", fakeToday)
		}
	}
	cronSchedule := strings.TrimSpace(os.Getenv("ABSENCE_CRON_SCHEDULE"))
	if cronSchedule == "" {
		cronSchedule = DefaultAbsenceCronSchedule
	}

	return &Config{
		Port:      getEnv("PORT", "8080"),
		DBUrl:     dbURL,
		JWTSecret: getEnv("JWT_SECRET", ""),
		DBPool:    pool,

		FirebaseCredentialsJSON: os.Getenv("FIREBASE_CREDENTIALS_JSON"),
		FirebaseCredentialsPath: firebaseCredentialsPath(),

		ShutdownTimeout:     shutdownTimeout,
		AbsenceCronSchedule: cronSchedule,
		OnRailway:           onRailway,
		RailwayDraining:     railwayDraining,
		TrustedProxies:      trusted,
		FakeToday:           fakeToday,
	}, nil
}

// validSSLModes are the libpq sslmode values accepted by the pgx driver.
var validSSLModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

// BuildDBURL returns the connection URL with its sslmode and session timezone resolved:
//   - an sslmode already in the URL is kept (and validated);
//   - otherwise sslModeEnv (DB_SSL_MODE) is used;
//   - otherwise require, except disable for a loopback host (localhost, 127.0.0.1, ::1),
//     whose traffic never leaves the machine.
//
// The Postgres session TimeZone is pinned to Asia/Baghdad unless the URL sets one, so
// CURRENT_DATE, CURRENT_TIMESTAMP and DEFAULT timestamps do not depend on the server
// default (Railway is UTC). An empty URL is returned as-is for NewConnection to report.
func BuildDBURL(raw, sslModeEnv string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return "", fmt.Errorf("DATABASE_URL/DB_URL must be a postgres:// URL with a host")
	}
	q := u.Query()
	host := u.Hostname()

	mode := q.Get("sslmode")
	source := "DATABASE_URL/DB_URL"
	if mode == "" {
		mode, source = strings.ToLower(strings.TrimSpace(sslModeEnv)), "DB_SSL_MODE"
		if mode == "" {
			mode, source = "require", "default"
			if isLoopback(host) {
				mode = "disable"
			}
		}
	} else if env := strings.TrimSpace(sslModeEnv); env != "" && !strings.EqualFold(env, mode) {
		slog.Warn("DB_SSL_MODE ignored: the database URL already sets sslmode", "url_sslmode", mode, "db_ssl_mode", env)
	}
	if !isValidSSLMode(mode) {
		return "", fmt.Errorf("invalid sslmode %q (from %s): use one of %s", mode, source, strings.Join(validSSLModes, ", "))
	}
	if !isLoopback(host) && (mode == "disable" || mode == "allow" || mode == "prefer") {
		slog.Warn("TLS is not enforced for a remote database; use sslmode=require or stricter", "host", host, "sslmode", mode)
	}
	q.Set("sslmode", mode)

	hasTimezone := false
	for key := range q {
		if strings.EqualFold(key, "timezone") {
			hasTimezone = true
		}
	}
	if !hasTimezone {
		q.Set("timezone", "Asia/Baghdad")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func isValidSSLMode(mode string) bool {
	for _, m := range validSSLModes {
		if mode == m {
			return true
		}
	}
	return false
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// loadPool reads DB_MAX_OPEN_CONNS, DB_MAX_IDLE_CONNS and DB_CONN_MAX_LIFETIME, falling
// back to database.DefaultPool for each one that is unset.
func loadPool() (database.Pool, error) {
	p := database.DefaultPool
	if v := strings.TrimSpace(os.Getenv("DB_MAX_OPEN_CONNS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return p, fmt.Errorf("invalid DB_MAX_OPEN_CONNS %q: want an integer >= 1", v)
		}
		p.MaxOpenConns = n
	}
	if v := strings.TrimSpace(os.Getenv("DB_MAX_IDLE_CONNS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return p, fmt.Errorf("invalid DB_MAX_IDLE_CONNS %q: want an integer >= 0", v)
		}
		p.MaxIdleConns = n
	}
	if p.MaxIdleConns > p.MaxOpenConns {
		return p, fmt.Errorf("DB_MAX_IDLE_CONNS (%d) must not exceed DB_MAX_OPEN_CONNS (%d)", p.MaxIdleConns, p.MaxOpenConns)
	}
	if v := strings.TrimSpace(os.Getenv("DB_CONN_MAX_LIFETIME")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return p, fmt.Errorf("invalid DB_CONN_MAX_LIFETIME %q: want a positive duration such as 5m or 90s", v)
		}
		p.ConnMaxLifetime = d
	}
	return p, nil
}

// DefaultFirebaseCredentialsPath is used when FIREBASE_CREDENTIALS_PATH is unset or empty.
const DefaultFirebaseCredentialsPath = "firebase-credentials.json"

func firebaseCredentialsPath() string {
	if p := strings.TrimSpace(os.Getenv("FIREBASE_CREDENTIALS_PATH")); p != "" {
		return p
	}
	return DefaultFirebaseCredentialsPath
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

// getEnvFirstMatch returns the value of the first key that is set and non-empty.
// This lets production vars (e.g. DATABASE_URL on Railway) transparently take
// precedence over local-dev vars (e.g. DB_URL) without branching in LoadConfig.
func getEnvFirstMatch(keys ...string) string {
	for _, key := range keys {
		if value, exists := os.LookupEnv(key); exists && value != "" {
			return value
		}
	}
	return ""
}
