package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"future_kids/internal/auth"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Struct that will be JSON for Flutter
type AttendanceRecord struct {
	StudentID int       `json:"student_id"` // change it to 'int' based on our DB
	FullName  string    `json:"full_name"`
	CheckTime time.Time `json:"check_time"`
	DeviceSN  string    `json:"device_sn"`
}

// LoginRequest represents the expected JSON payload from the Flutter app for login
type LoginRequest struct {
	PhoneNumber string `json:"phone_number"`
	FCMToken    string `json:"fcm_token"`
}

// ParentStudent represents the student data needed by the mobile app.
type ParentStudent struct {
	ID        int    `json:"id"`
	FullName  string `json:"full_name"`
	Grade     string `json:"grade"`
	Section   string `json:"section"`
	AvatarURL string `json:"avatar_url"`
}

type SchedulePeriod struct {
	PeriodNumber int    `json:"period_number"`
	SubjectName  string `json:"subject_name"`
	TeacherName  string `json:"teacher_name"`
}

type DailySchedule struct {
	DayOfWeek string           `json:"day_of_week"`
	Periods   []SchedulePeriod `json:"periods"`
}

type NotificationRecord struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	IsRead    bool   `json:"is_read"`
	CreatedAt string `json:"created_at"`
}

type AttendanceSummary struct {
	TotalPresent int `json:"total_present"`
	TotalExcused int `json:"total_excused"`
	TotalAbsent  int `json:"total_absent"`
}

// GetTodayAttendanceHandler returns today's attendance for the authenticated parent's students.
func (app *AppEnv) GetTodayAttendanceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	phone, ok := r.Context().Value("phone").(string)
	if !ok || phone == "" {
		http.Error(w, `{"status":"error","message":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}

	query := `
		SELECT s.id, s.full_name, a.check_time, a.device_sn
		FROM attendance_logs a
		JOIN students s ON a.student_id = s.id
		WHERE DATE(a.check_time) = CURRENT_DATE
		  AND s.parent_phone = $1
		ORDER BY a.check_time DESC;
	`

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := app.DB.QueryContext(ctx, query, phone)
	if err != nil {
		slog.Error("Database query failed", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var records []AttendanceRecord
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
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	if records == nil {
		records = []AttendanceRecord{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(records); err != nil {
		slog.Error("Failed to encode json", "error", err)
	}
}

// MonthlyDayRecord represents the attendance details for one day.
type MonthlyDayRecord struct {
	Date      string  `json:"date"`       // YYYY-MM-DD
	Status    string  `json:"status"`     // present, late, absent
	EntryTime *string `json:"entry_time"` // 07:45
	ExitTime  *string `json:"exit_time"`  // 12:30
	Duration  *string `json:"duration"`   // e.g. "4 ساعات و 45 دقيقة"
}

// GetAttendanceSummaryHandler returns attendance totals for an authorized student.
func (app *AppEnv) GetAttendanceSummaryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	phone, ok := r.Context().Value("phone").(string)
	studentIDStr := r.URL.Query().Get("student_id")
	if !ok || phone == "" || studentIDStr == "" {
		http.Error(w, `{"status":"error","message":"Unauthorized or missing student_id"}`, http.StatusUnauthorized)
		return
	}

	studentID, err := strconv.Atoi(studentIDStr)
	if err != nil || studentID <= 0 {
		http.Error(w, `{"status":"error","message":"Invalid student_id"}`, http.StatusBadRequest)
		return
	}

	var exists bool
	err = app.DB.QueryRowContext(
		r.Context(),
		"SELECT EXISTS(SELECT 1 FROM students WHERE id = $1 AND parent_phone = $2)",
		studentID,
		phone,
	).Scan(&exists)
	if err != nil {
		slog.Error("Failed to verify student ownership", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, `{"status":"error","message":"Student not found or unauthorized"}`, http.StatusForbidden)
		return
	}

	var summary AttendanceSummary
	err = app.DB.QueryRowContext(r.Context(), `
		SELECT
			(SELECT COUNT(*) FROM attendance_logs WHERE student_id = $1 AND status = 'present'),
			(SELECT COUNT(*) FROM attendance_logs WHERE student_id = $1 AND status = 'absent'),
			(SELECT COUNT(*) FROM student_leaves WHERE student_id = $1);
	`, studentID).Scan(
		&summary.TotalPresent,
		&summary.TotalAbsent,
		&summary.TotalExcused,
	)
	if err != nil {
		slog.Error("Failed to calculate attendance summary", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data":   summary,
	}); err != nil {
		slog.Error("Failed to encode attendance summary response", "error", err)
	}
}

// GetMonthlyAttendanceHandler handles monthly attendance reports for a student.
func (app *AppEnv) GetMonthlyAttendanceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	studentIDStr := r.URL.Query().Get("student_id")
	yearStr := r.URL.Query().Get("year")
	monthStr := r.URL.Query().Get("month")

	if studentIDStr == "" || yearStr == "" || monthStr == "" {
		http.Error(w, `{"status":"error","message":"student_id, year, and month are required"}`, http.StatusBadRequest)
		return
	}

	studentID, err := strconv.Atoi(studentIDStr)
	year, errY := strconv.Atoi(yearStr)
	month, errM := strconv.Atoi(monthStr)
	if err != nil || errY != nil || errM != nil || month < 1 || month > 12 {
		http.Error(w, `{"status":"error","message":"Invalid query parameters"}`, http.StatusBadRequest)
		return
	}

	phone, ok := r.Context().Value("phone").(string)
	if !ok || phone == "" {
		http.Error(w, `{"status":"error","message":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}

	query := `
		SELECT
			TO_CHAR(a.check_time, 'YYYY-MM-DD') AS day_date,
			MIN(a.check_time) AS first_punch,
			MAX(a.check_time) AS last_punch,
			COUNT(*) AS punch_count
		FROM attendance_logs a
		JOIN students s ON a.student_id = s.id
		WHERE a.student_id = $1
		  AND s.parent_phone = $2
		  AND EXTRACT(YEAR FROM a.check_time) = $3
		  AND EXTRACT(MONTH FROM a.check_time) = $4
		GROUP BY TO_CHAR(a.check_time, 'YYYY-MM-DD')
		ORDER BY day_date ASC;
	`

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := app.DB.QueryContext(ctx, query, studentID, phone, year, month)
	if err != nil {
		slog.Error("Failed to fetch monthly attendance", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var records []MonthlyDayRecord
	for rows.Next() {
		var dayDate string
		var firstPunch, lastPunch time.Time
		var punchCount int

		if err := rows.Scan(&dayDate, &firstPunch, &lastPunch, &punchCount); err != nil {
			slog.Error("Failed to scan monthly attendance row", "error", err)
			http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
			return
		}

		entryStr := firstPunch.Format("15:04")
		var exitStr *string
		var durationStr *string
		status := "present"

		if firstPunch.Hour() > 8 || (firstPunch.Hour() == 8 && firstPunch.Minute() > 0) {
			status = "late"
		}

		if punchCount > 1 && !lastPunch.Equal(firstPunch) {
			formattedExit := lastPunch.Format("15:04")
			exitStr = &formattedExit

			diff := lastPunch.Sub(firstPunch)
			hours := int(diff.Hours())
			minutes := int(diff.Minutes()) % 60
			duration := fmt.Sprintf("%d ساعة و %d دقيقة", hours, minutes)
			durationStr = &duration
		}

		records = append(records, MonthlyDayRecord{
			Date:      dayDate,
			Status:    status,
			EntryTime: &entryStr,
			ExitTime:  exitStr,
			Duration:  durationStr,
		})
	}

	if err := rows.Err(); err != nil {
		slog.Error("Error during monthly attendance iteration", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	if records == nil {
		records = []MonthlyDayRecord{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data":   records,
	}); err != nil {
		slog.Error("Failed to encode monthly attendance response", "error", err)
	}
}

// GetWeeklyScheduleHandler returns the weekly schedule for an authorized student.
func (app *AppEnv) GetWeeklyScheduleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	phone, ok := r.Context().Value("phone").(string)
	studentIDStr := r.URL.Query().Get("student_id")
	if !ok || phone == "" || studentIDStr == "" {
		http.Error(w, `{"status":"error","message":"Unauthorized or missing student_id"}`, http.StatusUnauthorized)
		return
	}

	studentID, err := strconv.Atoi(studentIDStr)
	if err != nil || studentID <= 0 {
		http.Error(w, `{"status":"error","message":"Invalid student_id"}`, http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var grade, section string
	err = app.DB.QueryRowContext(
		ctx,
		"SELECT COALESCE(grade, ''), COALESCE(section, '') FROM students WHERE id = $1 AND parent_phone = $2",
		studentID,
		phone,
	).Scan(&grade, &section)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, `{"status":"error","message":"Student not found or unauthorized"}`, http.StatusForbidden)
		} else {
			slog.Error("Failed to fetch student class for schedule", "error", err)
			http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		}
		return
	}

	if grade == "" || section == "" {
		writeWeeklyScheduleResponse(w, []DailySchedule{})
		return
	}

	query := `
		SELECT day_of_week, period_number, subject_name, COALESCE(teacher_name, '')
		FROM weekly_schedules
		WHERE grade = $1 AND section = $2
		ORDER BY
			CASE day_of_week
				WHEN 'الأحد' THEN 1 WHEN 'الإثنين' THEN 2 WHEN 'الثلاثاء' THEN 3
				WHEN 'الأربعاء' THEN 4 WHEN 'الخميس' THEN 5 ELSE 6
			END,
			period_number ASC;
	`

	rows, err := app.DB.QueryContext(ctx, query, grade, section)
	if err != nil {
		slog.Error("Failed to fetch weekly schedule", "error", err)
		http.Error(w, `{"status":"error","message":"Failed to fetch schedule"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	scheduleMap := make(map[string][]SchedulePeriod)
	var daysOrder []string
	for rows.Next() {
		var day string
		var period SchedulePeriod
		if err := rows.Scan(&day, &period.PeriodNumber, &period.SubjectName, &period.TeacherName); err != nil {
			slog.Error("Failed to scan weekly schedule row", "error", err)
			http.Error(w, `{"status":"error","message":"Failed to fetch schedule"}`, http.StatusInternalServerError)
			return
		}
		if len(scheduleMap[day]) == 0 {
			daysOrder = append(daysOrder, day)
		}
		scheduleMap[day] = append(scheduleMap[day], period)
	}

	if err := rows.Err(); err != nil {
		slog.Error("Error during weekly schedule iteration", "error", err)
		http.Error(w, `{"status":"error","message":"Failed to fetch schedule"}`, http.StatusInternalServerError)
		return
	}

	result := make([]DailySchedule, 0, len(daysOrder))
	for _, day := range daysOrder {
		result = append(result, DailySchedule{
			DayOfWeek: day,
			Periods:   scheduleMap[day],
		})
	}
	writeWeeklyScheduleResponse(w, result)
}

func writeWeeklyScheduleResponse(w http.ResponseWriter, schedule []DailySchedule) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data":   schedule,
	}); err != nil {
		slog.Error("Failed to encode weekly schedule response", "error", err)
	}
}

// GetParentStudentsHandler returns the students linked to the authenticated parent's phone number.
func (app *AppEnv) GetParentStudentsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	phone, ok := r.Context().Value("phone").(string)
	if !ok || phone == "" {
		http.Error(w, `{"status":"error","message":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}

	query := `
		SELECT id, full_name, COALESCE(grade, ''), COALESCE(section, ''), COALESCE(avatar_url, '')
		FROM students
		WHERE parent_phone = $1
		ORDER BY id ASC;
	`

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := app.DB.QueryContext(ctx, query, phone)
	if err != nil {
		slog.Error("Failed to fetch parent students", "error", err, "phone", phone)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var students []ParentStudent
	for rows.Next() {
		var student ParentStudent
		if err := rows.Scan(
			&student.ID,
			&student.FullName,
			&student.Grade,
			&student.Section,
			&student.AvatarURL,
		); err != nil {
			slog.Error("Failed to scan student row", "error", err)
			http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
			return
		}
		students = append(students, student)
	}

	if err := rows.Err(); err != nil {
		slog.Error("Error during student rows iteration", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	if students == nil {
		students = []ParentStudent{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data":   students,
	}); err != nil {
		slog.Error("Failed to encode parent students response", "error", err)
	}
}

// GetNotificationsHandler returns the authenticated parent's recent notifications.
func (app *AppEnv) GetNotificationsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	phone, ok := r.Context().Value("phone").(string)
	if !ok || phone == "" {
		http.Error(w, `{"status":"error","message":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := app.DB.QueryContext(ctx, `
		SELECT id, title, body, is_read, created_at
		FROM notifications
		WHERE parent_phone = $1
		ORDER BY created_at DESC
		LIMIT 50;
	`, phone)
	if err != nil {
		slog.Error("Failed to fetch notifications", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	notifications := make([]NotificationRecord, 0)
	for rows.Next() {
		var notification NotificationRecord
		var createdAt time.Time
		if err := rows.Scan(
			&notification.ID,
			&notification.Title,
			&notification.Body,
			&notification.IsRead,
			&createdAt,
		); err != nil {
			slog.Error("Failed to scan notification row", "error", err)
			http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
			return
		}
		notification.CreatedAt = createdAt.Format(time.RFC3339)
		notifications = append(notifications, notification)
	}

	if err := rows.Err(); err != nil {
		slog.Error("Error during notification rows iteration", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data":   notifications,
	}); err != nil {
		slog.Error("Failed to encode notifications response", "error", err)
	}
}

// MobileLoginHandler handles the authentication request from the mobile app
func (app *AppEnv) MobileLoginHandler(w http.ResponseWriter, r *http.Request) {
	// 1. Ensure the request method is POST only
	if r.Method != http.MethodPost {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// 2. Decode the incoming JSON payload
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"status":"error","message":"Invalid request body"}`, http.StatusBadRequest)
		return
	}

	// --- Real Database Validation ---
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var studentID int
	var parentPhone string

	// 3. Check if the phone number exists in the students table
	query := `SELECT id, parent_phone FROM students WHERE parent_phone = $1 LIMIT 1`
	err := app.DB.QueryRowContext(ctx, query, req.PhoneNumber).Scan(&studentID, &parentPhone)

	if err != nil {
		if err == sql.ErrNoRows {
			// الهاتف غير موجود في قاعدة البيانات
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"status":"error","message":"رقم الهاتف غير مسجل في النظام"}`))
			return
		}
		// خطأ داخلي في قاعدة البيانات
		slog.Error("Database query failed during login", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	// 4. Update the FCM Token for this guardian's phone number
	updateQuery := `UPDATE students SET fcm_token = $1 WHERE parent_phone = $2`
	_, err = app.DB.ExecContext(ctx, updateQuery, req.FCMToken, req.PhoneNumber)
	if err != nil {
		slog.Error("Failed to update FCM token", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	// 5. Generate the Real JWT Token
	tokenString, err := auth.GenerateToken(req.PhoneNumber)
	if err != nil {
		slog.Error("Failed to generate JWT", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	// 6. Send the success response with the real token
	response := map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"access_token": tokenString, // هنا يتم حقن التوكن الحقيقي المشفر
			"guardian": map[string]interface{}{
				"phone":   parentPhone,
				"message": "تم التحقق من الهاتف وتحديث مفتاح الإشعارات بنجاح",
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}
