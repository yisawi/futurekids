package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"future_kids/internal/auth"
	"future_kids/internal/phone"
	"future_kids/internal/ratelimit"
	"future_kids/internal/tz"

	"github.com/jackc/pgx/v5/pgconn"
	excelize "github.com/xuri/excelize/v2"
	"golang.org/x/crypto/bcrypt"
)

type AdminLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type DashboardStats struct {
	TotalStudents int `json:"total_students"`
	TotalParents  int `json:"total_parents"`
	PresentToday  int `json:"present_today"`
	ExcusedToday  int `json:"excused_today"`
	AbsentToday   int `json:"absent_today"`
}

type StudentPayload struct {
	ID          int    `json:"id,omitempty"`
	Name        string `json:"name"`
	ParentName  string `json:"parent_name"`
	ParentPhone string `json:"parent_phone"`
	ParentPin   string `json:"parent_pin,omitempty"`
	RfidTag     string `json:"rfid_tag"` // IMPORTANT: this must equal the PIN the student is enrolled under on the ZKTeco device, not a physical RFID card value
	// Nullable. On PUT, an omitted or null grade/section (and an empty rfid_tag) keeps the stored value.
	Grade   *string `json:"grade"`
	Section *string `json:"section"`
}

// requestedDate returns the ?date= query param (YYYY-MM-DD), defaulting to today in
// Asia/Baghdad; ok is false (and 400 has been written) when the value is malformed.
func requestedDate(w http.ResponseWriter, r *http.Request) (date string, ok bool) {
	d := r.URL.Query().Get("date")
	if d == "" {
		return tz.Today(), true
	}
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		respondError(w, http.StatusBadRequest, "date must be formatted as YYYY-MM-DD")
		return "", false
	}
	if !yearInRange(t) {
		respondError(w, http.StatusBadRequest, "date must have a year from 2000 to 2100")
		return "", false
	}
	return d, true
}

// Dates and months the API accepts lie in years MinYear to MaxYear.
const (
	MinYear = 2000
	MaxYear = 2100
)

func yearInRange(t time.Time) bool { return t.Year() >= MinYear && t.Year() <= MaxYear }

// tooLong returns "<field> must be at most max characters" when value has more than max
// characters, or "". Characters are counted as PostgreSQL counts them for varchar(max), which
// silently drops excess trailing spaces instead of rejecting them, so trailing spaces are ignored.
func tooLong(field, value string, max int) string {
	if utf8.RuneCountInString(strings.TrimRight(value, " ")) > max {
		return fmt.Sprintf("%s must be at most %d characters", field, max)
	}
	return ""
}

// firstProblem returns the first non-empty message.
func firstProblem(msgs ...string) string {
	for _, m := range msgs {
		if m != "" {
			return m
		}
	}
	return ""
}

func (app *AppEnv) AdminLoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req AdminLoginRequest
	if !decodeJSONBody(w, r, &req, "Invalid request") {
		return
	}

	if app.AdminUserLimiter == nil || app.AdminIPLimiter == nil {
		slog.Error("AdminLoginHandler: admin login limiters are not configured")
		respondError(w, http.StatusInternalServerError, "Internal server error")
		return
	}
	ip, identified := app.ClientIP.Resolve(r)
	userKey := limiterKey("user:", req.Username) + "|" + ip
	if ok, retryAfter := app.AdminUserLimiter.Allow(userKey); !ok {
		logRateLimited("AdminLoginHandler", r, "username", logValue(req.Username), "ip", ip, "limit", "username+ip")
		respondTooManyAttempts(w, retryAfter)
		return
	}
	userOutcome := ratelimit.Released
	defer func() { app.AdminUserLimiter.Finish(userKey, userOutcome) }()
	ipOutcome := ratelimit.Released
	if identified {
		ipKey := "ip:" + ip
		if ok, retryAfter := app.AdminIPLimiter.Allow(ipKey); !ok {
			logRateLimited("AdminLoginHandler", r, "username", logValue(req.Username), "ip", ip, "limit", "ip")
			respondTooManyAttempts(w, retryAfter)
			return
		}
		defer func() { app.AdminIPLimiter.Finish(ipKey, ipOutcome) }()
	}

	var storedHash string
	var sessionVersion int
	query := `SELECT password_hash, session_version FROM admins WHERE username = $1`
	err := app.DB.QueryRowContext(r.Context(), query, req.Username).Scan(&storedHash, &sessionVersion)
	switch {
	case err == sql.ErrNoRows:
		burnBcrypt(req.Password)
		userOutcome, ipOutcome = ratelimit.Failed, ratelimit.Failed
		respondError(w, http.StatusUnauthorized, "بيانات الدخول غير صحيحة")
		return
	case err != nil:
		respondInternalError(w, "Internal server error", "AdminLoginHandler: query failed", err, "username", logValue(req.Username))
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(req.Password)); err != nil {
		userOutcome, ipOutcome = ratelimit.Failed, ratelimit.Failed
		respondError(w, http.StatusUnauthorized, "بيانات الدخول غير صحيحة")
		return
	}
	userOutcome = ratelimit.Succeeded

	tokenString, err := auth.GenerateAdminToken(req.Username, sessionVersion)
	if err != nil {
		respondInternalError(w, "Could not generate token", "AdminLoginHandler: token generation failed", err, "username", req.Username)
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status": "success",
		"data":   map[string]string{"token": tokenString},
	})
}

func (app *AppEnv) AdminDashboardHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	today := tz.Today()

	// استعلام CTE ذكي يحسب جميع الإحصائيات دفعة واحدة وبأداء عالٍ جداً
	query := `
		WITH stats AS (
			SELECT 
				COUNT(*) as total_students,
				COUNT(*) FILTER (WHERE st.status = 'Present') as present_today,
				COUNT(*) FILTER (WHERE st.status = 'Excused') as excused_today
			FROM students s
			CROSS JOIN LATERAL get_student_status(s.id, $1::DATE) st
			WHERE s.is_active = true
		)
		SELECT 
			total_students, 
			(SELECT COUNT(*) FROM parents) as total_parents, 
			present_today, 
			excused_today, 
			GREATEST(0, total_students - present_today - excused_today) as absent_today
		FROM stats
	`

	var stats DashboardStats
	err := app.DB.QueryRowContext(r.Context(), query, today).Scan(
		&stats.TotalStudents,
		&stats.TotalParents,
		&stats.PresentToday,
		&stats.ExcusedToday,
		&stats.AbsentToday,
	)

	if err != nil {
		respondInternalError(w, "Database error", "AdminDashboardHandler: query failed", err, "date", today)
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status": "success",
		"date":   today,
		"data":   stats,
	})
}

// InvalidParentPhoneMessage is the 400 message for a parent_phone that is not an Iraqi mobile number.
const InvalidParentPhoneMessage = "parent_phone must be an Iraqi mobile number, for example 07XXXXXXXXX or +9647XXXXXXXXX"

// validateStudentPayload trims the required fields in place, rewrites parent_phone to its
// canonical form, and returns an error message for the first invalid field, or "" when the
// payload is valid.
func validateStudentPayload(req *StudentPayload) string {
	req.Name = strings.TrimSpace(req.Name)
	req.ParentName = strings.TrimSpace(req.ParentName)
	req.ParentPhone = strings.TrimSpace(req.ParentPhone)
	switch {
	case req.ParentPhone == "":
		return "parent_phone is required"
	case req.ParentName == "":
		return "parent_name is required"
	case req.Name == "":
		return "name is required"
	}
	canonical, ok := phone.Normalize(req.ParentPhone)
	if !ok {
		return InvalidParentPhoneMessage
	}
	req.ParentPhone = canonical
	if strings.TrimSpace(req.ParentPin) != "" && !pinFormat.MatchString(req.ParentPin) {
		return PINFormatMessage
	}
	grade, section := "", ""
	if req.Grade != nil {
		grade = strings.TrimSpace(*req.Grade)
		req.Grade = &grade
	}
	if req.Section != nil {
		section = strings.TrimSpace(*req.Section)
		req.Section = &section
	}
	return firstProblem(
		tooLong("name", req.Name, 100),
		tooLong("parent_name", req.ParentName, 255),
		tooLong("parent_phone", req.ParentPhone, 20),
		tooLong("rfid_tag", req.RfidTag, 50),
		tooLong("grade", grade, 50),
		tooLong("section", section, 50),
	)
}

// A PIN the admin sets is exactly 6 ASCII digits. Login does not check the format, so parents
// whose PIN was set before this rule keep logging in with it.
var pinFormat = regexp.MustCompile(`^[0-9]{6}$`)

// PINFormatMessage is the 400 message for a parent_pin that is set but not 6 ASCII digits.
const PINFormatMessage = "parent_pin must be exactly 6 digits (0-9)"

// Messages for the student writes.
const (
	PINRequiredMessage  = "parent_pin is required for a new parent (parent_phone is not registered yet)"
	RFIDTagTakenMessage = "rfid_tag is already used by another student"
	rfidTagUniqueIndex  = "students_rfid_tag_key"
)

var errNewParentNeedsPIN = errors.New("new parent without a PIN")

// resolveParent finds or creates, inside tx, the parent with phone and returns its id. With a
// PIN it creates the parent or replaces the PIN, which signs the parent out (session_version
// rises, device tokens are removed). Without one it only updates an existing parent's name and
// returns errNewParentNeedsPIN when the number is not registered, so no parent is ever written
// without a PIN chosen by the admin.
func resolveParent(ctx context.Context, tx *sql.Tx, name, phone, pin string) (int, error) {
	var id int
	if strings.TrimSpace(pin) == "" {
		err := tx.QueryRowContext(ctx, `UPDATE parents SET full_name = $1 WHERE phone_number = $2 RETURNING id`, name, phone).Scan(&id)
		if err == sql.ErrNoRows {
			return 0, errNewParentNeedsPIN
		}
		return id, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pin), bcrypt.DefaultCost)
	if err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO parents (full_name, phone_number, pin_code) VALUES ($1, $2, $3)
		ON CONFLICT (phone_number) DO UPDATE
		SET full_name = EXCLUDED.full_name,
		    pin_code = EXCLUDED.pin_code,
		    session_version = parents.session_version + 1
		RETURNING id`, name, phone, string(hash)).Scan(&id); err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM device_tokens WHERE parent_id = $1`, id)
	return id, err
}

// respondStudentWriteError answers a failed student create or update: 400 for a new parent
// without a PIN, 409 for an rfid_tag another student has, and a logged 500 otherwise.
func respondStudentWriteError(w http.ResponseWriter, op string, err error, attrs ...any) {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, errNewParentNeedsPIN):
		respondError(w, http.StatusBadRequest, PINRequiredMessage)
	case errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == rfidTagUniqueIndex:
		respondError(w, http.StatusConflict, RFIDTagTakenMessage)
	default:
		respondInternalError(w, "Internal server error", op, err, attrs...)
	}
}

func (app *AppEnv) AdminStudentsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {

	// 1. القراءة (JOIN بين جدول الطلاب والآباء)
	case http.MethodGet:
		query := `
			SELECT 
				s.id, 
				s.full_name, 
				COALESCE(p.full_name, '') as parent_name,
				COALESCE(p.phone_number, ''),
				COALESCE(s.rfid_tag, ''),
				s.grade,
				s.section
			FROM students s
			LEFT JOIN parents p ON s.parent_id = p.id
			WHERE s.is_active = true
			ORDER BY s.id DESC
		`
		rows, err := app.DB.QueryContext(r.Context(), query)
		if err != nil {
			respondInternalError(w, "Database error", "AdminStudentsHandler: query failed", err)
			return
		}
		defer rows.Close()

		var students []StudentPayload
		for rows.Next() {
			var s StudentPayload
			if err := rows.Scan(&s.ID, &s.Name, &s.ParentName, &s.ParentPhone, &s.RfidTag, &s.Grade, &s.Section); err != nil {
				respondInternalError(w, "Database error", "AdminStudentsHandler: scan failed", err)
				return
			}
			students = append(students, s)
		}
		if err := rows.Err(); err != nil {
			respondInternalError(w, "Database error", "AdminStudentsHandler: rows iteration failed", err)
			return
		}
		if students == nil {
			students = []StudentPayload{}
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "data": students})

	// 2. الإضافة (CTE ذكي لإنشاء/تحديث ولي الأمر وربطه بالطالب فوراً)
	case http.MethodPost:
		var req StudentPayload
		if !decodeJSONBody(w, r, &req, "Invalid request body") {
			return
		}
		if msg := validateStudentPayload(&req); msg != "" {
			respondError(w, http.StatusBadRequest, msg)
			return
		}
		if req.RfidTag == "" {
			req.RfidTag = fmt.Sprintf("admin-%d", time.Now().UnixNano())
		}

		err := func() error {
			tx, err := app.DB.BeginTx(r.Context(), nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			parentID, err := resolveParent(r.Context(), tx, req.ParentName, req.ParentPhone, req.ParentPin)
			if err != nil {
				return err
			}
			if err := tx.QueryRowContext(r.Context(), `
				INSERT INTO students (full_name, rfid_tag, parent_id, grade, section)
				VALUES ($1, $2, $3, $4, $5)
				RETURNING id`, req.Name, req.RfidTag, parentID, req.Grade, req.Section).Scan(&req.ID); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err != nil {
			respondStudentWriteError(w, "AdminStudentsHandler: create failed", err, "parent_phone", req.ParentPhone, "rfid_tag", req.RfidTag)
			return
		}
		req.ParentPin = "" // Don't echo it back
		respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "message": "Student created", "data": req})

	// 3. التعديل
	case http.MethodPut:
		var req StudentPayload
		if !decodeJSONBody(w, r, &req, "Invalid request body or missing ID") {
			return
		}
		if req.ID == 0 {
			respondError(w, http.StatusBadRequest, "Invalid request body or missing ID")
			return
		}
		if msg := validateStudentPayload(&req); msg != "" {
			respondError(w, http.StatusBadRequest, msg)
			return
		}
		var errStudentNotFound = errors.New("student not found")
		err := func() error {
			tx, err := app.DB.BeginTx(r.Context(), nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			parentID, err := resolveParent(r.Context(), tx, req.ParentName, req.ParentPhone, req.ParentPin)
			if err != nil {
				return err
			}
			res, err := tx.ExecContext(r.Context(), `
				UPDATE students
				SET full_name = $1,
				    rfid_tag = COALESCE(NULLIF($2::text, ''), rfid_tag),
				    parent_id = $3,
				    grade = COALESCE($4, grade),
				    section = COALESCE($5, section)
				WHERE id = $6`, req.Name, req.RfidTag, parentID, req.Grade, req.Section, req.ID)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n == 0 {
				return errStudentNotFound
			}
			return tx.Commit()
		}()
		if errors.Is(err, errStudentNotFound) {
			respondError(w, http.StatusNotFound, "Student not found")
			return
		}
		if err != nil {
			respondStudentWriteError(w, "AdminStudentsHandler: update failed", err, "student_id", req.ID)
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "message": "Student updated"})

	// 4. الحذف (يحذف الطالب فقط ويبقي بيانات ولي الأمر)
	case http.MethodDelete:
		id, err := strconv.Atoi(r.URL.Query().Get("id"))
		if err != nil || id == 0 {
			respondError(w, http.StatusBadRequest, "Invalid student ID")
			return
		}

		result, err := app.DB.ExecContext(r.Context(), `UPDATE students SET is_active = false WHERE id = $1`, id)
		if err != nil {
			respondInternalError(w, "Internal server error", "AdminStudentsHandler: delete failed", err, "student_id", id)
			return
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			respondInternalError(w, "Internal server error", "AdminStudentsHandler: rows affected failed", err, "student_id", id)
			return
		}
		if rowsAffected == 0 {
			respondError(w, http.StatusNotFound, "Student not found")
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "message": "Student deleted"})

	default:
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

type LeavePayload struct {
	StudentID int    `json:"student_id"`
	LeaveDate string `json:"leave_date"` // Format: YYYY-MM-DD
	Notes     string `json:"notes"`
}

// LeaveCancelledMessage answers every valid DELETE /api/admin/leaves, whether or not a leave existed.
const LeaveCancelledMessage = "No leave remains for this student on this date"

// cancelLeave removes the leave of ?student_id= on ?date=. It is idempotent: a missing leave or
// an unknown student is also 200, so retries never fail and nothing about the student is revealed.
func (app *AppEnv) cancelLeave(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	idParam, date := q.Get("student_id"), q.Get("date")
	if idParam == "" || date == "" {
		respondError(w, http.StatusBadRequest, "student_id and date are required")
		return
	}
	studentID, err := strconv.Atoi(idParam)
	if err != nil || studentID < 1 {
		respondError(w, http.StatusBadRequest, "student_id must be a positive student id")
		return
	}
	leaveDate, err := time.Parse("2006-01-02", date)
	if err != nil {
		respondError(w, http.StatusBadRequest, "date must be formatted as YYYY-MM-DD")
		return
	}
	if !yearInRange(leaveDate) {
		respondError(w, http.StatusBadRequest, "date must have a year from 2000 to 2100")
		return
	}
	res, err := app.DB.ExecContext(r.Context(), `DELETE FROM student_leaves WHERE student_id = $1 AND leave_date = $2`, studentID, date)
	if err != nil {
		respondInternalError(w, "Failed to cancel leave", "AdminCreateLeaveHandler: delete failed", err, "student_id", studentID, "date", date)
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		respondInternalError(w, "Failed to cancel leave", "AdminCreateLeaveHandler: rows affected failed", err, "student_id", studentID, "date", date)
		return
	}
	slog.Info("Leave cancelled", "student_id", studentID, "date", date, "removed", n > 0)
	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "message": LeaveCancelledMessage})
}

func (app *AppEnv) AdminCreateLeaveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		app.cancelLeave(w, r)
		return
	}
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req LeavePayload
	if !decodeJSONBody(w, r, &req, "Invalid request payload") {
		return
	}

	if req.StudentID == 0 || req.LeaveDate == "" {
		respondError(w, http.StatusBadRequest, "student_id and leave_date are required")
		return
	}
	if req.StudentID < 0 {
		respondError(w, http.StatusBadRequest, "student_id must be a positive student id")
		return
	}
	leaveDate, err := time.Parse("2006-01-02", req.LeaveDate)
	if err != nil {
		respondError(w, http.StatusBadRequest, "leave_date must be formatted as YYYY-MM-DD")
		return
	}
	if !yearInRange(leaveDate) {
		respondError(w, http.StatusBadRequest, "leave_date must have a year from 2000 to 2100")
		return
	}

	query := `
		INSERT INTO student_leaves (student_id, leave_date, notes)
		SELECT s.id, $2, $3 FROM students s WHERE s.id = $1 AND s.is_active = true
		ON CONFLICT (student_id, leave_date)
		DO UPDATE SET notes = EXCLUDED.notes
		RETURNING id
	`

	var leaveID int
	err = app.DB.QueryRowContext(r.Context(), query, req.StudentID, req.LeaveDate, req.Notes).Scan(&leaveID)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "Student not found")
		return
	}
	if err != nil {
		respondInternalError(w, "Failed to create leave record", "AdminCreateLeaveHandler: insert failed", err, "student_id", req.StudentID, "leave_date", req.LeaveDate)
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "success",
		"message": "Leave recorded successfully",
		"data":    map[string]int{"leave_id": leaveID},
	})
}

func (app *AppEnv) AdminDailyAttendanceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	dateParam, ok := requestedDate(w, r)
	if !ok {
		return
	}

	// استعلام مركب يجلب كل الطلاب ويحدد حالتهم بناءً على الجداول المرتبطة
	query := `
		SELECT 
			s.id, 
			s.full_name,
			st.status,
			st.first_check AS check_in_time,
			st.last_check AS check_out_time
		FROM students s
		CROSS JOIN LATERAL get_student_status(s.id, $1::DATE) st
		WHERE s.is_active = true AND ` + fmt.Sprintf(studentExistedOnSQL, "$1::DATE") + `
		ORDER BY st.status DESC, s.full_name ASC, s.id ASC
	`

	rows, err := app.DB.QueryContext(r.Context(), query, dateParam)
	if err != nil {
		respondInternalError(w, "Database error", "AdminDailyAttendanceHandler: query failed", err, "date", dateParam)
		return
	}
	defer rows.Close()

	var records []DailyAttendanceDTO
	for rows.Next() {
		var rec DailyAttendanceDTO
		if err := rows.Scan(&rec.StudentID, &rec.FullName, &rec.Status, &rec.CheckInTime, &rec.CheckOutTime); err != nil {
			respondInternalError(w, "Database error", "AdminDailyAttendanceHandler: scan failed", err, "date", dateParam)
			return
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "AdminDailyAttendanceHandler: rows iteration failed", err, "date", dateParam)
		return
	}

	if records == nil {
		records = []DailyAttendanceDTO{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status": "success",
		"date":   dateParam,
		"data":   records,
	})
}

func (app *AppEnv) AdminExportExcelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	dateParam, ok := requestedDate(w, r)
	if !ok {
		return
	}

	var schoolName string
	switch err := app.DB.QueryRowContext(r.Context(), `SELECT setting_value FROM settings WHERE setting_key = 'school_name'`).Scan(&schoolName); {
	case err == sql.ErrNoRows:
		slog.Warn("AdminExportExcelHandler: school_name setting is missing; the report header will be blank")
	case err != nil:
		respondInternalError(w, "Database error", "AdminExportExcelHandler: school name query failed", err, "date", dateParam)
		return
	}

	query := `
		SELECT 
			s.id, 
			s.full_name, 
			COALESCE(s.grade, 'غير محدد'), 
			COALESCE(s.section, '-'), 
			COALESCE(p.full_name, '-') as parent_name,
			COALESCE(p.phone_number, '-') as phone_number,
			CASE st.status
				WHEN 'Present' THEN 'حاضر'
				WHEN 'Excused' THEN 'مجاز'
				ELSE 'غائب'
			END as status,
			COALESCE(st.first_check, '') as check_in_time,
			COALESCE(st.last_check, '') as check_out_time
		FROM students s
		LEFT JOIN parents p ON s.parent_id = p.id
		CROSS JOIN LATERAL get_student_status(s.id, $1::DATE) st
		WHERE s.is_active = true AND ` + fmt.Sprintf(studentExistedOnSQL, "$1::DATE") + `
		ORDER BY st.status DESC, s.full_name ASC
	`

	rows, err := app.DB.QueryContext(r.Context(), query, dateParam)
	if err != nil {
		respondInternalError(w, "Database error", "AdminExportExcelHandler: query failed", err, "date", dateParam)
		return
	}
	defer rows.Close()

	f := excelize.NewFile()
	defer f.Close()

	sheet := "Sheet1"

	// 1. تحويل اتجاه الشيت من اليمين إلى اليسار (RTL)
	rtlEnable := true
	f.SetSheetView(sheet, 0, &excelize.ViewOptions{RightToLeft: &rtlEnable})

	// 2. إعداد تنسيق العناوين (توسيط وخط عريض)
	titleStyle, _ := f.NewStyle(&excelize.Style{
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"},
		Font:      &excelize.Font{Bold: true, Size: 14},
	})

	// 3. كتابة الترويسة الرسمية ودمج الخلايا من العمود A إلى H
	f.MergeCell(sheet, "A1", "I1")
	f.SetCellValue(sheet, "A1", "وزارة التربية والتعليم")

	f.MergeCell(sheet, "A2", "I2")
	f.SetCellValue(sheet, "A2", schoolName)

	f.MergeCell(sheet, "A3", "I3")
	f.SetCellValue(sheet, "A3", fmt.Sprintf("تقرير الحضور والغياب اليومي الشامل - تاريخ: %s", dateParam))

	// تطبيق التنسيق على الترويسة
	f.SetCellStyle(sheet, "A1", "I3", titleStyle)

	// 4. إعداد ترويسة أعمدة الجدول (في الصف الخامس لترك مسافة)
	headerStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"#E0E0E0"}, Pattern: 1},
	})

	headers := []string{"رقم الطالب", "اسم الطالب", "الصف", "الشعبة", "ولي الأمر", "رقم الهاتف", "الحالة", "وقت الدخول", "وقت الخروج"}
	for i, header := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 5)
		f.SetCellValue(sheet, cell, header)
	}
	f.SetCellStyle(sheet, "A5", "I5", headerStyle)

	// 5. تعبئة البيانات (ابتداءً من الصف السادس)
	rowIndex := 6
	for rows.Next() {
		var id int
		var studentName, grade, section, parentName, phone, status, checkIn, checkOut string
		if err := rows.Scan(&id, &studentName, &grade, &section, &parentName, &phone, &status, &checkIn, &checkOut); err != nil {
			respondInternalError(w, "Database error", "AdminExportExcelHandler: scan failed", err, "date", dateParam, "row", rowIndex)
			return
		}
		f.SetCellValue(sheet, fmt.Sprintf("A%d", rowIndex), id)
		f.SetCellValue(sheet, fmt.Sprintf("B%d", rowIndex), studentName)
		f.SetCellValue(sheet, fmt.Sprintf("C%d", rowIndex), grade)
		f.SetCellValue(sheet, fmt.Sprintf("D%d", rowIndex), section)
		f.SetCellValue(sheet, fmt.Sprintf("E%d", rowIndex), parentName)
		f.SetCellValue(sheet, fmt.Sprintf("F%d", rowIndex), phone)
		f.SetCellValue(sheet, fmt.Sprintf("G%d", rowIndex), status)
		f.SetCellValue(sheet, fmt.Sprintf("H%d", rowIndex), checkIn)
		f.SetCellValue(sheet, fmt.Sprintf("I%d", rowIndex), checkOut)
		rowIndex++
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "AdminExportExcelHandler: rows iteration failed", err, "date", dateParam)
		return
	}

	// Write to buffer first so we can return a clean HTTP error if generation fails.
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		respondInternalError(w, "Failed to generate excel file", "AdminExportExcelHandler: excel write failed", err, "date", dateParam)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=attendance_%s.xlsx", dateParam))
	w.Write(buf.Bytes())
}

type SettingPayload struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (app *AppEnv) AdminSettingsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		query := `SELECT setting_key, setting_value FROM settings`
		rows, err := app.DB.QueryContext(r.Context(), query)
		if err != nil {
			respondInternalError(w, "Database error", "AdminSettingsHandler: query failed", err)
			return
		}
		defer rows.Close()

		settings := make(map[string]string)
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				respondInternalError(w, "Database error", "AdminSettingsHandler: scan failed", err)
				return
			}
			settings[k] = v
		}
		if err := rows.Err(); err != nil {
			respondInternalError(w, "Database error", "AdminSettingsHandler: rows iteration failed", err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "data": settings})

	case http.MethodPut:
		var req SettingPayload
		if !decodeJSONBody(w, r, &req, "Invalid payload") {
			return
		}
		if req.Key == "" {
			respondError(w, http.StatusBadRequest, "Invalid payload")
			return
		}
		if msg := tooLong("key", req.Key, 100); msg != "" {
			respondError(w, http.StatusBadRequest, msg)
			return
		}

		query := `
			INSERT INTO settings (setting_key, setting_value, updated_at) 
			VALUES ($1, $2, CURRENT_TIMESTAMP)
			ON CONFLICT (setting_key) 
			DO UPDATE SET setting_value = EXCLUDED.setting_value, updated_at = CURRENT_TIMESTAMP
		`
		_, err := app.DB.ExecContext(r.Context(), query, req.Key, req.Value)
		if err != nil {
			respondInternalError(w, "Failed to update setting", "AdminSettingsHandler: exec failed", err, "key", req.Key)
			return
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "message": "Setting saved"})
	default:
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

type DevicePayload struct {
	SerialNumber string `json:"serial_number"`
	LocationName string `json:"location_name"`
	IsActive     bool   `json:"is_active"`
	LastSync     string `json:"last_sync,omitempty"`
}

// DeviceCreateRequest registers a device; an omitted is_active means active.
type DeviceCreateRequest struct {
	SerialNumber string `json:"serial_number"`
	LocationName string `json:"location_name"`
	IsActive     *bool  `json:"is_active"`
}

// DeviceUpdateRequest changes the fields it carries and keeps the stored value of any omitted one.
type DeviceUpdateRequest struct {
	SerialNumber string  `json:"serial_number"`
	LocationName *string `json:"location_name"`
	IsActive     *bool   `json:"is_active"`
}

func (app *AppEnv) AdminDevicesHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		query := `SELECT serial_number, COALESCE(location_name, '') AS location_name, COALESCE(is_active, true) AS is_active, COALESCE(TO_CHAR(last_sync, 'YYYY-MM-DD HH24:MI:SS'), '') FROM devices ORDER BY location_name ASC`
		rows, err := app.DB.QueryContext(r.Context(), query)
		if err != nil {
			respondInternalError(w, "Database error", "AdminDevicesHandler: query failed", err)
			return
		}
		defer rows.Close()

		var devices []DevicePayload
		for rows.Next() {
			var d DevicePayload
			if err := rows.Scan(&d.SerialNumber, &d.LocationName, &d.IsActive, &d.LastSync); err != nil {
				respondInternalError(w, "Database error", "AdminDevicesHandler: scan failed", err)
				return
			}
			devices = append(devices, d)
		}
		if err := rows.Err(); err != nil {
			respondInternalError(w, "Database error", "AdminDevicesHandler: rows iteration failed", err)
			return
		}
		if devices == nil {
			devices = []DevicePayload{}
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "data": devices})

	case http.MethodPost:
		var req DeviceCreateRequest
		if !decodeJSONBody(w, r, &req, "Invalid payload or missing SN") {
			return
		}
		if req.SerialNumber == "" {
			respondError(w, http.StatusBadRequest, "Invalid payload or missing SN")
			return
		}
		if msg := firstProblem(tooLong("serial_number", req.SerialNumber, 50), tooLong("location_name", req.LocationName, 50)); msg != "" {
			respondError(w, http.StatusBadRequest, msg)
			return
		}
		active := true
		if req.IsActive != nil {
			active = *req.IsActive
		}

		query := `INSERT INTO devices (serial_number, location_name, is_active) VALUES ($1, $2, $3)`
		_, err := app.DB.ExecContext(r.Context(), query, req.SerialNumber, req.LocationName, active)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			respondError(w, http.StatusConflict, "Device SN already exists")
			return
		} else if err != nil {
			respondInternalError(w, "Internal server error", "AdminDevicesHandler: insert failed", err, "device_sn", req.SerialNumber)
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "message": "Device added successfully"})

	case http.MethodPut:
		var req DeviceUpdateRequest
		if !decodeJSONBody(w, r, &req, "Invalid payload") {
			return
		}
		if req.SerialNumber == "" {
			respondError(w, http.StatusBadRequest, "Invalid payload")
			return
		}
		if req.LocationName == nil && req.IsActive == nil {
			respondError(w, http.StatusBadRequest, "location_name or is_active is required")
			return
		}
		location := ""
		if req.LocationName != nil {
			location = *req.LocationName
		}
		if msg := firstProblem(tooLong("serial_number", req.SerialNumber, 50), tooLong("location_name", location, 50)); msg != "" {
			respondError(w, http.StatusBadRequest, msg)
			return
		}

		query := `UPDATE devices SET location_name = COALESCE($1, location_name), is_active = COALESCE($2, is_active) WHERE serial_number = $3`
		res, err := app.DB.ExecContext(r.Context(), query, req.LocationName, req.IsActive, req.SerialNumber)
		if err != nil {
			respondInternalError(w, "Failed to update device", "AdminDevicesHandler: update failed", err, "device_sn", req.SerialNumber)
			return
		}

		rowsAffected, _ := res.RowsAffected()
		if rowsAffected == 0 {
			respondError(w, http.StatusNotFound, "Device not found")
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "message": "Device updated successfully"})

	case http.MethodDelete:
		sn := r.URL.Query().Get("sn")
		if sn == "" {
			respondError(w, http.StatusBadRequest, "Missing device SN")
			return
		}

		// تعطيل الجهاز بدلاً من حذفه للحفاظ على تكامل قاعدة البيانات
		query := `UPDATE devices SET is_active = false WHERE serial_number = $1`
		_, err := app.DB.ExecContext(r.Context(), query, sn)
		if err != nil {
			respondInternalError(w, "Failed to disable device", "AdminDevicesHandler: disable failed", err, "device_sn", sn)
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "message": "Device disabled logically"})

	default:
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// SchedulePeriod is one lesson in a class's weekly schedule. A nil TeacherName is stored as NULL.
type SchedulePeriod struct {
	DayOfWeek    string  `json:"day_of_week"`
	PeriodNumber int     `json:"period_number"`
	SubjectName  string  `json:"subject_name"`
	TeacherName  *string `json:"teacher_name"`
}

// ScheduleRequest replaces the whole weekly schedule of one class. Periods must be present;
// an empty array clears the class.
type ScheduleRequest struct {
	Grade   string            `json:"grade"`
	Section string            `json:"section"`
	Periods *[]SchedulePeriod `json:"periods"`
}

// A class has at most MaxPeriodNumber periods on each of the five school days.
const (
	MaxPeriodNumber    = 12
	MaxSchedulePeriods = 5 * MaxPeriodNumber
)

// schoolDays maps a folded day name (see canonicalSchoolDay) to the form stored in
// weekly_schedules. Friday and Saturday are not school days.
var schoolDays = map[string]string{
	"sunday": "الأحد", "الاحد": "الأحد",
	"monday": "الإثنين", "الاثنين": "الإثنين",
	"tuesday": "الثلاثاء", "الثلاثاء": "الثلاثاء",
	"wednesday": "الأربعاء", "الاربعاء": "الأربعاء",
	"thursday": "الخميس", "الخميس": "الخميس",
}

var hamzaFold = strings.NewReplacer("أ", "ا", "إ", "ا", "آ", "ا")

// canonicalSchoolDay folds day the way weekdayRankSQL does (trimmed, lower-cased, أ/إ/آ as ا)
// and returns its stored Arabic form, or false when it is not Sunday to Thursday.
func canonicalSchoolDay(day string) (string, bool) {
	c, ok := schoolDays[hamzaFold.Replace(strings.ToLower(strings.TrimSpace(day)))]
	return c, ok
}

func scheduleClassProblem(grade, section string) string {
	switch {
	case grade == "":
		return "grade is required"
	case section == "":
		return "section is required"
	}
	return firstProblem(tooLong("grade", grade, 50), tooLong("section", section, 50))
}

// validateScheduleRequest trims the class and every period in place, stores canonical day
// names and blank teachers as nil, and returns the first problem, or "" when the request is valid.
func validateScheduleRequest(req *ScheduleRequest) string {
	req.Grade = strings.TrimSpace(req.Grade)
	req.Section = strings.TrimSpace(req.Section)
	if msg := scheduleClassProblem(req.Grade, req.Section); msg != "" {
		return msg
	}
	if req.Periods == nil {
		return "periods is required; send an empty array to clear the schedule"
	}
	periods := *req.Periods
	if len(periods) > MaxSchedulePeriods {
		return fmt.Sprintf("periods must contain at most %d entries", MaxSchedulePeriods)
	}
	seen := map[string]bool{}
	for i := range periods {
		p := &periods[i]
		field := func(name string) string { return fmt.Sprintf("periods[%d].%s", i, name) }
		day, ok := canonicalSchoolDay(p.DayOfWeek)
		if !ok {
			return field("day_of_week") + " must be a school day, Sunday to Thursday (Arabic or English)"
		}
		p.DayOfWeek = day
		if p.PeriodNumber < 1 || p.PeriodNumber > MaxPeriodNumber {
			return fmt.Sprintf("%s must be from 1 to %d", field("period_number"), MaxPeriodNumber)
		}
		p.SubjectName = strings.TrimSpace(p.SubjectName)
		if p.SubjectName == "" {
			return field("subject_name") + " is required"
		}
		teacher := ""
		if p.TeacherName != nil {
			teacher = strings.TrimSpace(*p.TeacherName)
			p.TeacherName = &teacher
			if teacher == "" {
				p.TeacherName = nil
			}
		}
		if msg := firstProblem(tooLong(field("subject_name"), p.SubjectName, 100), tooLong(field("teacher_name"), teacher, 100)); msg != "" {
			return msg
		}
		key := fmt.Sprintf("%s|%d", day, p.PeriodNumber)
		if seen[key] {
			return fmt.Sprintf("periods contain day_of_week %s with period_number %d more than once", day, p.PeriodNumber)
		}
		seen[key] = true
	}
	return ""
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// readSchedule returns one class's periods in the parent schedule's order: school week, then
// period_number.
func readSchedule(ctx context.Context, q queryer, grade, section string) ([]SchedulePeriod, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT ws.day_of_week, ws.period_number, ws.subject_name, ws.teacher_name
		FROM weekly_schedules ws
		CROSS JOIN LATERAL (SELECT `+weekdayRankSQL+` AS rank) day
		WHERE ws.grade = $1 AND ws.section = $2
		ORDER BY day.rank ASC, CASE WHEN day.rank = 8 THEN ws.day_of_week END ASC, ws.period_number ASC, ws.id ASC`, grade, section)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	periods := []SchedulePeriod{}
	for rows.Next() {
		var p SchedulePeriod
		if err := rows.Scan(&p.DayOfWeek, &p.PeriodNumber, &p.SubjectName, &p.TeacherName); err != nil {
			return nil, err
		}
		periods = append(periods, p)
	}
	return periods, rows.Err()
}

func respondSchedule(w http.ResponseWriter, grade, section string, periods []SchedulePeriod) {
	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "grade": grade, "section": section, "data": periods})
}

func (app *AppEnv) AdminScheduleHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		grade := strings.TrimSpace(r.URL.Query().Get("grade"))
		section := strings.TrimSpace(r.URL.Query().Get("section"))
		if msg := scheduleClassProblem(grade, section); msg != "" {
			respondError(w, http.StatusBadRequest, msg)
			return
		}
		periods, err := readSchedule(r.Context(), app.DB, grade, section)
		if err != nil {
			respondInternalError(w, "Database error", "AdminScheduleHandler: query failed", err, "grade", grade, "section", section)
			return
		}
		respondSchedule(w, grade, section, periods)

	case http.MethodPut:
		var req ScheduleRequest
		if !decodeJSONBody(w, r, &req, "Invalid request body") {
			return
		}
		if msg := validateScheduleRequest(&req); msg != "" {
			respondError(w, http.StatusBadRequest, msg)
			return
		}
		var saved []SchedulePeriod
		err := func() error {
			tx, err := app.DB.BeginTx(r.Context(), nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(r.Context(), `LOCK TABLE weekly_schedules IN SHARE ROW EXCLUSIVE MODE`); err != nil {
				return err
			}
			if _, err := tx.ExecContext(r.Context(), `DELETE FROM weekly_schedules WHERE grade = $1 AND section = $2`, req.Grade, req.Section); err != nil {
				return err
			}
			for _, p := range *req.Periods {
				if _, err := tx.ExecContext(r.Context(), `
					INSERT INTO weekly_schedules (grade, section, day_of_week, period_number, subject_name, teacher_name)
					VALUES ($1, $2, $3, $4, $5, $6)`, req.Grade, req.Section, p.DayOfWeek, p.PeriodNumber, p.SubjectName, p.TeacherName); err != nil {
					return err
				}
			}
			if saved, err = readSchedule(r.Context(), tx, req.Grade, req.Section); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err != nil {
			respondInternalError(w, "Failed to save schedule", "AdminScheduleHandler: save failed", err, "grade", req.Grade, "section", req.Section)
			return
		}
		slog.Info("Schedule saved", "grade", req.Grade, "section", req.Section, "periods", len(*req.Periods))
		respondSchedule(w, req.Grade, req.Section, saved)

	default:
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}
