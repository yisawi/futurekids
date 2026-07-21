package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)


type AppEnv struct {
	DB *sql.DB
}

type AttendanceEvent struct {
	DeviceSN  string    // device Serial Number
	StudentID string    // student ID
	CheckTime time.Time // time
}

// raw text parsing function
func parseADMSPayload(deviceSN, rawText string) ([]AttendanceEvent, error) {
	/*
		read the text line by line,
		the device may send multiple fingerprints at once if it is offline
	*/
	var events []AttendanceEvent

	lines := strings.Split(strings.TrimSpace(rawText), "\n")
	for _, line := range lines {
		// Separating values ​​based on tabs
		parts := strings.Fields(line)

		if len(parts) < 3 {
			continue // Skip empty or incomplete lines
		}

		studentID := parts[0]
		// Combine date and time into a single string.
		timeStr := fmt.Sprintf("%s %s", parts[1], parts[2])

		// Time Object
		checkTime, err := time.Parse("2006-01-02 15:04:05", timeStr)
		if err != nil {
			// Validation: Log the error and skip the corrupted line instead of stopping the entire process.
			slog.Warn("Invalid time format in payload", "student_id", studentID, "error", err)
		}

		event := AttendanceEvent{
			DeviceSN:  deviceSN,
			StudentID: studentID,
			CheckTime: checkTime,
		}
		events = append(events, event)
	}

	return events, nil
}

// first thing the device will do
// The path that will receive the request from the device.
func (app *AppEnv) ADMSHandler(w http.ResponseWriter, r *http.Request) {
	// Device will send the data from A POST
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Query Parameter extract deviceSN
	deviceSN := r.URL.Query().Get("SN")
	if deviceSN == "" {
		http.Error(w, "Device SN is required", http.StatusBadRequest)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}

	defer r.Body.Close()

	// Passing the raw text to the parsing function
	events, err := parseADMSPayload(deviceSN, string(bodyBytes))
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to parse ADMS payload: %v", err), http.StatusBadRequest)
		return
	}

	insertedCount := 0
	for _, ev := range events {
		err := saveAttendanceLog(app.DB, ev)
		if err != nil {
			// We log the error on the server but do not halt the process
			// (since a single fingerprint might fail while the others succeed).
			slog.Error("Failed to save attendance log", "student_id", ev.StudentID, "error", err)
			continue
		}
		insertedCount++
	}

	slog.Info("ADMS payload processed", "received", len(events), "inserted_or_ignored", insertedCount)

	// Respond to the device acknowledging successful op so it does not re-send the data.
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}


// func to connect with PostegreSQL
func saveAttendanceLog(db *sql.DB, ev AttendanceEvent) error {

		query := `
				  INSERT INTO attendance_logs (student_id, device_id, check_time)
		VALUES ($1, $2, $3)
		ON CONFLICT (student_id, check_time) DO NOTHING;
	`
	// Using a short timeout to protect the server from database hangs
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := db.ExecContext(ctx, query, ev.StudentID, ev.DeviceSN, ev.CheckTime)
	return err

}