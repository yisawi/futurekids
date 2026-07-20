package database

import (
	"context"
	"database/sql"
	"time"

	"log/slog"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Establishes a connection to the database
func NewConnection(dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}

	// configurations to ensure server resources are not consumed
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	// pinging to the server
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return nil, err
	}

	slog.Info("Successfully connected to the database!")
	return db, nil
}
