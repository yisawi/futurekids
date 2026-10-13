package handlers

// DailyAttendanceDTO is the canonical shared DTO for per-student daily attendance records.
// Used by MobileTodayAttendanceHandler, and extended by AdminDailyAttendanceDTO.
// Check-in/out times are always present in JSON and null when there is no punch in the window.
type DailyAttendanceDTO struct {
	StudentID    int     `json:"student_id"`
	FullName     string  `json:"full_name"`
	Status       string  `json:"status"` // Present, Absent, Excused
	CheckInTime  *string `json:"check_in_time"`
	CheckOutTime *string `json:"check_out_time"`
}

// AdminDailyAttendanceDTO is one student's day in the admin daily report: the shared fields, the
// source of the punch that is the check-in and the check-out, and the student's manual punches.
type AdminDailyAttendanceDTO struct {
	DailyAttendanceDTO
	CheckInSource  *string       `json:"check_in_source"`
	CheckOutSource *string       `json:"check_out_source"`
	ManualPunches  []ManualPunch `json:"manual_punches"`
}

// ManualPunch is a manual attendance row as the admin daily report lists it.
type ManualPunch struct {
	ID   int64   `json:"id"`
	Type string  `json:"type"`
	Time string  `json:"time"`
	Note *string `json:"note"`
}
