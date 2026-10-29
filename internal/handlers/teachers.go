package handlers

import (
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"future_kids/internal/phone"
	"future_kids/internal/tz"

	"github.com/jackc/pgx/v5/pgconn"
)

// Teachers are managed by the admin only (there is no teacher app or login). A teacher's rfid_tag
// is the user ID on the ZKTeco device in the staff range 1 to 99; a device punch whose PIN matches
// no student but a teacher is stored in teacher_attendance_logs and is never notified.

// Teacher statuses; status is descriptive only and never blocks storing punches.
const (
	TeacherActive      = "active"
	TeacherOnLeave     = "on_leave"
	TeacherTransferred = "transferred"
	TeacherRetired     = "retired"
)

// Teacher field limits, in characters.
const (
	MaxTeacherName           = 100
	MaxTeacherSpecialization = 100
	MaxTeacherNotes          = 500
)

// Teacher messages.
const (
	TeacherPhoneMessage       = "phone must be an Iraqi mobile number, for example 07XXXXXXXXX or +9647XXXXXXXXX"
	TeacherGenderMessage      = "gender must be male or female"
	TeacherStatusMessage      = "status must be active, on_leave, transferred or retired"
	TeacherRFIDTagMessage     = "rfid_tag must be the teacher's user ID on the device: 1 to 99, digits only, not starting with 0"
	TeacherIDMessage          = "id must be a positive teacher id"
	TeacherNotFoundMessage    = "Teacher not found"
	TeacherPhoneTakenMessage  = "phone is already used by another teacher"
	TeacherTagTakenMessage    = "rfid_tag is already used by another teacher"
	TeacherTagStudentMessage  = "rfid_tag is already used by a student"
	teacherPhoneUniqueIndex   = "teachers_phone_key"
	teacherRFIDTagUniqueIndex = "teachers_rfid_tag_key"
)

var teacherTagRE = regexp.MustCompile(`^[1-9][0-9]?$`)

var teacherStatuses = map[string]bool{TeacherActive: true, TeacherOnLeave: true, TeacherTransferred: true, TeacherRetired: true}

// Teacher is a teacher as the admin API returns it.
type Teacher struct {
	ID              int     `json:"id"`
	FirstName       string  `json:"first_name"`
	FatherName      string  `json:"father_name"`
	GrandfatherName string  `json:"grandfather_name"`
	FullName        string  `json:"full_name"`
	Phone           string  `json:"phone"`
	Gender          string  `json:"gender"`
	Status          string  `json:"status"`
	Specialization  *string `json:"specialization"`
	Notes           *string `json:"notes"`
	RfidTag         *string `json:"rfid_tag"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
}

// TeacherWriteRequest is the body of POST and PUT /api/admin/teachers. PUT replaces the whole
// record: an optional field that is omitted, null or blank is cleared.
type TeacherWriteRequest struct {
	ID              int     `json:"id"`
	FirstName       string  `json:"first_name"`
	FatherName      string  `json:"father_name"`
	GrandfatherName string  `json:"grandfather_name"`
	Phone           string  `json:"phone"`
	Gender          string  `json:"gender"`
	Status          string  `json:"status"`
	Specialization  *string `json:"specialization"`
	Notes           *string `json:"notes"`
	RfidTag         *string `json:"rfid_tag"`
}

// blankToNil trims an optional field; blank becomes NULL.
func blankToNil(s *string) *string {
	if s == nil {
		return nil
	}
	if t := strings.TrimSpace(*s); t != "" {
		return &t
	}
	return nil
}

// validateTeacher trims and normalises req in place and returns the first problem, or "".
// On update, status is required; on create it defaults to active.
func validateTeacher(req *TeacherWriteRequest, update bool) string {
	req.FirstName = strings.TrimSpace(req.FirstName)
	req.FatherName = strings.TrimSpace(req.FatherName)
	req.GrandfatherName = strings.TrimSpace(req.GrandfatherName)
	req.Phone = strings.TrimSpace(req.Phone)
	req.Gender = strings.TrimSpace(req.Gender)
	req.Status = strings.TrimSpace(req.Status)
	req.Specialization, req.Notes, req.RfidTag = blankToNil(req.Specialization), blankToNil(req.Notes), blankToNil(req.RfidTag)
	for _, f := range []struct{ name, value string }{
		{"first_name", req.FirstName}, {"father_name", req.FatherName}, {"grandfather_name", req.GrandfatherName},
		{"phone", req.Phone}, {"gender", req.Gender},
	} {
		if f.value == "" {
			return f.name + " is required"
		}
	}
	if update && req.Status == "" {
		return "status is required"
	}
	if req.Status == "" {
		req.Status = TeacherActive
	}
	canonical, ok := phone.Normalize(req.Phone)
	if !ok {
		return TeacherPhoneMessage
	}
	req.Phone = canonical
	if req.Gender != "male" && req.Gender != "female" {
		return TeacherGenderMessage
	}
	if !teacherStatuses[req.Status] {
		return TeacherStatusMessage
	}
	optional := func(field string, s *string, max int) string {
		if s == nil {
			return ""
		}
		return tooLong(field, *s, max)
	}
	if msg := firstProblem(
		tooLong("first_name", req.FirstName, MaxTeacherName),
		tooLong("father_name", req.FatherName, MaxTeacherName),
		tooLong("grandfather_name", req.GrandfatherName, MaxTeacherName),
		optional("specialization", req.Specialization, MaxTeacherSpecialization),
		optional("notes", req.Notes, MaxTeacherNotes),
	); msg != "" {
		return msg
	}
	if req.RfidTag != nil && !teacherTagRE.MatchString(*req.RfidTag) {
		return TeacherRFIDTagMessage
	}
	return ""
}

const teacherColumns = `id, first_name, father_name, grandfather_name, phone, gender, status, specialization, notes, rfid_tag, created_at, updated_at`

func scanTeacher(row interface{ Scan(...any) error }) (Teacher, error) {
	var t Teacher
	var created, updated time.Time
	if err := row.Scan(&t.ID, &t.FirstName, &t.FatherName, &t.GrandfatherName, &t.Phone, &t.Gender, &t.Status,
		&t.Specialization, &t.Notes, &t.RfidTag, &created, &updated); err != nil {
		return t, err
	}
	t.FullName = teacherFullName(t.FirstName, t.FatherName, t.GrandfatherName)
	t.CreatedAt = created.In(tz.Baghdad).Format(time.RFC3339)
	t.UpdatedAt = updated.In(tz.Baghdad).Format(time.RFC3339)
	return t, nil
}

func teacherFullName(first, father, grandfather string) string {
	return first + " " + father + " " + grandfather
}

var errTeacherTagStudent = errors.New("rfid_tag held by a student")
var errTeacherNotFound = errors.New("teacher not found")

// writeTeacher creates (id 0) or replaces a teacher in one transaction, refusing a tag a student
// holds. New student tags are 100 or more, so only an older student under 100 can hold one.
func (app *AppEnv) writeTeacher(r *http.Request, req TeacherWriteRequest) (Teacher, error) {
	tx, err := app.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return Teacher{}, err
	}
	defer tx.Rollback()
	if req.RfidTag != nil {
		var held bool
		if err := tx.QueryRowContext(r.Context(), `SELECT EXISTS (SELECT 1 FROM students WHERE rfid_tag = $1)`, *req.RfidTag).Scan(&held); err != nil {
			return Teacher{}, err
		}
		if held {
			return Teacher{}, errTeacherTagStudent
		}
	}
	args := []any{req.FirstName, req.FatherName, req.GrandfatherName, req.Phone, req.Gender, req.Status, req.Specialization, req.Notes, req.RfidTag}
	var row *sql.Row
	if req.ID == 0 {
		row = tx.QueryRowContext(r.Context(), `
			INSERT INTO teachers (first_name, father_name, grandfather_name, phone, gender, status, specialization, notes, rfid_tag)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING `+teacherColumns, args...)
	} else {
		row = tx.QueryRowContext(r.Context(), `
			UPDATE teachers SET first_name = $1, father_name = $2, grandfather_name = $3, phone = $4, gender = $5,
				status = $6, specialization = $7, notes = $8, rfid_tag = $9, updated_at = now()
			WHERE id = $10
			RETURNING `+teacherColumns, append(args, req.ID)...)
	}
	t, err := scanTeacher(row)
	if err == sql.ErrNoRows {
		return Teacher{}, errTeacherNotFound
	}
	if err != nil {
		return Teacher{}, err
	}
	return t, tx.Commit()
}

// respondTeacherWriteError answers a failed teacher create or update.
func respondTeacherWriteError(w http.ResponseWriter, op string, err error, id int) {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, errTeacherNotFound):
		respondError(w, http.StatusNotFound, TeacherNotFoundMessage)
	case errors.Is(err, errTeacherTagStudent):
		respondError(w, http.StatusConflict, TeacherTagStudentMessage)
	case errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == teacherPhoneUniqueIndex:
		respondError(w, http.StatusConflict, TeacherPhoneTakenMessage)
	case errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == teacherRFIDTagUniqueIndex:
		respondError(w, http.StatusConflict, TeacherTagTakenMessage)
	default:
		respondInternalError(w, "Internal server error", op, err, "teacher_id", id)
	}
}

// AdminTeachersHandler serves /api/admin/teachers: GET lists (?status=, ?search=, or ?id= for one
// teacher, in the same list shape), POST creates and PUT replaces a teacher (id in the body).
func (app *AppEnv) AdminTeachersHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		app.listTeachers(w, r)
	case http.MethodPost, http.MethodPut:
		update := r.Method == http.MethodPut
		var req TeacherWriteRequest
		if !decodeJSONBody(w, r, &req, "Invalid request body") {
			return
		}
		if update {
			if req.ID < 1 {
				respondError(w, http.StatusBadRequest, TeacherIDMessage)
				return
			}
			NoteActivity(r, "teacher", strconv.Itoa(req.ID), "")
		} else {
			req.ID = 0
		}
		if msg := validateTeacher(&req, update); msg != "" {
			respondError(w, http.StatusBadRequest, msg)
			return
		}
		t, err := app.writeTeacher(r, req)
		if err != nil {
			respondTeacherWriteError(w, "AdminTeachersHandler: write failed", err, req.ID)
			return
		}
		message := "Teacher created"
		if update {
			message = "Teacher updated"
		} else {
			NoteActivity(r, "teacher", strconv.Itoa(t.ID), "")
		}
		respondJSON(w, http.StatusOK, map[string]any{"status": "success", "message": message, "data": t})
	default:
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// likePattern escapes s for a LIKE/ILIKE pattern and wraps it in %…%.
func likePattern(s string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s) + "%"
}

// searchDigits returns the digits of s (ASCII, Arabic-Indic or Persian) without a leading 00964,
// 964 or 0, so "0770 123" matches the stored +964770123…; "" when s has fewer than 3 digits.
func searchDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= '٠' && r <= '٩':
			b.WriteRune('0' + r - '٠')
		case r >= '۰' && r <= '۹':
			b.WriteRune('0' + r - '۰')
		}
	}
	d := b.String()
	for _, prefix := range []string{"00964", "964", "0"} {
		if strings.HasPrefix(d, prefix) {
			d = d[len(prefix):]
			break
		}
	}
	if len(d) < 3 {
		return ""
	}
	return d
}

func (app *AppEnv) listTeachers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var status, id, name, digits any
	if s := strings.TrimSpace(q.Get("status")); s != "" {
		if !teacherStatuses[s] {
			respondError(w, http.StatusBadRequest, TeacherStatusMessage)
			return
		}
		status = s
	}
	if v := q.Get("id"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			respondError(w, http.StatusBadRequest, TeacherIDMessage)
			return
		}
		id = n
	}
	if s := strings.TrimSpace(q.Get("search")); s != "" {
		name = likePattern(s)
		if d := searchDigits(s); d != "" {
			digits = likePattern(d)
		}
	}
	rows, err := app.DB.QueryContext(r.Context(), `
		SELECT `+teacherColumns+` FROM teachers
		WHERE ($1::text IS NULL OR status = $1)
		  AND ($2::int IS NULL OR id = $2)
		  AND ($3::text IS NULL OR first_name || ' ' || father_name || ' ' || grandfather_name ILIKE $3 OR phone LIKE $4::text)
		ORDER BY first_name, father_name, grandfather_name, id`, status, id, name, digits)
	if err != nil {
		respondInternalError(w, "Database error", "AdminTeachersHandler: list failed", err)
		return
	}
	defer rows.Close()
	teachers := []Teacher{}
	for rows.Next() {
		t, err := scanTeacher(rows)
		if err != nil {
			respondInternalError(w, "Database error", "AdminTeachersHandler: scan failed", err)
			return
		}
		teachers = append(teachers, t)
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "AdminTeachersHandler: rows iteration failed", err)
		return
	}
	if id != nil && len(teachers) == 0 {
		respondError(w, http.StatusNotFound, TeacherNotFoundMessage)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"status": "success", "data": teachers})
}

// TeacherAttendanceItem is one teacher's punches on a date.
type TeacherAttendanceItem struct {
	TeacherID  int     `json:"teacher_id"`
	FullName   string  `json:"full_name"`
	Status     string  `json:"status"`
	FirstPunch *string `json:"first_punch"`
	LastPunch  *string `json:"last_punch"`
	PunchCount int     `json:"punch_count"`
	NotPunched bool    `json:"not_punched"`
}

// AdminTeachersAttendanceHandler (GET /api/admin/teachers/attendance?date=) lists, for a date
// (default today in Asia/Baghdad), every active teacher who existed by then and every teacher
// with a punch that day: the earliest and latest punch and their count. not_punched marks an
// active teacher without a punch on a school day that is today or earlier. There are no lateness
// or absence rules.
func (app *AppEnv) AdminTeachersAttendanceHandler(w http.ResponseWriter, r *http.Request) {
	date, ok := requestedDate(w, r)
	if !ok {
		return
	}
	day, err := DayOn(r.Context(), app.DB, date)
	if err != nil {
		respondInternalError(w, "Database error", "AdminTeachersAttendanceHandler: day lookup failed", err, "date", date)
		return
	}
	rows, err := app.DB.QueryContext(r.Context(), `
		WITH p AS (
			SELECT teacher_id, MIN(check_time) AS first_at, MAX(check_time) AS last_at, COUNT(*) AS n
			FROM teacher_attendance_logs
			WHERE check_time >= $1::date AND check_time < $1::date + 1
			GROUP BY teacher_id
		)
		SELECT t.id, t.first_name, t.father_name, t.grandfather_name, t.status,
		       TO_CHAR(p.first_at, 'HH12:MI AM'), CASE WHEN p.n > 1 THEN TO_CHAR(p.last_at, 'HH12:MI AM') END, COALESCE(p.n, 0)
		FROM teachers t
		LEFT JOIN p ON p.teacher_id = t.id
		WHERE p.teacher_id IS NOT NULL
		   OR (t.status = 'active' AND (t.created_at AT TIME ZONE 'Asia/Baghdad')::date <= $1::date)
		ORDER BY p.first_at NULLS LAST, t.first_name, t.father_name, t.grandfather_name, t.id`, date)
	if err != nil {
		respondInternalError(w, "Database error", "AdminTeachersAttendanceHandler: query failed", err, "date", date)
		return
	}
	defer rows.Close()
	// A future date can have no missed punches yet.
	canMiss := day.School() && date <= today()
	items := []TeacherAttendanceItem{}
	for rows.Next() {
		var it TeacherAttendanceItem
		var first, father, grandfather string
		if err := rows.Scan(&it.TeacherID, &first, &father, &grandfather, &it.Status, &it.FirstPunch, &it.LastPunch, &it.PunchCount); err != nil {
			respondInternalError(w, "Database error", "AdminTeachersAttendanceHandler: scan failed", err, "date", date)
			return
		}
		it.FullName = teacherFullName(first, father, grandfather)
		it.NotPunched = canMiss && it.Status == TeacherActive && it.PunchCount == 0
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "AdminTeachersAttendanceHandler: rows iteration failed", err, "date", date)
		return
	}
	respondJSON(w, http.StatusOK, withDay(map[string]any{"status": "success", "date": date, "school_day": day.School(), "data": items}, day))
}

// AdminTeacherSpecializationsHandler (GET /api/admin/teachers/specializations) returns the
// distinct non-empty subject names of the weekly schedules and teacher specializations, sorted,
// so the dashboard can suggest matching names.
func (app *AppEnv) AdminTeacherSpecializationsHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := app.DB.QueryContext(r.Context(), `
		SELECT v FROM (
			SELECT btrim(subject_name) AS v FROM weekly_schedules
			UNION
			SELECT btrim(specialization) FROM teachers WHERE specialization IS NOT NULL
		) s
		WHERE v <> ''
		ORDER BY v`)
	if err != nil {
		respondInternalError(w, "Database error", "AdminTeacherSpecializationsHandler: query failed", err)
		return
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			respondInternalError(w, "Database error", "AdminTeacherSpecializationsHandler: scan failed", err)
			return
		}
		names = append(names, v)
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "AdminTeacherSpecializationsHandler: rows iteration failed", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"status": "success", "data": names})
}
