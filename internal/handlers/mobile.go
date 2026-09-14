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

type MobileAttendanceRecord struct {
	StudentID int    `json:"student_id"`
	FullName  string `json:"full_name"`
	Status    string `json:"status"` // Present, Absent, Excused
	CheckTime string `json:"check_time,omitempty"`
}

// MobileLoginRequest is the expected JSON payload from the Flutter app for login.
type MobileLoginRequest struct {
	Phone string `json:"phone"`
	Pin   string `json:"pin"`
}

// LoginRequest kept for backwards-compatibility.
type LoginRequest = MobileLoginRequest

type MobileStudentPayload struct {
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

type MobileNotificationPayload struct {
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

type Banner struct {
	ID         int    `json:"id"`
	Title      string `json:"title"`
	ImageURL   string `json:"image_url"`
	ActionLink string `json:"action_link"`
}

func (app *AppEnv) MobileTodayAttendanceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok {
		http.Error(w, `{"status":"error","message":"Unauthorized context"}`, http.StatusUnauthorized)
		return
	}

	loc, _ := time.LoadLocation("Asia/Baghdad")
	today := time.Now().In(loc).Format("2006-01-02")

	query := `
		SELECT 
			s.id, 
			s.full_name,
			CASE 
				WHEN al.id IS NOT NULL THEN 'Present'
				WHEN sl.id IS NOT NULL THEN 'Excused'
				ELSE 'Absent'
			END as status,
			COALESCE(CAST(al.check_time AS TEXT), '') as check_time
		FROM students s
		LEFT JOIN attendance_logs al ON s.id = al.student_id AND DATE(al.check_time) = $2
		LEFT JOIN student_leaves sl ON s.id = sl.student_id AND sl.leave_date = $2
		WHERE s.parent_id = $1
	`

	rows, err := app.DB.QueryContext(r.Context(), query, parentID, today)
	if err != nil {
		http.Error(w, `{"status":"error","message":"Database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var records []MobileAttendanceRecord
	for rows.Next() {
		var rec MobileAttendanceRecord
		if err := rows.Scan(&rec.StudentID, &rec.FullName, &rec.Status, &rec.CheckTime); err != nil {
			continue
		}
		records = append(records, rec)
	}

	if records == nil {
		records = []MobileAttendanceRecord{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "success", "date": today, "data": records})
}

// GetActiveBannersHandler returns active banners ordered from newest to oldest.
func (app *AppEnv) GetActiveBannersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	rows, err := app.DB.QueryContext(r.Context(), `
		SELECT id, COALESCE(title, ''), image_url, COALESCE(action_link, '')
		FROM banners
		WHERE is_active = true
		ORDER BY created_at DESC;
	`)
	if err != nil {
		slog.Error("Failed to fetch banners", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	banners := make([]Banner, 0)
	for rows.Next() {
		var banner Banner
		if err := rows.Scan(&banner.ID, &banner.Title, &banner.ImageURL, &banner.ActionLink); err != nil {
			slog.Error("Failed to scan banner row", "error", err)
			http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
			return
		}
		banners = append(banners, banner)
	}

	if err := rows.Err(); err != nil {
		slog.Error("Error during banner rows iteration", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data":   banners,
	}); err != nil {
		slog.Error("Failed to encode banners response", "error", err)
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

	parentID, ok := r.Context().Value(ParentIDKey).(int)
	studentIDStr := r.URL.Query().Get("student_id")
	if !ok || parentID == 0 || studentIDStr == "" {
		http.Error(w, `{"status":"error","message":"Unauthorized or missing student_id"}`, http.StatusUnauthorized)
		return
	}

	studentID, err := strconv.Atoi(studentIDStr)
	if err != nil || studentID <= 0 {
		http.Error(w, `{"status":"error","message":"Invalid student_id"}`, http.StatusBadRequest)
		return
	}

	// Ownership check: student must belong to this parent
	var exists bool
	err = app.DB.QueryRowContext(
		r.Context(),
		"SELECT EXISTS(SELECT 1 FROM students WHERE id = $1 AND parent_id = $2)",
		studentID,
		parentID,
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

	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok || parentID == 0 {
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
		  AND s.parent_id = $2
		  AND EXTRACT(YEAR FROM a.check_time) = $3
		  AND EXTRACT(MONTH FROM a.check_time) = $4
		GROUP BY TO_CHAR(a.check_time, 'YYYY-MM-DD')
		ORDER BY day_date ASC;
	`

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := app.DB.QueryContext(ctx, query, studentID, parentID, year, month)
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

	parentID, ok := r.Context().Value(ParentIDKey).(int)
	studentIDStr := r.URL.Query().Get("student_id")
	if !ok || parentID == 0 || studentIDStr == "" {
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
		"SELECT COALESCE(grade, ''), COALESCE(section, '') FROM students WHERE id = $1 AND parent_id = $2",
		studentID,
		parentID,
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

func (app *AppEnv) MobileStudentsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// استخراج هوية الأب من سياق الطلب (تم حقنها عبر AuthMiddleware)
	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok {
		http.Error(w, `{"status":"error","message":"Unauthorized context"}`, http.StatusUnauthorized)
		return
	}

	query := `
		SELECT id, full_name, COALESCE(grade, ''), COALESCE(section, ''), COALESCE(avatar_url, '') 
		FROM students 
		WHERE parent_id = $1 
		ORDER BY id ASC
	`
	
	rows, err := app.DB.QueryContext(r.Context(), query, parentID)
	if err != nil {
		http.Error(w, `{"status":"error","message":"Database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var students []MobileStudentPayload
	for rows.Next() {
		var s MobileStudentPayload
		if err := rows.Scan(&s.ID, &s.FullName, &s.Grade, &s.Section, &s.AvatarURL); err != nil {
			continue
		}
		students = append(students, s)
	}

	if students == nil {
		students = []MobileStudentPayload{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "success", "data": students})
}




func (app *AppEnv) MobileNotificationsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// 1. استخراج parent_id من السياق المحمي
	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok {
		http.Error(w, `{"status":"error","message":"Unauthorized context"}`, http.StatusUnauthorized)
		return
	}

	// 2. استعلام JOIN لجلب الإشعارات عبر مطابقة رقم الهاتف المرتبط بـ parent_id
	query := `
		SELECT n.id, n.title, n.body, n.is_read, n.created_at
		FROM notifications n
		JOIN parents p ON n.parent_phone = p.phone_number
		WHERE p.id = $1
		ORDER BY n.created_at DESC
	`

	rows, err := app.DB.QueryContext(r.Context(), query, parentID)
	if err != nil {
		http.Error(w, `{"status":"error","message":"Database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var notifications []MobileNotificationPayload
	for rows.Next() {
		var n MobileNotificationPayload
		var createdAt time.Time
		if err := rows.Scan(&n.ID, &n.Title, &n.Body, &n.IsRead, &createdAt); err != nil {
			continue
		}
		// تنسيق الوقت ليقبله تطبيق فلاتر بسلاسة
		n.CreatedAt = createdAt.Format(time.RFC3339)
		notifications = append(notifications, n)
	}

	if notifications == nil {
		notifications = []MobileNotificationPayload{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "success", "data": notifications})
}



// MobileLoginHandler authenticates a parent against the parents table and issues a parent_id JWT.
func (app *AppEnv) MobileLoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req MobileLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"status":"error","message":"Invalid request"}`, http.StatusBadRequest)
		return
	}

	if req.Phone == "" || req.Pin == "" {
		http.Error(w, `{"status":"error","message":"Phone and PIN are required"}`, http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var parentID int
	var parentName, dbPin string

	// البحث حصراً في جدول الآباء
	query := `SELECT id, full_name, pin_code FROM parents WHERE phone_number = $1`
	err := app.DB.QueryRowContext(ctx, query, req.Phone).Scan(&parentID, &parentName, &dbPin)

	if err != nil || req.Pin != dbPin {
		// توحيد رسالة الخطأ أمنياً لمنع هجمات التخمين
		http.Error(w, `{"status":"error","message":"Invalid phone number or PIN"}`, http.StatusUnauthorized)
		return
	}

	tokenString, err := auth.GenerateParentToken(parentID, req.Phone)
	if err != nil {
		slog.Error("Failed to generate JWT", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"token": tokenString,
			"parent": map[string]interface{}{
				"id":    parentID,
				"name":  parentName,
				"phone": req.Phone,
			},
		},
	}); err != nil {
		slog.Error("Failed to encode login response", "error", err)
	}
}
