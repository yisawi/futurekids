package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"future_kids/internal/notify"
	"future_kids/internal/tz"

	"github.com/jackc/pgx/v5/pgconn"
)

// Manual attendance: the default times of a manual check-in and check-out, the sources of an
// attendance_logs row, and the longest note, counted in characters as PostgreSQL counts them.
const (
	ManualCheckInTime  = "08:00"
	ManualCheckOutTime = "13:00"
	SourceDevice       = "device"
	SourceManual       = "manual"
	MaxManualNoteChars = 200
)

// Manual punch types.
const (
	ManualTypeIn  = "in"
	ManualTypeOut = "out"
)

var manualTimeRE = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9](:[0-5][0-9])?$`)

type manualAttendanceRequest struct {
	StudentID json.RawMessage `json:"student_id"`
	Date      json.RawMessage `json:"date"`
	Type      json.RawMessage `json:"type"`
	Time      json.RawMessage `json:"time"`
	Notify    json.RawMessage `json:"notify"`
	Note      json.RawMessage `json:"note"`
	DryRun    json.RawMessage `json:"dry_run"`
}

// ManualAttendanceItem is a manual punch created (or, with dry_run, previewed) for a student.
type ManualAttendanceItem struct {
	ID          *int64  `json:"id"`
	StudentID   int     `json:"student_id"`
	StudentName string  `json:"student_name"`
	Grade       *string `json:"grade"`
	Section     *string `json:"section"`
	Date        string  `json:"date"`
	Type        string  `json:"type"`
	Time        string  `json:"time"`
	Source      string  `json:"source"`
	Note        *string `json:"note"`
	Notified    bool    `json:"notified"`
	DryRun      bool    `json:"dry_run"`
}

// ManualAttendanceDeleteItem is a manual punch that was deleted.
type ManualAttendanceDeleteItem struct {
	ID          int64   `json:"id"`
	StudentID   int     `json:"student_id"`
	StudentName string  `json:"student_name"`
	Grade       *string `json:"grade"`
	Section     *string `json:"section"`
	Date        string  `json:"date"`
	Type        string  `json:"type"`
	Time        string  `json:"time"`
}

// manualEntry is a validated create request.
type manualEntry struct {
	studentID int
	date      time.Time
	typ       string
	clock     time.Duration
	notify    bool
	note      *string
	dryRun    bool
}

func (e manualEntry) checkTime() string {
	return ymd(e.date) + " " + clockText(e.clock)
}

func (e manualEntry) detail() string {
	return dryRunDetail(e.dryRun, "type="+e.typ, "date="+ymd(e.date), "notify="+strconv.FormatBool(e.notify))
}

func clockText(d time.Duration) string {
	s := int(d / time.Second)
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, s/60%60, s%60)
}

// manualWindow returns the window of a punch type as the status function defines it.
func manualWindow(typ string) (start, end time.Duration, sqlFmt, name string) {
	if typ == ManualTypeOut {
		return CheckOutWindowStart, CheckOutWindowEnd, checkOutWindowSQL, "check-out"
	}
	return CheckInWindowStart, CheckInWindowEnd, checkInWindowSQL, "check-in"
}

// manualDates returns today and yesterday (Asia/Baghdad) as dates.
func manualDates(now time.Time) (today, yesterday time.Time) {
	today, _ = time.Parse("2006-01-02", now.Format("2006-01-02"))
	return today, today.AddDate(0, 0, -1)
}

func parseManualEntry(req *manualAttendanceRequest, now time.Time) (manualEntry, string) {
	var e manualEntry
	if absent(req.StudentID) {
		return e, "student_id is required"
	}
	if err := json.Unmarshal(req.StudentID, &e.studentID); err != nil || e.studentID < 1 {
		e.studentID = 0
		return e, "student_id must be a positive student id"
	}
	today, yesterday := manualDates(now)
	date, msg := jsonDate("date", req.Date)
	if msg != "" {
		return e, msg
	}
	e.date = today
	if date != nil {
		e.date = *date
	}
	if !e.date.Equal(today) && !e.date.Equal(yesterday) {
		return e, fmt.Sprintf("date must be today (%s) or yesterday (%s)", ymd(today), ymd(yesterday))
	}
	if absent(req.Type) {
		return e, "type is required"
	}
	if err := json.Unmarshal(req.Type, &e.typ); err != nil || (e.typ != ManualTypeIn && e.typ != ManualTypeOut) {
		e.typ = ""
		return e, "type must be in or out"
	}
	clock := ManualCheckInTime
	if e.typ == ManualTypeOut {
		clock = ManualCheckOutTime
	}
	if !absent(req.Time) {
		if err := json.Unmarshal(req.Time, &clock); err != nil || !manualTimeRE.MatchString(clock) {
			return e, "time must be formatted as HH:MM or HH:MM:SS"
		}
	}
	if len(clock) == 5 {
		clock += ":00"
	}
	t, _ := time.Parse("15:04:05", clock)
	e.clock = time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute + time.Duration(t.Second())*time.Second
	start, end, _, name := manualWindow(e.typ)
	if e.clock < start || e.clock >= end {
		return e, fmt.Sprintf("time must be in the %s window %s to %s", name, clockText(start), clockText(end-time.Second))
	}
	if e.notify, msg = jsonBool("notify", req.Notify, true); msg != "" {
		return e, msg
	}
	if e.note, msg = optionalText("note", req.Note, MaxManualNoteChars); msg != "" {
		return e, msg
	}
	if e.dryRun, msg = jsonBool("dry_run", req.DryRun, false); msg != "" {
		return e, msg
	}
	if e.date.Equal(today) {
		at := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, tz.Baghdad).Add(e.clock)
		if at.After(now) {
			return e, fmt.Sprintf("time must not be later than now (%s) for today", now.Format("15:04:05"))
		}
	}
	return e, ""
}

// AdminManualAttendanceHandler serves POST (create) and DELETE (undo) /api/admin/attendance/manual.
func (app *AppEnv) AdminManualAttendanceHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		app.createManualAttendance(w, r)
	case http.MethodDelete:
		app.deleteManualAttendance(w, r)
	default:
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

type manualStudent struct {
	name     string
	grade    *string
	section  *string
	parentID sql.NullInt64
	existed  bool
}

var manualStudentSQL = `
	SELECT s.full_name, s.grade, s.section, s.parent_id, ` + fmt.Sprintf(studentExistedOnSQL, "$2::date") + `
	FROM students s
	WHERE s.id = $1 AND s.is_active = true`

// sameKindPunch returns the 409 message naming the student's first punch in the window of e, from
// any source, or "".
func sameKindPunch(ctx context.Context, q rowQueryer, e manualEntry) (string, error) {
	_, _, sqlFmt, name := manualWindow(e.typ)
	var at, source string
	err := q.QueryRowContext(ctx, `
		SELECT TO_CHAR(a.check_time, 'HH12:MI AM'), a.source FROM attendance_logs a
		WHERE a.student_id = $1 AND `+fmt.Sprintf(sqlFmt, "a.check_time", "$2::date")+`
		ORDER BY a.check_time LIMIT 1`, e.studentID, ymd(e.date)).Scan(&at, &source)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Student already has a %s at %s on %s (%s)", name, at, ymd(e.date), source), nil
}

func (app *AppEnv) createManualAttendance(w http.ResponseWriter, r *http.Request) {
	var req manualAttendanceRequest
	if !decodeJSONBody(w, r, &req, "Invalid request body") {
		return
	}
	e, msg := parseManualEntry(&req, Clock())
	if msg != "" {
		if e.studentID > 0 {
			NoteActivity(r, "student", strconv.Itoa(e.studentID), "")
		}
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	NoteActivity(r, "student", strconv.Itoa(e.studentID), e.detail())
	ctx := r.Context()
	date := ymd(e.date)
	day, err := DayOn(ctx, app.DB, date)
	if err != nil {
		respondInternalError(w, "Failed to record attendance", "AdminManualAttendanceHandler: day lookup failed", err, "date", date)
		return
	}
	if !day.School() {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("%s is not a school day (%s)", date, day.Type))
		return
	}

	item := ManualAttendanceItem{StudentID: e.studentID, Date: date, Type: e.typ, Time: clockText(e.clock), Source: SourceManual, Note: e.note, DryRun: e.dryRun}
	status, problem := http.StatusOK, ""
	var tokens []string
	var title, body string
	check := func(q rowQueryer, lock string) error {
		var st manualStudent
		err := q.QueryRowContext(ctx, manualStudentSQL+lock, e.studentID, date).Scan(&st.name, &st.grade, &st.section, &st.parentID, &st.existed)
		if err == sql.ErrNoRows {
			status, problem = http.StatusNotFound, "Student not found"
			return nil
		}
		if err != nil {
			return err
		}
		item.StudentName, item.Grade, item.Section = st.name, st.grade, st.section
		if !st.existed {
			status, problem = http.StatusBadRequest, fmt.Sprintf("The student was not registered yet on %s", date)
			return nil
		}
		if problem, err = sameKindPunch(ctx, q, e); err != nil {
			return err
		}
		if problem != "" {
			status = http.StatusConflict
		}
		item.Notified = e.notify && st.parentID.Valid
		return nil
	}

	if e.dryRun {
		if err := check(app.DB, ""); err != nil {
			respondInternalError(w, "Failed to record attendance", "AdminManualAttendanceHandler: preview failed", err, "student_id", e.studentID)
			return
		}
		if problem != "" {
			respondError(w, status, problem)
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"status": "success", "message": "Preview only", "data": item})
		return
	}

	err = func() error {
		tx, err := app.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := check(tx, " FOR NO KEY UPDATE OF s"); err != nil || problem != "" {
			return err
		}
		var id int64
		var checkTime time.Time
		err = tx.QueryRowContext(ctx, `
			INSERT INTO attendance_logs (student_id, check_time, source, note)
			VALUES ($1, $2::timestamp, 'manual', $3)
			RETURNING id, check_time`, e.studentID, e.checkTime(), e.note).Scan(&id, &checkTime)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			status, problem = http.StatusConflict, fmt.Sprintf("Student already has a punch at %s on %s", clockText(e.clock), date)
			return nil
		}
		if err != nil {
			return err
		}
		item.ID = &id
		item.Notified = false
		if e.notify {
			if tokens, title, body, item.Notified, err = manualNotification(ctx, tx, id, e, checkTime); err != nil {
				return err
			}
		}
		return tx.Commit()
	}()
	if err != nil {
		respondInternalError(w, "Failed to record attendance", "AdminManualAttendanceHandler: create failed", err, "student_id", e.studentID, "date", date, "type", e.typ)
		return
	}
	if problem != "" {
		respondError(w, status, problem)
		return
	}
	slog.Info("Manual attendance recorded", "id", *item.ID, "student_id", e.studentID, "date", date, "type", e.typ, "time", item.Time, "notified", item.Notified)
	if item.Notified {
		notify.SendToDevices(app.Background, app.FCMClient, app.DB, tokens, title, body)
	}
	respondJSON(w, http.StatusOK, map[string]any{"status": "success", "message": "Manual attendance recorded", "data": item})
}

// manualNotification decides with the punch notification query (notifyPunchesSQL) whether the
// new row is notified, and stores the parent's notification inside tx. It returns the parent's
// device tokens and the text to push after the commit.
func manualNotification(ctx context.Context, tx *sql.Tx, id int64, e manualEntry, checkTime time.Time) ([]string, string, string, bool, error) {
	rows, err := tx.QueryContext(ctx, notifyPunchesSQL, []int{e.studentID}, []time.Time{checkTime})
	if err != nil {
		return nil, "", "", false, err
	}
	var studentID int
	var at time.Time
	var name string
	var deviceTokens, parentPhone sql.NullString
	var parentID sql.NullInt64
	var isCheckOut, found bool
	for rows.Next() {
		if err := rows.Scan(&studentID, &at, &name, &deviceTokens, &parentID, &parentPhone, &isCheckOut); err != nil {
			rows.Close()
			return nil, "", "", false, err
		}
		found = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", "", false, err
	}
	if !found || !parentID.Valid {
		return nil, "", "", false, nil
	}
	if isCheckOut != (e.typ == ManualTypeOut) {
		return nil, "", "", false, fmt.Errorf("manual punch %d classified as check-out=%v, requested %s", id, isCheckOut, e.typ)
	}
	title, body := PunchNotificationText(name, at, isCheckOut)
	if err := notify.SaveNotificationTx(ctx, tx, int(parentID.Int64), title, body); err != nil {
		return nil, "", "", false, err
	}
	return notify.DeviceTokens(deviceTokens), title, body, true, nil
}

func (app *AppEnv) deleteManualAttendance(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || id < 1 {
		respondError(w, http.StatusBadRequest, "id must be a positive attendance entry id")
		return
	}
	NoteActivity(r, "", "", "id="+strconv.FormatInt(id, 10))
	ctx := r.Context()
	today, yesterday := manualDates(Clock())
	item := ManualAttendanceDeleteItem{ID: id}
	status, problem := http.StatusOK, ""
	err = func() error {
		tx, err := app.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var source string
		var checkTime time.Time
		var checkIn bool
		err = tx.QueryRowContext(ctx, `
			SELECT a.student_id, a.source, a.check_time, `+fmt.Sprintf(checkInWindowSQL, "a.check_time", "a.check_time::date")+`,
			       s.full_name, s.grade, s.section
			FROM attendance_logs a
			JOIN students s ON s.id = a.student_id
			WHERE a.id = $1
			FOR UPDATE OF a`, id).Scan(&item.StudentID, &source, &checkTime, &checkIn, &item.StudentName, &item.Grade, &item.Section)
		if err == sql.ErrNoRows {
			status, problem = http.StatusNotFound, "Attendance entry not found"
			return nil
		}
		if err != nil {
			return err
		}
		item.Date, item.Time, item.Type = checkTime.Format("2006-01-02"), checkTime.Format("15:04:05"), ManualTypeOut
		if checkIn {
			item.Type = ManualTypeIn
		}
		NoteActivity(r, "student", strconv.Itoa(item.StudentID), fmt.Sprintf("id=%d type=%s date=%s", id, item.Type, item.Date))
		if source != SourceManual {
			status, problem = http.StatusConflict, "Only manual entries can be deleted; this punch came from a device"
			return nil
		}
		if item.Date != ymd(today) && item.Date != ymd(yesterday) {
			status, problem = http.StatusConflict, fmt.Sprintf("Only manual entries from today (%s) or yesterday (%s) can be deleted", ymd(today), ymd(yesterday))
			return nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM attendance_logs WHERE id = $1`, id); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		respondInternalError(w, "Failed to delete attendance entry", "AdminManualAttendanceHandler: delete failed", err, "id", id)
		return
	}
	if problem != "" {
		respondError(w, status, problem)
		return
	}
	slog.Info("Manual attendance deleted", "id", id, "student_id", item.StudentID, "date", item.Date, "type", item.Type)
	respondJSON(w, http.StatusOK, map[string]any{"status": "success", "message": "Manual attendance deleted", "data": item})
}
