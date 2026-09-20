package handlers

import (
	"reflect"
	"testing"
	"time"
)

func TestParseATTLOG(t *testing.T) {
	deviceSN := "TEST_DEVICE_001"
	
	// Create stable timestamps for verification
	ts1, _ := time.Parse("2006-01-02 15:04:05", "2023-10-15 08:30:00")
	ts2, _ := time.Parse("2006-01-02 15:04:05", "2023-10-15 08:35:10")

	tests := []struct {
		name     string
		rawBody  string
		expected []AttendanceEvent
	}{
		{
			name: "Valid multi-line Positional format",
			rawBody: "12345\t2023-10-15 08:30:00\t1\t1\n" +
				"67890\t2023-10-15 08:35:10\t1\t1\n",
			expected: []AttendanceEvent{
				{DeviceSN: deviceSN, StudentID: "12345", CheckTime: ts1},
				{DeviceSN: deviceSN, StudentID: "67890", CheckTime: ts2},
			},
		},
		{
			name: "Valid multi-line Key-Value format",
			rawBody: "PIN=12345\tDateTime=2023-10-15 08:30:00\n" +
				"PIN=67890\tDateTime=2023-10-15 08:35:10\n",
			expected: []AttendanceEvent{
				{DeviceSN: deviceSN, StudentID: "12345", CheckTime: ts1},
				{DeviceSN: deviceSN, StudentID: "67890", CheckTime: ts2},
			},
		},
		{
			name: "Missing columns / malformed (Positional)",
			rawBody: "12345\n", // only 1 column, should be skipped
			expected: nil,
		},
		{
			name: "Missing columns / malformed (Key-Value)",
			rawBody: "PIN=12345\n", // missing DateTime, should be skipped
			expected: nil,
		},
		{
			name: "Invalid date format",
			rawBody: "12345\t15-10-2023 08:30:00\t1\t1\n", // should fail time.Parse
			expected: nil,
		},
		{
			name: "Completely garbage data",
			rawBody: "this is garbage\nhello world\n",
			expected: nil,
		},
		{
			name: "Mixed valid and invalid rows",
			rawBody: "garbage data\n" +
				"12345\t2023-10-15 08:30:00\t1\t1\n" +
				"PIN=67890\n" +
				"PIN=67890\tDateTime=2023-10-15 08:35:10\n",
			expected: []AttendanceEvent{
				{DeviceSN: deviceSN, StudentID: "12345", CheckTime: ts1},
				{DeviceSN: deviceSN, StudentID: "67890", CheckTime: ts2},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := parseATTLOG(deviceSN, tt.rawBody)
			
			if len(actual) == 0 && len(tt.expected) == 0 {
				return // both empty, test passes
			}

			if !reflect.DeepEqual(actual, tt.expected) {
				t.Errorf("parseATTLOG() = %v, want %v", actual, tt.expected)
			}
		})
	}
}
