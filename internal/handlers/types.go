package handlers

// DailyAttendanceDTO is the canonical shared DTO for per-student daily attendance records.
// Used by both AdminDailyAttendanceHandler and MobileTodayAttendanceHandler.
type DailyAttendanceDTO struct {
	StudentID int    `json:"student_id"`
	FullName  string `json:"full_name"`
	Status       string `json:"status"` // Present, Absent, Excused
	CheckInTime  string `json:"check_in_time,omitempty"`
	CheckOutTime string `json:"check_out_time,omitempty"`
}
