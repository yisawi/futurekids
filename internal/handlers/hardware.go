package hardware

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

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
			return nil, fmt.Errorf("invalid time format for student %s: %v", studentID, err)
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
func ADMSHandler(w http.ResponseWriter, r *http.Request) {
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

	// Temporary printout to verify successful unpacking (will later be replaced by saving to the database)
	for _, ev := range events {
		fmt.Printf("Successful Parsed Event -> Device: %s | Student: %s | Time: %v\n", ev.DeviceSN, ev.StudentID, ev.CheckTime)
	}

	// Respond to the device acknowledging successful op so it does not re-send the data.
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
