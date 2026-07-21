package hardware

import (
	"fmt"
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
	lines := strings.Split(strings.TrimSpace(rawText), "\n")

	for _, line := range lines {
		// Separating values ​​based on tabs
		parts := strings.Fields(line)
		if len(parts) < 2 {
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
