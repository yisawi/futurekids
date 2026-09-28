package handlers

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"future_kids/internal/auth"

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

// getRequestedDateOrDefault returns the ?date= query param, defaulting to today in Asia/Baghdad.
func getRequestedDateOrDefault(r *http.Request) string {
	if d := r.URL.Query().Get("date"); d != "" {
		return d
	}
	loc, _ := time.LoadLocation("Asia/Baghdad")
	return time.Now().In(loc).Format("2006-01-02")
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

	// جلب الهاش المخزن في قاعدة البيانات
	var storedHash string
	query := `SELECT password_hash FROM admins WHERE username = $1`
	err := app.DB.QueryRowContext(r.Context(), query, req.Username).Scan(&storedHash)

	if err != nil {
		if err == sql.ErrNoRows {
			respondError(w, http.StatusUnauthorized, "بيانات الدخول غير صحيحة")
			return
		}
		respondInternalError(w, "Internal server error", "AdminLoginHandler: query failed", err, "username", req.Username)
		return
	}

	// مقارنة كلمة المرور المدخلة مع الهاش
	err = bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(req.Password))
	if err != nil {
		respondError(w, http.StatusUnauthorized, "بيانات الدخول غير صحيحة")
		return
	}

	// إصدار توكن الإدارة
	tokenString, err := auth.GenerateAdminToken(req.Username)
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

	loc, _ := time.LoadLocation("Asia/Baghdad")
	today := time.Now().In(loc).Format("2006-01-02")

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

// validateStudentPayload trims the required fields in place and returns an error
// message for the first one that is empty, or "" when the payload is valid.
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
	return ""
}

func (app *AppEnv) AdminStudentsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {

	// 1. القراءة (JOIN بين جدول الطلاب والآباء)
	case http.MethodGet:
		query := `
			SELECT 
				s.id, 
				s.full_name, 
				p.full_name as parent_name, 
				p.phone_number,
				COALESCE(s.rfid_tag, ''),
				s.grade,
				s.section
			FROM students s
			JOIN parents p ON s.parent_id = p.id
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
		// Captured before defaults are applied: on parent conflict, only overwrite what the admin actually sent.
		pinProvided, nameProvided := req.ParentPin != "", req.ParentName != ""
		if req.ParentPin == "" {
			req.ParentPin = "1234"
		}
		hashedPin, err := bcrypt.GenerateFromPassword([]byte(req.ParentPin), bcrypt.DefaultCost)
		if err != nil {
			respondInternalError(w, "Internal server error", "AdminStudentsHandler: PIN hashing failed", err, "parent_phone", req.ParentPhone)
			return
		}

		if req.RfidTag == "" {
			req.RfidTag = fmt.Sprintf("admin-%d", time.Now().UnixNano())
		}

		query := `
			WITH upsert_parent AS (
				INSERT INTO parents (full_name, phone_number, pin_code)
				VALUES ($1, $2, $3)
				ON CONFLICT (phone_number) DO UPDATE 
				SET full_name = CASE WHEN $9::boolean THEN EXCLUDED.full_name ELSE parents.full_name END,
				    pin_code = CASE WHEN $8::boolean THEN EXCLUDED.pin_code ELSE parents.pin_code END
				RETURNING id
			)
			INSERT INTO students (full_name, rfid_tag, parent_id, grade, section)
			VALUES ($4, $5, (SELECT id FROM upsert_parent), $6, $7)
			RETURNING id
		`
		if err := app.DB.QueryRowContext(r.Context(), query, req.ParentName, req.ParentPhone, string(hashedPin), req.Name, req.RfidTag, req.Grade, req.Section, pinProvided, nameProvided).Scan(&req.ID); err != nil {
			respondInternalError(w, "Failed to create student and parent", "AdminStudentsHandler: create failed", err, "parent_phone", req.ParentPhone, "rfid_tag", req.RfidTag)
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
		// Captured before defaults are applied: on parent conflict, only overwrite what the admin actually sent.
		pinProvided, nameProvided := req.ParentPin != "", req.ParentName != ""
		if req.ParentPin == "" {
			req.ParentPin = "1234"
		}
		hashedPin, err := bcrypt.GenerateFromPassword([]byte(req.ParentPin), bcrypt.DefaultCost)
		if err != nil {
			respondInternalError(w, "Internal server error", "AdminStudentsHandler: PIN hashing failed", err, "parent_phone", req.ParentPhone)
			return
		}

		query := `
			WITH upsert_parent AS (
				INSERT INTO parents (full_name, phone_number, pin_code)
				VALUES ($1, $2, $3)
				ON CONFLICT (phone_number) DO UPDATE
				SET full_name = CASE WHEN $10::boolean THEN EXCLUDED.full_name ELSE parents.full_name END,
				    pin_code = CASE WHEN $9::boolean THEN EXCLUDED.pin_code ELSE parents.pin_code END
				RETURNING id
			)
			UPDATE students
			SET full_name = $4,
			    rfid_tag = COALESCE(NULLIF($5::text, ''), rfid_tag),
			    parent_id = (SELECT id FROM upsert_parent),
			    grade = COALESCE($6, grade),
			    section = COALESCE($7, section)
			WHERE id = $8
		`
		result, err := app.DB.ExecContext(r.Context(), query, req.ParentName, req.ParentPhone, string(hashedPin), req.Name, req.RfidTag, req.Grade, req.Section, req.ID, pinProvided, nameProvided)
		if err != nil {
			respondInternalError(w, "Failed to update student", "AdminStudentsHandler: update failed", err, "student_id", req.ID)
			return
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			respondInternalError(w, "Internal server error", "AdminStudentsHandler: rows affected failed", err, "student_id", req.ID)
			return
		}
		if rowsAffected == 0 {
			respondError(w, http.StatusNotFound, "Student not found")
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

func (app *AppEnv) AdminCreateLeaveHandler(w http.ResponseWriter, r *http.Request) {
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

	// استخدام ON CONFLICT لتحديث الملاحظات إذا كانت الإجازة مسجلة مسبقاً لنفس اليوم
	query := `
		INSERT INTO student_leaves (student_id, leave_date, notes) 
		VALUES ($1, $2, $3)
		ON CONFLICT (student_id, leave_date) 
		DO UPDATE SET notes = EXCLUDED.notes
		RETURNING id
	`

	var leaveID int
	err := app.DB.QueryRowContext(r.Context(), query, req.StudentID, req.LeaveDate, req.Notes).Scan(&leaveID)

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

	dateParam := getRequestedDateOrDefault(r)

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
		WHERE s.is_active = true
		ORDER BY st.status DESC, s.full_name ASC
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

	dateParam := getRequestedDateOrDefault(r)

	query := `
		SELECT 
			s.id, 
			s.full_name, 
			COALESCE(s.grade, 'غير محدد'), 
			COALESCE(s.section, '-'), 
			p.full_name as parent_name, 
			p.phone_number,
			CASE st.status
				WHEN 'Present' THEN 'حاضر'
				WHEN 'Excused' THEN 'مجاز'
				ELSE 'غائب'
			END as status,
			COALESCE(st.first_check, '') as check_time
		FROM students s
		JOIN parents p ON s.parent_id = p.id
		CROSS JOIN LATERAL get_student_status(s.id, $1::DATE) st
		WHERE s.is_active = true
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
	f.MergeCell(sheet, "A1", "H1")
	f.SetCellValue(sheet, "A1", "وزارة التربية والتعليم")

	f.MergeCell(sheet, "A2", "H2")
	f.SetCellValue(sheet, "A2", "مدرسة الرحمن الابتدائية الأهلية")

	f.MergeCell(sheet, "A3", "H3")
	f.SetCellValue(sheet, "A3", fmt.Sprintf("تقرير الحضور والغياب اليومي الشامل - تاريخ: %s", dateParam))

	// تطبيق التنسيق على الترويسة
	f.SetCellStyle(sheet, "A1", "H3", titleStyle)

	// 4. إعداد ترويسة أعمدة الجدول (في الصف الخامس لترك مسافة)
	headerStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"#E0E0E0"}, Pattern: 1},
	})

	headers := []string{"رقم الطالب", "اسم الطالب", "الصف", "الشعبة", "ولي الأمر", "رقم الهاتف", "الحالة", "وقت البصمة"}
	for i, header := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 5)
		f.SetCellValue(sheet, cell, header)
	}
	f.SetCellStyle(sheet, "A5", "H5", headerStyle)

	// 5. تعبئة البيانات (ابتداءً من الصف السادس)
	rowIndex := 6
	for rows.Next() {
		var id int
		var studentName, grade, section, parentName, phone, status, checkTime string
		if err := rows.Scan(&id, &studentName, &grade, &section, &parentName, &phone, &status, &checkTime); err != nil {
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
		f.SetCellValue(sheet, fmt.Sprintf("H%d", rowIndex), checkTime)
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
		var req DevicePayload
		if !decodeJSONBody(w, r, &req, "Invalid payload or missing SN") {
			return
		}
		if req.SerialNumber == "" {
			respondError(w, http.StatusBadRequest, "Invalid payload or missing SN")
			return
		}

		query := `INSERT INTO devices (serial_number, location_name, is_active) VALUES ($1, $2, $3)`
		_, err := app.DB.ExecContext(r.Context(), query, req.SerialNumber, req.LocationName, req.IsActive)
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
		var req DevicePayload
		if !decodeJSONBody(w, r, &req, "Invalid payload") {
			return
		}
		if req.SerialNumber == "" {
			respondError(w, http.StatusBadRequest, "Invalid payload")
			return
		}

		query := `UPDATE devices SET location_name = $1, is_active = $2 WHERE serial_number = $3`
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
