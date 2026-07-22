package handlers

import (
	"encoding/json"
	"net/http"
	"time"
)

// Struct that will be JSON for Flutter
type AttendanceRecord struct {
	StudentID string    `json:"student_id"`
	FullName  string    `json:"full_name"`
	CheckTime time.Time `json:"check_time"`
	DeviceSN  string    `json:"device_sn"`
}


func GetTodayAttendanceHandler(w http.ResponseWriter, r *http.Request) {
	// تأمين المسار ليقبل فقط طلبات GET
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}


	// dummy data for testing with flutter
	dummyData := []AttendanceRecord{
		{
			StudentID: "1001",
			FullName:  "أحمد محمد",
			CheckTime: time.Now(),
			DeviceSN:  "ZK-MAIN-GATE-01",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	// تحويل البيانات البرمجية إلى JSON وإرسالها
	if err := json.NewEncoder(w).Encode(dummyData); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}