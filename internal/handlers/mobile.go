package handlers

import (
	"context"
	"database/sql"
	"future_kids/internal/auth"
	"future_kids/internal/notify"
	"future_kids/internal/phone"
	"future_kids/internal/ratelimit"
	"future_kids/internal/tz"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// MobileLoginRequest is the expected JSON payload from the Flutter app for login.
type MobileLoginRequest struct {
	Phone    string `json:"phone"`
	Pin      string `json:"pin"`
	FCMToken string `json:"fcm_token"`
}

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
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok {
		respondError(w, http.StatusUnauthorized, "Unauthorized context")
		return
	}

	today := tz.Today()

	query := `
		SELECT 
			s.id, 
			s.full_name,
			st.status,
			st.first_check AS check_in_time,
			st.last_check AS check_out_time
		FROM students s
		CROSS JOIN LATERAL get_student_status(s.id, $2::DATE) st
		WHERE s.parent_id = $1 AND s.is_active = true
	`

	rows, err := app.DB.QueryContext(r.Context(), query, parentID, today)
	if err != nil {
		respondInternalError(w, "Database error", "MobileTodayAttendanceHandler: query failed", err, "parent_id", parentID)
		return
	}
	defer rows.Close()

	var records []DailyAttendanceDTO
	for rows.Next() {
		var rec DailyAttendanceDTO
		if err := rows.Scan(&rec.StudentID, &rec.FullName, &rec.Status, &rec.CheckInTime, &rec.CheckOutTime); err != nil {
			respondInternalError(w, "Database error", "MobileTodayAttendanceHandler: scan failed", err, "parent_id", parentID)
			return
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "MobileTodayAttendanceHandler: rows iteration failed", err, "parent_id", parentID)
		return
	}

	if records == nil {
		records = []DailyAttendanceDTO{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "date": today, "data": records})
}

// GetActiveBannersHandler returns active banners ordered from newest to oldest.
func (app *AppEnv) GetActiveBannersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	rows, err := app.DB.QueryContext(r.Context(), `
		SELECT id, COALESCE(title, ''), image_url, COALESCE(action_link, '')
		FROM banners
		WHERE is_active = true
		ORDER BY created_at DESC;
	`)
	if err != nil {
		respondInternalError(w, "Internal server error", "GetActiveBannersHandler: query failed", err)
		return
	}
	defer rows.Close()

	banners := make([]Banner, 0)
	for rows.Next() {
		var banner Banner
		if err := rows.Scan(&banner.ID, &banner.Title, &banner.ImageURL, &banner.ActionLink); err != nil {
			respondInternalError(w, "Internal server error", "GetActiveBannersHandler: scan failed", err)
			return
		}
		banners = append(banners, banner)
	}

	if err := rows.Err(); err != nil {
		respondInternalError(w, "Internal server error", "GetActiveBannersHandler: rows iteration failed", err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "data": banners})
}

type MonthlyRecord struct {
	Date         string  `json:"date"`
	Status       string  `json:"status"` // Present, Absent, Excused
	CheckTime    *string `json:"check_time"`
	CheckOutTime *string `json:"check_out_time"`
}

// requestedMonth returns the ?month= query param (YYYY-MM), defaulting to now's month;
// ok is false (and 400 has been written) when the value is malformed.
func requestedMonth(w http.ResponseWriter, r *http.Request, now time.Time) (month string, ok bool) {
	m := r.URL.Query().Get("month")
	if m == "" {
		return now.Format("2006-01"), true
	}
	if _, err := time.Parse("2006-01", m); err != nil {
		respondError(w, http.StatusBadRequest, "month must be formatted as YYYY-MM")
		return "", false
	}
	return m, true
}

type StudentMonthlyReport struct {
	StudentID int             `json:"student_id"`
	FullName  string          `json:"full_name"`
	Records   []MonthlyRecord `json:"records"`
}

func (app *AppEnv) MobileAttendanceSummaryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok {
		respondError(w, http.StatusUnauthorized, "Unauthorized context")
		return
	}

	now := tz.Now()
	today := now.Format("2006-01-02")
	monthParam, ok := requestedMonth(w, r, now)
	if !ok {
		return
	}

	// CTE ذكي يحسب الأيام الفعلية للدوام حتى تاريخ اليوم (يستبعد الجمعة، السبت، والأيام المستقبلية)
	query := `
		WITH valid_days AS (
			SELECT d::DATE AS m_date
			FROM generate_series(
				DATE($1 || '-01'),
				LEAST((DATE($1 || '-01') + INTERVAL '1 month - 1 day')::DATE, $3::DATE),
				'1 day'::interval
			) AS d
			WHERE EXTRACT(DOW FROM d) NOT IN (5, 6)
		)
		SELECT 
			s.id, 
			s.full_name,
			COUNT(CASE WHEN st.status = 'Present' THEN 1 END) as present_days,
			COUNT(CASE WHEN st.status = 'Excused' THEN 1 END) as excused_days,
			COUNT(CASE WHEN st.status = 'Absent' THEN 1 END) as absent_days
		FROM students s
		CROSS JOIN valid_days vd
		CROSS JOIN LATERAL get_student_status(s.id, vd.m_date) st
		WHERE s.parent_id = $2 AND s.is_active = true
		GROUP BY s.id, s.full_name
		ORDER BY s.id ASC
	`

	rows, err := app.DB.QueryContext(r.Context(), query, monthParam, parentID, today)
	if err != nil {
		respondInternalError(w, "Database error", "MobileAttendanceSummaryHandler: query failed", err, "parent_id", parentID, "month", monthParam)
		return
	}
	defer rows.Close()

	var summaries []StudentSummary
	for rows.Next() {
		var s StudentSummary
		if err := rows.Scan(&s.StudentID, &s.FullName, &s.TotalPresent, &s.TotalExcused, &s.TotalAbsent); err != nil {
			respondInternalError(w, "Database error", "MobileAttendanceSummaryHandler: scan failed", err, "parent_id", parentID, "month", monthParam)
			return
		}

		summaries = append(summaries, s)
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "MobileAttendanceSummaryHandler: rows iteration failed", err, "parent_id", parentID, "month", monthParam)
		return
	}

	if summaries == nil {
		summaries = []StudentSummary{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status": "success",
		"month":  monthParam,
		"data":   summaries,
	})
}

func (app *AppEnv) MobileMonthlyAttendanceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok {
		respondError(w, http.StatusUnauthorized, "Unauthorized context")
		return
	}

	now := tz.Now()
	today := now.Format("2006-01-02")
	monthParam, ok := requestedMonth(w, r, now)
	if !ok {
		return
	}

	// استعلام CTE يولد أيام الشهر، يستبعد المستقبل وعطلة نهاية الأسبوع (5=الجمعة، 6=السبت)
	query := `
		WITH month_dates AS (
			SELECT generate_series(
				DATE($1 || '-01'), 
				(DATE($1 || '-01') + INTERVAL '1 month - 1 day')::DATE, 
				'1 day'::interval
			)::DATE AS m_date
		)
		SELECT 
			s.id as student_id,
			s.full_name,
			TO_CHAR(md.m_date, 'YYYY-MM-DD') as record_date,
			st.status,
			st.first_check AS check_time,
			st.last_check AS check_out_time
		FROM students s
		CROSS JOIN month_dates md
		CROSS JOIN LATERAL get_student_status(s.id, md.m_date) st
		WHERE s.parent_id = $2 AND s.is_active = true
		  AND md.m_date <= $3::DATE
		  AND EXTRACT(DOW FROM md.m_date) NOT IN (5, 6)
		ORDER BY s.id, md.m_date DESC
	`

	rows, err := app.DB.QueryContext(r.Context(), query, monthParam, parentID, today)
	if err != nil {
		respondInternalError(w, "Database error", "MobileMonthlyAttendanceHandler: query failed", err, "parent_id", parentID, "month", monthParam)
		return
	}
	defer rows.Close()

	// تجميع البيانات هيكلياً لتسهيل عرضها في فلاتر
	reportMap := make(map[int]*StudentMonthlyReport)
	var studentIDs []int // للحفاظ على ترتيب الأبناء

	for rows.Next() {
		var studentID int
		var fullName, recordDate, status string
		var checkTime, checkOutTime *string

		if err := rows.Scan(&studentID, &fullName, &recordDate, &status, &checkTime, &checkOutTime); err != nil {
			respondInternalError(w, "Database error", "MobileMonthlyAttendanceHandler: scan failed", err, "parent_id", parentID, "month", monthParam)
			return
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
			Date:         recordDate,
			Status:       status,
			CheckTime:    checkTime,
			CheckOutTime: checkOutTime,
		})
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "MobileMonthlyAttendanceHandler: rows iteration failed", err, "parent_id", parentID, "month", monthParam)
		return
	}

	var data []StudentMonthlyReport
	for _, id := range studentIDs {
		data = append(data, *reportMap[id])
	}
	if data == nil {
		data = []StudentMonthlyReport{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status": "success",
		"month":  monthParam,
		"data":   data,
	})
}

// weekdayRankSQL ranks ws.day_of_week in school-week order: Sunday→Thursday (1-5), then
// Friday and Saturday (6-7). The name is trimmed, lower-cased and has أ/إ/آ folded to ا, so
// Arabic (with or without hamza) and English names in any case are recognized. Anything else
// ranks 8: it sorts after every real day, alphabetically by the stored name.
const weekdayRankSQL = `CASE translate(lower(btrim(ws.day_of_week)), 'أإآ', 'ااا')
			WHEN 'sunday' THEN 1 WHEN 'الاحد' THEN 1
			WHEN 'monday' THEN 2 WHEN 'الاثنين' THEN 2
			WHEN 'tuesday' THEN 3 WHEN 'الثلاثاء' THEN 3
			WHEN 'wednesday' THEN 4 WHEN 'الاربعاء' THEN 4
			WHEN 'thursday' THEN 5 WHEN 'الخميس' THEN 5
			WHEN 'friday' THEN 6 WHEN 'الجمعة' THEN 6
			WHEN 'saturday' THEN 7 WHEN 'السبت' THEN 7
			ELSE 8
		END`

func (app *AppEnv) MobileScheduleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok {
		respondError(w, http.StatusUnauthorized, "Unauthorized context")
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
			COALESCE(ws.teacher_name, '') AS teacher_name
		FROM students s
		JOIN weekly_schedules ws ON s.grade = ws.grade AND s.section = ws.section
		CROSS JOIN LATERAL (SELECT ` + weekdayRankSQL + ` AS rank) day
		WHERE s.parent_id = $1 AND s.is_active = true
		ORDER BY s.id ASC, day.rank ASC, CASE WHEN day.rank = 8 THEN ws.day_of_week END ASC, ws.period_number ASC
	`

	rows, err := app.DB.QueryContext(r.Context(), query, parentID)
	if err != nil {
		respondInternalError(w, "Database error", "MobileScheduleHandler: query failed", err, "parent_id", parentID)
		return
	}
	defer rows.Close()

	var schedules []MobileSchedulePayload
	for rows.Next() {
		var sp MobileSchedulePayload
		if err := rows.Scan(&sp.StudentID, &sp.StudentName, &sp.Grade, &sp.Section, &sp.DayOfWeek, &sp.PeriodNumber, &sp.SubjectName, &sp.TeacherName); err != nil {
			respondInternalError(w, "Database error", "MobileScheduleHandler: scan failed", err, "parent_id", parentID)
			return
		}
		schedules = append(schedules, sp)
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "MobileScheduleHandler: rows iteration failed", err, "parent_id", parentID)
		return
	}

	if schedules == nil {
		schedules = []MobileSchedulePayload{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "data": schedules})
}

func (app *AppEnv) MobileStudentsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// استخراج هوية الأب من سياق الطلب (تم حقنها عبر AuthMiddleware)
	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok {
		respondError(w, http.StatusUnauthorized, "Unauthorized context")
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
		respondInternalError(w, "Database error", "MobileStudentsHandler: query failed", err, "parent_id", parentID)
		return
	}
	defer rows.Close()

	var students []MobileStudentPayload
	for rows.Next() {
		var s MobileStudentPayload
		if err := rows.Scan(&s.ID, &s.FullName, &s.Grade, &s.Section, &s.AvatarURL); err != nil {
			respondInternalError(w, "Database error", "MobileStudentsHandler: scan failed", err, "parent_id", parentID)
			return
		}
		students = append(students, s)
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "MobileStudentsHandler: rows iteration failed", err, "parent_id", parentID)
		return
	}

	if students == nil {
		students = []MobileStudentPayload{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "data": students})
}

// NotificationsPageSize is the most notifications one request returns; use has_more and
// next_before to fetch older pages.
const NotificationsPageSize = 100

func (app *AppEnv) MobileNotificationsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// 1. استخراج parent_id من السياق المحمي
	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok {
		respondError(w, http.StatusUnauthorized, "Unauthorized context")
		return
	}

	// Keyset pagination: newest first by id; ?before=<id> returns the page after that id.
	var before sql.NullInt64
	if v := r.URL.Query().Get("before"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id < 1 {
			respondError(w, http.StatusBadRequest, "before must be a positive notification id")
			return
		}
		before = sql.NullInt64{Int64: id, Valid: true}
	}

	// 2. استعلام JOIN لجلب الإشعارات عبر مطابقة رقم الهاتف المرتبط بـ parent_id
	query := `
		SELECT n.id, n.title, n.body, COALESCE(n.is_read, false) AS is_read, COALESCE(n.created_at AT TIME ZONE 'Asia/Baghdad', CURRENT_TIMESTAMP) AS created_at
		FROM notifications n
		JOIN parents p ON n.parent_phone = p.phone_number
		WHERE p.id = $1 AND ($2::bigint IS NULL OR n.id < $2)
		ORDER BY n.id DESC
		LIMIT $3
	`

	rows, err := app.DB.QueryContext(r.Context(), query, parentID, before, NotificationsPageSize+1)
	if err != nil {
		respondInternalError(w, "Database error", "MobileNotificationsHandler: query failed", err, "parent_id", parentID)
		return
	}
	defer rows.Close()

	var notifications []MobileNotificationPayload
	for rows.Next() {
		var n MobileNotificationPayload
		var createdAt time.Time
		if err := rows.Scan(&n.ID, &n.Title, &n.Body, &n.IsRead, &createdAt); err != nil {
			respondInternalError(w, "Database error", "MobileNotificationsHandler: scan failed", err, "parent_id", parentID)
			return
		}
		// تنسيق الوقت ليقبله تطبيق فلاتر بسلاسة
		n.CreatedAt = createdAt.In(tz.Baghdad).Format(time.RFC3339)
		notifications = append(notifications, n)
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "MobileNotificationsHandler: rows iteration failed", err, "parent_id", parentID)
		return
	}

	if notifications == nil {
		notifications = []MobileNotificationPayload{}
	}
	hasMore := len(notifications) > NotificationsPageSize
	var nextBefore *int
	if hasMore {
		notifications = notifications[:NotificationsPageSize]
		nextBefore = &notifications[NotificationsPageSize-1].ID
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":      "success",
		"data":        notifications,
		"has_more":    hasMore,
		"next_before": nextBefore,
	})
}

// MobileLoginHandler authenticates a parent against the parents table and issues a parent_id JWT.
func (app *AppEnv) MobileLoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req MobileLoginRequest
	if !decodeJSONBody(w, r, &req, "Invalid request") {
		return
	}

	req.Phone = strings.TrimSpace(req.Phone)
	if req.Phone == "" || req.Pin == "" {
		respondError(w, http.StatusBadRequest, "Phone and PIN are required")
		return
	}

	if app.LoginLimiter == nil {
		slog.Error("MobileLoginHandler: LoginLimiter is not configured")
		respondError(w, http.StatusInternalServerError, "Internal server error")
		return
	}
	// A number that is not an Iraqi mobile number gets the same answer as an unregistered one.
	canonical, valid := phone.Normalize(req.Phone)
	if !valid {
		burnBcrypt(req.Pin)
		respondError(w, http.StatusUnauthorized, "Invalid phone number or PIN")
		return
	}
	req.Phone = canonical
	// Unknown phone numbers are limited too, so a 429 never reveals which numbers are registered.
	allowed, retryAfter := app.LoginLimiter.Allow(req.Phone)
	if !allowed {
		logRateLimited("MobileLoginHandler", r, "phone", req.Phone)
		respondTooManyAttempts(w, retryAfter)
		return
	}
	outcome := ratelimit.Released
	defer func() { app.LoginLimiter.Finish(req.Phone, outcome) }()

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var parentID, sessionVersion int
	var parentName, dbPin string

	query := `SELECT id, full_name, pin_code, session_version FROM parents WHERE phone_number = $1`
	err := app.DB.QueryRowContext(ctx, query, req.Phone).Scan(&parentID, &parentName, &dbPin, &sessionVersion)

	if err == sql.ErrNoRows {
		burnBcrypt(req.Pin)
		outcome = ratelimit.Failed
		respondError(w, http.StatusUnauthorized, "Invalid phone number or PIN")
		return
	} else if err != nil {
		respondInternalError(w, "Internal server error", "MobileLoginHandler: query failed", err, "phone", req.Phone)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(dbPin), []byte(req.Pin)); err != nil {
		// توحيد رسالة الخطأ أمنياً لمنع هجمات التخمين
		outcome = ratelimit.Failed
		respondError(w, http.StatusUnauthorized, "Invalid phone number or PIN")
		return
	}
	outcome = ratelimit.Succeeded

	tokenString, err := auth.GenerateParentToken(parentID, req.Phone, sessionVersion)
	if err != nil {
		respondInternalError(w, "Internal server error", "MobileLoginHandler: token generation failed", err, "parent_id", parentID)
		return
	}

	if req.FCMToken != "" {
		if !validDeviceToken(req.FCMToken) {
			slog.Warn("MobileLoginHandler: invalid fcm_token ignored", "parent_id", parentID)
		} else if err := app.registerDeviceToken(ctx, parentID, req.FCMToken); err != nil {
			slog.Error("MobileLoginHandler: device token not registered", "parent_id", parentID, "token", notify.TokenFingerprint(req.FCMToken), "error", err)
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"token": tokenString,
			"parent": map[string]interface{}{
				"id":    parentID,
				"name":  parentName,
				"phone": req.Phone,
			},
		},
	})
}

// PublicSettingKeys are the only settings the unauthenticated mobile settings endpoint returns.
var PublicSettingKeys = []string{"whatsapp_number", "school_name"}

func (app *AppEnv) MobileSettingsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// Public endpoint: return only the keys the app needs, never internal settings.
	query := `SELECT setting_key, setting_value FROM settings WHERE setting_key = ANY($1::text[])`
	rows, err := app.DB.QueryContext(r.Context(), query, PublicSettingKeys)
	if err != nil {
		respondInternalError(w, "Database error", "MobileSettingsHandler: query failed", err)
		return
	}
	defer rows.Close()

	// تحويل البيانات إلى خريطة (Map) ليسهل على فلاتر قراءتها
	settings := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			respondInternalError(w, "Database error", "MobileSettingsHandler: scan failed", err)
			return
		}
		settings[key] = value
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "MobileSettingsHandler: rows iteration failed", err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status": "success",
		"data":   settings,
	})
}

// Device tokens: each parent's phones register their FCM token, and every push for any of the
// parent's children goes to all of them.
const (
	MaxDeviceTokensPerParent = 10
	MinDeviceTokenLength     = 20
	MaxDeviceTokenLength     = 1024
)

// InvalidDeviceTokenMessage is the 400 message for a token that fails validDeviceToken.
const InvalidDeviceTokenMessage = "token must be 20 to 1024 characters: letters, digits, ':', '_', '.' or '-'"

var deviceTokenChars = regexp.MustCompile(`^[A-Za-z0-9:_.-]+$`)

func validDeviceToken(token string) bool {
	return len(token) >= MinDeviceTokenLength && len(token) <= MaxDeviceTokenLength && deviceTokenChars.MatchString(token)
}

type DeviceTokenRequest struct {
	Token string `json:"token"`
}

// registerDeviceToken stores token for parentID, moving it from any other parent (a phone
// belongs to one parent at a time) and marking it seen now. A parent keeps at most
// MaxDeviceTokensPerParent tokens; the least recently seen ones beyond that are deleted.
func (app *AppEnv) registerDeviceToken(ctx context.Context, parentID int, token string) error {
	tx, err := app.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO device_tokens (parent_id, token) VALUES ($1, $2)
		ON CONFLICT (token) DO UPDATE
		SET created_at = CASE WHEN device_tokens.parent_id = EXCLUDED.parent_id THEN device_tokens.created_at ELSE CURRENT_TIMESTAMP END,
		    parent_id = EXCLUDED.parent_id,
		    last_seen_at = CURRENT_TIMESTAMP`, parentID, token); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM device_tokens WHERE id IN (
			SELECT id FROM device_tokens WHERE parent_id = $1
			ORDER BY last_seen_at DESC, id DESC
			OFFSET $2)`, parentID, MaxDeviceTokensPerParent); err != nil {
		return err
	}
	return tx.Commit()
}

// decodeDeviceToken reads {"token": "..."}, writing 400 or 413 and returning false when it is
// malformed or the token is invalid.
func decodeDeviceToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req DeviceTokenRequest
	if !decodeJSONBody(w, r, &req, "Invalid request body") {
		return "", false
	}
	if !validDeviceToken(req.Token) {
		respondError(w, http.StatusBadRequest, InvalidDeviceTokenMessage)
		return "", false
	}
	return req.Token, true
}

// RegisterDeviceTokenHandler (PUT /api/mobile/device-token) registers or refreshes this phone's
// FCM token for the authenticated parent.
func (app *AppEnv) RegisterDeviceTokenHandler(w http.ResponseWriter, r *http.Request) {
	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok {
		respondError(w, http.StatusUnauthorized, "Unauthorized context")
		return
	}
	token, ok := decodeDeviceToken(w, r)
	if !ok {
		return
	}
	if err := app.registerDeviceToken(r.Context(), parentID, token); err != nil {
		respondInternalError(w, "Internal server error", "RegisterDeviceTokenHandler: register failed", err, "parent_id", parentID, "token", notify.TokenFingerprint(token))
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "success", "message": "Device token registered"})
}

// RemoveDeviceTokenHandler (DELETE /api/mobile/device-token) removes this phone's FCM token
// from the authenticated parent. It answers 200 whether or not the parent had the token, so a
// retried logout succeeds and nothing is revealed about other parents' tokens.
func (app *AppEnv) RemoveDeviceTokenHandler(w http.ResponseWriter, r *http.Request) {
	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok {
		respondError(w, http.StatusUnauthorized, "Unauthorized context")
		return
	}
	token, ok := decodeDeviceToken(w, r)
	if !ok {
		return
	}
	if _, err := app.DB.ExecContext(r.Context(), `DELETE FROM device_tokens WHERE token = $1 AND parent_id = $2`, token, parentID); err != nil {
		respondInternalError(w, "Internal server error", "RemoveDeviceTokenHandler: delete failed", err, "parent_id", parentID, "token", notify.TokenFingerprint(token))
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "success", "message": "Device token removed"})
}
