package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Struct that will be JSON for Flutter
type AttendanceRecord struct {
	StudentID int       `json:"student_id"` // change it to 'int' based on our DB
	FullName  string    `json:"full_name"`
	CheckTime time.Time `json:"check_time"`
	DeviceSN  string    `json:"device_sn"`
}

// this func is now bound to AppEnv to access app.DB.
func (app *AppEnv) GetTodayAttendanceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Query for GET Today's Data with Student names
	query := `
		SELECT s.id, s.full_name, a.check_time, a.device_sn
		FROM attendance_logs a
		JOIN students s ON a.student_id = s.id
		WHERE DATE(a.check_time) = CURRENT_DATE
		ORDER BY a.check_time DESC;
	`

	// Set timeout for Query
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := app.DB.QueryContext(ctx, query)
	if err != nil {
		slog.Error("Database query failed", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var records []AttendanceRecord

	// Read the data line-by-line
	for rows.Next() {
		var rec AttendanceRecord
		if err := rows.Scan(&rec.StudentID, &rec.FullName, &rec.CheckTime, &rec.DeviceSN); err != nil {
			slog.Error("Row scan failed", "error", err)
			continue
		}
		records = append(records, rec)
	}

	if err := rows.Err(); err != nil {
		slog.Error("Error during rows iteration", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// This step ensures an empty array [] is sent instead of null if no one attends today.
	if records == nil {
		records = []AttendanceRecord{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(records); err != nil {
		slog.Error("Failed to encode json", "error", err)
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}
