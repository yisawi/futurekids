package handlers

import (
	"context"
	"encoding/json"
	"future_kids/internal/auth"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type MobileAttendanceRecord struct {
	StudentID int    `json:"student_id"`
	FullName  string `json:"full_name"`
	Status    string `json:"status"` // Present, Absent, Excused
	CheckTime string `json:"check_time,omitempty"`
}

// MobileLoginRequest is the expected JSON payload from the Flutter app for login.
type MobileLoginRequest struct {
	Phone    string `json:"phone"`
	Pin      string `json:"pin"`
	FCMToken string `json:"fcm_token"`
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

type MobileSchedulePayload struct {
	StudentID    int    `json:"student_id"`
	StudentName  string `json:"student_name"`
	Grade        string `json:"grade"`
	Section      string `json:"section"`
	DayOfWeek    string `json:"day_of_week"`
	PeriodNumber int    `json:"period_number"`
	SubjectName  string `json:"subject_name"`
	TeacherName  string `json:"teacher_name"`
}

type MobileNotificationPayload struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	IsRead    bool   `json:"is_read"`
	CreatedAt string `json:"created_at"`
}

type StudentSummary struct {
	StudentID    int    `json:"student_id"`
	FullName     string `json:"full_name"`
	TotalPresent int    `json:"total_present"`
	TotalExcused int    `json:"total_excused"`
	TotalAbsent  int    `json:"total_absent"`
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
		WHERE s.parent_id = $1 AND s.is_active = true
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

type MonthlyRecord struct {
	Date      string `json:"date"`
	Status    string `json:"status"` // Present, Absent, Excused
	CheckTime string `json:"check_time,omitempty"`
}

type StudentMonthlyReport struct {
	StudentID int             `json:"student_id"`
	FullName  string          `json:"full_name"`
	Records   []MonthlyRecord `json:"records"`
}

func (app *AppEnv) MobileAttendanceSummaryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok {
		http.Error(w, `{"status":"error","message":"Unauthorized context"}`, http.StatusUnauthorized)
		return
	}

	monthParam := r.URL.Query().Get("month")
	if monthParam == "" {
		loc, _ := time.LoadLocation("Asia/Baghdad")
		monthParam = time.Now().In(loc).Format("2006-01")
	}

	// CTE ذكي يحسب الأيام الفعلية للدوام حتى تاريخ اليوم (يستبعد الجمعة، السبت، والأيام المستقبلية)
	query := `
		WITH valid_days AS (
			SELECT m_date
			FROM generate_series(
				DATE($1 || '-01'), 
				LEAST((DATE($1 || '-01') + INTERVAL '1 month - 1 day')::DATE, CURRENT_DATE), 
				'1 day'::interval
			) AS md(m_date)
			WHERE EXTRACT(DOW FROM m_date) NOT IN (5, 6)
		)
		SELECT 
			s.id, 
			s.full_name,
			COUNT(al.id) as present_days,
			COUNT(sl.id) as excused_days,
			(SELECT COUNT(*) FROM valid_days) - COUNT(al.id) - COUNT(sl.id) as absent_days
		FROM students s
		CROSS JOIN valid_days vd
		LEFT JOIN attendance_logs al ON s.id = al.student_id AND DATE(al.check_time) = vd.m_date
		LEFT JOIN student_leaves sl ON s.id = sl.student_id AND sl.leave_date = vd.m_date
		WHERE s.parent_id = $2 AND s.is_active = true
		GROUP BY s.id, s.full_name
		ORDER BY s.id ASC
	`

	rows, err := app.DB.QueryContext(r.Context(), query, monthParam, parentID)
	if err != nil {
		http.Error(w, `{"status":"error","message":"Database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var summaries []StudentSummary
	for rows.Next() {
		var s StudentSummary
		if err := rows.Scan(&s.StudentID, &s.FullName, &s.TotalPresent, &s.TotalExcused, &s.TotalAbsent); err != nil {
			continue
		}

		// منع ظهور قيم سالبة في حال وجود خطأ في إدخالات الإجازات/الحضور في أيام العطل
		if s.TotalAbsent < 0 {
			s.TotalAbsent = 0
		}

		summaries = append(summaries, s)
	}

	if summaries == nil {
		summaries = []StudentSummary{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"month":  monthParam,
		"data":   summaries,
	})
}

func (app *AppEnv) MobileMonthlyAttendanceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok {
		http.Error(w, `{"status":"error","message":"Unauthorized context"}`, http.StatusUnauthorized)
		return
	}

	monthParam := r.URL.Query().Get("month")
	if monthParam == "" {
		loc, _ := time.LoadLocation("Asia/Baghdad")
		monthParam = time.Now().In(loc).Format("2006-01")
	}

	// استعلام CTE يولد أيام الشهر، يستبعد المستقبل وعطلة نهاية الأسبوع (5=الجمعة، 6=السبت)
	query := `
		WITH month_dates AS (
			SELECT generate_series(
				DATE($1 || '-01'), 
				(DATE($1 || '-01') + INTERVAL '1 month - 1 day')::DATE, 
				'1 day'::interval
			)::DATE as m_date
		)
		SELECT 
			s.id, 
			s.full_name, 
			TO_CHAR(md.m_date, 'YYYY-MM-DD') as record_date,
			CASE 
				WHEN al.id IS NOT NULL THEN 'Present'
				WHEN sl.id IS NOT NULL THEN 'Excused'
				ELSE 'Absent'
			END as status,
			COALESCE(TO_CHAR(al.check_time, 'HH24:MI'), '') as check_time
		FROM students s
		CROSS JOIN month_dates md
		LEFT JOIN attendance_logs al ON s.id = al.student_id AND DATE(al.check_time) = md.m_date
		LEFT JOIN student_leaves sl ON s.id = sl.student_id AND sl.leave_date = md.m_date
		WHERE s.parent_id = $2 AND s.is_active = true
		  AND md.m_date <= CURRENT_DATE
		  AND EXTRACT(DOW FROM md.m_date) NOT IN (5, 6)
		ORDER BY s.id, md.m_date DESC
	`

	rows, err := app.DB.QueryContext(r.Context(), query, monthParam, parentID)
	if err != nil {
		http.Error(w, `{"status":"error","message":"Database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	// تجميع البيانات هيكلياً لتسهيل عرضها في فلاتر
	reportMap := make(map[int]*StudentMonthlyReport)
	var studentIDs []int // للحفاظ على ترتيب الأبناء

	for rows.Next() {
		var studentID int
		var fullName, recordDate, status, checkTime string

		if err := rows.Scan(&studentID, &fullName, &recordDate, &status, &checkTime); err != nil {
			continue
		}

		if _, exists := reportMap[studentID]; !exists {
			reportMap[studentID] = &StudentMonthlyReport{
				StudentID: studentID,
				FullName:  fullName,
				Records:   []MonthlyRecord{},
			}
			studentIDs = append(studentIDs, studentID)
		}

		reportMap[studentID].Records = append(reportMap[studentID].Records, MonthlyRecord{
			Date:      recordDate,
			Status:    status,
			CheckTime: checkTime,
		})
	}

	var data []StudentMonthlyReport
	for _, id := range studentIDs {
		data = append(data, *reportMap[id])
	}
	if data == nil {
		data = []StudentMonthlyReport{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"month":  monthParam,
		"data":   data,
	})
}

func (app *AppEnv) MobileScheduleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok {
		http.Error(w, `{"status":"error","message":"Unauthorized context"}`, http.StatusUnauthorized)
		return
	}

	// استعلام يربط الطلاب بجدول الحصص بناءً على تطابق الصف والشعبة
	query := `
		SELECT 
			s.id as student_id, 
			s.full_name, 
			s.grade, 
			s.section,
			ws.day_of_week, 
			ws.period_number, 
			ws.subject_name, 
			ws.teacher_name
		FROM students s
		JOIN weekly_schedules ws ON s.grade = ws.grade AND s.section = ws.section
		WHERE s.parent_id = $1 AND s.is_active = true
		ORDER BY s.id ASC, ws.day_of_week ASC, ws.period_number ASC
	`

	rows, err := app.DB.QueryContext(r.Context(), query, parentID)
	if err != nil {
		http.Error(w, `{"status":"error","message":"Database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var schedules []MobileSchedulePayload
	for rows.Next() {
		var sp MobileSchedulePayload
		if err := rows.Scan(&sp.StudentID, &sp.StudentName, &sp.Grade, &sp.Section, &sp.DayOfWeek, &sp.PeriodNumber, &sp.SubjectName, &sp.TeacherName); err != nil {
			continue
		}
		schedules = append(schedules, sp)
	}

	if schedules == nil {
		schedules = []MobileSchedulePayload{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "success", "data": schedules})
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
		WHERE parent_id = $1 AND is_active = true
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

	if err != nil {
		http.Error(w, `{"status":"error","message":"Invalid phone number or PIN"}`, http.StatusUnauthorized)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(dbPin), []byte(req.Pin)); err != nil {
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

	if req.FCMToken != "" {
		_, err := app.DB.ExecContext(ctx, "UPDATE students SET fcm_token = $1 WHERE parent_id = $2", req.FCMToken, parentID)
		if err != nil {
			slog.Error("Failed to update fcm_token for students", "parent_id", parentID, "error", err)
		}
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

func (app *AppEnv) MobileSettingsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"status":"error"}`, http.StatusMethodNotAllowed)
		return
	}

	query := `SELECT setting_key, setting_value FROM settings`
	rows, err := app.DB.QueryContext(r.Context(), query)
	if err != nil {
		http.Error(w, `{"status":"error","message":"Database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	// تحويل البيانات إلى خريطة (Map) ليسهل على فلاتر قراءتها
	settings := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			continue
		}
		settings[key] = value
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data":   settings,
	})
}
