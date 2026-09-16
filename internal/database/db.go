package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// NewConnection opens and verifies a PostgreSQL connection using the provided
// DSN (typically the value of DATABASE_URL on Railway).
//
// Production behaviour:
//   - Fails fast with the exact driver error if the DSN is empty or malformed.
//   - Appends sslmode=disable when no sslmode is present, which is required for
//     Railway's internal private-network connections.
//   - Returns a wrapped error so callers always see the root driver message.
func NewConnection(dsn string) (*sql.DB, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("db connection failed: DSN is empty — ensure DATABASE_URL (or DB_URL) is set in your environment")
	}

	dsn, err := ensureSSLMode(dsn)
	if err != nil {
		return nil, fmt.Errorf("db connection failed: could not parse DSN: %w", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("db connection failed: sql.Open error: %w", err)
	}

	// Connection-pool tuning — prevents exhausting server resources.
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Verify the connection is actually reachable before returning.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		// Close the pool so the caller doesn't hold an unusable handle.
		_ = db.Close()
		return nil, fmt.Errorf("db connection failed: ping error (check host/credentials/network): %w", err)
	}

	slog.Info("Successfully connected to the database")
	return db, nil
}

// ensureSSLMode appends sslmode=disable to the DSN when no sslmode query
// parameter is present. Railway's internal network does not use TLS by
// default, so the driver requires this to avoid a TLS handshake error.
func ensureSSLMode(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}

	q := u.Query()
	if q.Get("sslmode") == "" {
		q.Set("sslmode", "disable")
		u.RawQuery = q.Encode()
	}

	return u.String(), nil
}
