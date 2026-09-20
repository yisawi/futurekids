package main

import (
	"database/sql"
	"log"
	"os"
	"strings"

	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

// Run this once, manually, BEFORE deploying the bcrypt-based login code.
// Safe to re-run — it skips rows that are already hashed.

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = os.Getenv("DB_URL")
	}
	if dbURL == "" {
		log.Fatal("DATABASE_URL or DB_URL environment variable is required")
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("Failed to ping database: %v", err)
	}

	rows, err := db.Query("SELECT id, pin_code FROM parents")
	if err != nil {
		log.Fatalf("Failed to query parents: %v", err)
	}
	defer rows.Close()

	type Parent struct {
		ID      int
		PinCode string
	}

	var parents []Parent
	for rows.Next() {
		var p Parent
		if err := rows.Scan(&p.ID, &p.PinCode); err != nil {
			log.Fatalf("Failed to scan row: %v", err)
		}
		parents = append(parents, p)
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("Error iterating rows: %v", err)
	}

	totalScanned := len(parents)
	alreadyHashed := 0
	migrated := 0
	failed := 0

	for _, p := range parents {
		// Check if already hashed. Valid bcrypt hashes start with $2a$, $2b$, or $2y$ and are 60 chars long
		if len(p.PinCode) == 60 && (strings.HasPrefix(p.PinCode, "$2a$") || strings.HasPrefix(p.PinCode, "$2b$") || strings.HasPrefix(p.PinCode, "$2y$")) {
			alreadyHashed++
			continue
		}

		hashed, err := bcrypt.GenerateFromPassword([]byte(p.PinCode), bcrypt.DefaultCost)
		if err != nil {
			log.Printf("Failed to hash PIN for parent ID %d: %v", p.ID, err)
			failed++
			continue
		}

		_, err = db.Exec("UPDATE parents SET pin_code = $1 WHERE id = $2", string(hashed), p.ID)
		if err != nil {
			log.Printf("Failed to update parent ID %d: %v", p.ID, err)
			failed++
			continue
		}
		migrated++
	}

	log.Printf("Migration summary:")
	log.Printf("  Total rows scanned: %d", totalScanned)
	log.Printf("  Already hashed (skipped): %d", alreadyHashed)
	log.Printf("  Successfully migrated: %d", migrated)
	log.Printf("  Failed to update: %d", failed)

	if failed > 0 {
		os.Exit(1)
	}
}
