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

// Pool holds the connection-pool limits. NewConnection is the only place they are applied.
type Pool struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// DefaultPool caps concurrent queries at 25, keeps 5 warm connections instead of holding
// 25 server slots while idle, and recycles connections every 5 minutes.
var DefaultPool = Pool{
	MaxOpenConns:    25,
	MaxIdleConns:    5,
	ConnMaxLifetime: 5 * time.Minute,
}

// NewConnection opens a PostgreSQL pool for dsn, applies pool, and verifies the server is
// reachable. The DSN is used as given; sslmode is resolved by config.LoadConfig.
func NewConnection(dsn string, pool Pool) (*sql.DB, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("db connection failed: DSN is empty — ensure DATABASE_URL (or DB_URL) is set in your environment")
	}
	redacted, sslmode := describe(dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("db connection failed (sslmode=%s): sql.Open error: %w", sslmode, err)
	}
	db.SetMaxOpenConns(pool.MaxOpenConns)
	db.SetMaxIdleConns(pool.MaxIdleConns)
	db.SetConnMaxLifetime(pool.ConnMaxLifetime)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("db connection failed (%s, sslmode=%s): ping error (check host/credentials/network; if a LOCAL server has no TLS, set DB_SSL_MODE=disable): %w", redacted, sslmode, err)
	}

	slog.Info("Connected to the database",
		"dsn", redacted,
		"sslmode", sslmode,
		"max_open_conns", pool.MaxOpenConns,
		"max_idle_conns", pool.MaxIdleConns,
		"conn_max_lifetime", pool.ConnMaxLifetime.String(),
	)
	return db, nil
}

// describe returns dsn with the password hidden and its sslmode ("driver default" if unset).
func describe(dsn string) (string, string) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "(unparseable DSN)", "unknown"
	}
	mode := u.Query().Get("sslmode")
	if mode == "" {
		mode = "driver default"
	}
	return u.Redacted(), mode
}
