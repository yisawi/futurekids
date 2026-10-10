package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"future_kids/internal/notify"
	"future_kids/internal/tz"
)

// Closure kinds and limits, counted in characters as PostgreSQL counts them.
const (
	ClosureHoliday       = "holiday"
	ClosurePause         = "pause"
	MaxClosureTitleChars = 80
	MaxClosureNotesChars = 500
	MaxHolidayDays       = 60
)

// WeekendLabel names a Friday or Saturday in the daily Excel export.
const WeekendLabel = "عطلة نهاية الأسبوع"

type closureRequest struct {
	Kind      json.RawMessage `json:"kind"`
	StartDate json.RawMessage `json:"start_date"`
	EndDate   json.RawMessage `json:"end_date"`
	Title     json.RawMessage `json:"title"`
	Notes     json.RawMessage `json:"notes"`
	Notify    json.RawMessage `json:"notify"`
	Confirm   json.RawMessage `json:"confirm"`
	DryRun    json.RawMessage `json:"dry_run"`
}

type closureCancelRequest struct {
	ID     json.RawMessage `json:"id"`
	Notify json.RawMessage `json:"notify"`
	Notes  json.RawMessage `json:"notes"`
	DryRun json.RawMessage `json:"dry_run"`
}

// ClosureItem is one closure in the admin list.
type ClosureItem struct {
	ID             int     `json:"id"`
	Kind           string  `json:"kind"`
	StartDate      string  `json:"start_date"`
	EndDate        *string `json:"end_date"`
	Title          string  `json:"title"`
	Notes          *string `json:"notes"`
	CreatedAt      string  `json:"created_at"`
	Active         bool    `json:"active"`
	Notified       bool    `json:"notified"`
	RecipientCount *int    `json:"recipient_count"`
}

// ClosedDay is one closed Sunday-to-Thursday date in the parent's month.
type ClosedDay struct {
	Date  string  `json:"date"`
	Kind  string  `json:"kind"`
	Title string  `json:"title"`
	Notes *string `json:"notes"`
}

func absent(raw json.RawMessage) bool {
	return len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

func jsonBool(name string, raw json.RawMessage, def bool) (bool, string) {
	switch string(bytes.TrimSpace(raw)) {
	case "", "null":
		return def, ""
	case "true":
		return true, ""
	case "false":
		return false, ""
	}
	return false, name + " must be true or false"
}

func jsonDate(name string, raw json.RawMessage) (*time.Time, string) {
	if absent(raw) {
		return nil, ""
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, name + " must be formatted as YYYY-MM-DD"
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil || t.Format("2006-01-02") != v {
		return nil, name + " must be formatted as YYYY-MM-DD"
	}
	if !yearInRange(t) {
		return nil, name + " must have a year from 2000 to 2100"
	}
	return &t, ""
}

func optionalText(name string, raw json.RawMessage, max int) (*string, string) {
	if absent(raw) {
		return nil, ""
	}
	if !utf8.Valid(raw) {
		return nil, name + " must be text"
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, name + " must be a string"
	}
	if strings.ContainsRune(v, 0) {
		return nil, name + " must be text"
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, ""
	}
	if msg := tooLong(name, v, max); msg != "" {
		return nil, msg
	}
	return &v, ""
}

func ymd(t time.Time) string { return t.Format("2006-01-02") }

func ymdPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := ymd(*t)
	return &s
}

func weekendDate(t time.Time) bool { return t.Weekday() == time.Friday || t.Weekday() == time.Saturday }

func schoolDaysBetween(start, end time.Time) int {
	n := 0
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if !weekendDate(d) {
			n++
		}
	}
	return n
}

// nextSchoolWeekday returns the first Sunday-to-Thursday date on or after t.
func nextSchoolWeekday(t time.Time) time.Time {
	for weekendDate(t) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

func truncateChars(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return string(r[:n-1]) + "…"
}

// closure is a validated create request.
type closure struct {
	kind    string
	start   time.Time
	end     *time.Time
	title   string
	notes   *string
	notify  bool
	confirm bool
	dryRun  bool
}

func parseClosure(req *closureRequest, today time.Time) (closure, string) {
	var c closure
	if absent(req.Kind) {
		return c, "kind is required"
	}
	if err := json.Unmarshal(req.Kind, &c.kind); err != nil || (c.kind != ClosureHoliday && c.kind != ClosurePause) {
		return c, "kind must be holiday or pause"
	}
	if absent(req.StartDate) {
		return c, "start_date is required"
	}
	start, msg := jsonDate("start_date", req.StartDate)
	if msg != "" {
		return c, msg
	}
	c.start = *start
	if c.end, msg = jsonDate("end_date", req.EndDate); msg != "" {
		return c, msg
	}
	if c.title, msg = broadcastText("title", req.Title, MaxClosureTitleChars); msg != "" {
		return c, msg
	}
	if c.notes, msg = optionalText("notes", req.Notes, MaxClosureNotesChars); msg != "" {
		return c, msg
	}
	if c.notify, msg = jsonBool("notify", req.Notify, true); msg != "" {
		return c, msg
	}
	if c.confirm, msg = jsonBool("confirm", req.Confirm, false); msg != "" {
		return c, msg
	}
	if c.dryRun, msg = jsonBool("dry_run", req.DryRun, false); msg != "" {
		return c, msg
	}
	if c.end != nil && c.end.Before(c.start) {
		return c, "end_date must not be before start_date"
	}
	if c.kind == ClosureHoliday {
		if c.end == nil {
			end := c.start
			c.end = &end
		}
		if c.end.Sub(c.start) >= MaxHolidayDays*24*time.Hour {
			return c, fmt.Sprintf("a holiday can be at most %d days; use a pause for a longer closure", MaxHolidayDays)
		}
		return c, ""
	}
	if !c.confirm && !c.dryRun {
		return c, "confirm must be true for a pause"
	}
	if c.start.Before(today) {
		return c, "a pause cannot start in the past"
	}
	return c, ""
}

// body is the notification text of a new closure: its notes, or a fixed Arabic sentence.
func (c closure) body() string {
	if c.notes != nil {
		return *c.notes
	}
	switch {
	case c.kind == ClosurePause && c.end == nil:
		return "تعطيل الدوام اعتباراً من " + ymd(c.start)
	case c.kind == ClosurePause:
		return "تعطيل الدوام اعتباراً من " + ymd(c.start) + " حتى " + ymd(*c.end)
	case c.end.Equal(c.start):
		return "عطلة رسمية يوم " + ymd(c.start)
	default:
		return "عطلة رسمية من " + ymd(c.start) + " إلى " + ymd(*c.end)
	}
}

func lockClosures(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `LOCK TABLE school_closures IN SHARE ROW EXCLUSIVE MODE`)
	return err
}

// overlap returns the 409 message naming the first closure that shares a date with start..end
// (end nil means open-ended), or "".
func overlap(ctx context.Context, q rowQueryer, start time.Time, end *time.Time) (string, error) {
	var id int
	var title, from string
	var to sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT id, title, TO_CHAR(start_date, 'YYYY-MM-DD'), TO_CHAR(end_date, 'YYYY-MM-DD')
		FROM school_closures
		WHERE start_date <= COALESCE($2::date, 'infinity'::date)
		  AND COALESCE(end_date, 'infinity'::date) >= $1::date
		ORDER BY start_date, id
		LIMIT 1`, ymd(start), ymdPtr(end)).Scan(&id, &title, &from, &to)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	span := "from " + from + ", open-ended"
	if to.Valid {
		span = from + " to " + to.String
	}
	return fmt.Sprintf("Overlaps closure %d %q (%s); cancel it first or choose other dates", id, title, span), nil
}

// closureBroadcast sends title and body to every parent inside tx through the broadcast engine.
// With no parents it writes nothing and returns id 0.
func closureBroadcast(ctx context.Context, tx *sql.Tx, title, body string) (int, int, error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT closure_broadcast`); err != nil {
		return 0, 0, err
	}
	id, recipients, err := notify.CreateBroadcast(ctx, tx, title, body, notify.Audience{Type: notify.AudienceAll})
	if errors.Is(err, notify.ErrNoRecipients) {
		_, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT closure_broadcast`)
		return 0, 0, err
	}
	return id, recipients, err
}

func (app *AppEnv) countAllParents(ctx context.Context) (int, error) {
	return notify.CountRecipients(ctx, app.DB, notify.Audience{Type: notify.AudienceAll})
}

func (app *AppEnv) AdminHolidaysHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		app.createClosure(w, r)
	case http.MethodGet:
		app.listClosures(w, r)
	default:
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (app *AppEnv) createClosure(w http.ResponseWriter, r *http.Request) {
	var req closureRequest
	if !decodeJSONBody(w, r, &req, "Invalid request body") {
		return
	}
	todayT, _ := time.Parse("2006-01-02", today())
	c, msg := parseClosure(&req, todayT)
	if msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	willNotify := c.notify && !(c.end != nil && c.end.Before(todayT))
	var schoolDays *int
	if c.end != nil {
		n := schoolDaysBetween(c.start, *c.end)
		schoolDays = &n
	}
	ctx := r.Context()
	respond := func(id any, notified bool, recipients int, message string) {
		respondJSON(w, http.StatusOK, map[string]any{
			"status":  "success",
			"message": message,
			"data": map[string]any{
				"id": id, "kind": c.kind, "start_date": ymd(c.start), "end_date": ymdPtr(c.end),
				"school_days": schoolDays, "notified": notified, "recipient_count": recipients, "dry_run": c.dryRun,
			},
		})
	}

	if c.dryRun {
		conflict, err := overlap(ctx, app.DB, c.start, c.end)
		if err != nil {
			respondInternalError(w, "Failed to register closure", "AdminHolidaysHandler: overlap check failed", err)
			return
		}
		if conflict != "" {
			respondError(w, http.StatusConflict, conflict)
			return
		}
		recipients := 0
		if willNotify {
			if recipients, err = app.countAllParents(ctx); err != nil {
				respondInternalError(w, "Failed to register closure", "AdminHolidaysHandler: recipient count failed", err)
				return
			}
		}
		respond(nil, recipients > 0, recipients, "Preview only")
		return
	}

	title, body := c.title, c.body()
	var id, broadcastID, recipients int
	var conflict string
	err := func() error {
		tx, err := app.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := lockClosures(ctx, tx); err != nil {
			return err
		}
		if conflict, err = overlap(ctx, tx, c.start, c.end); err != nil || conflict != "" {
			return err
		}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO school_closures (kind, start_date, end_date, title, notes)
			VALUES ($1, $2::date, $3::date, $4, $5)
			RETURNING id`, c.kind, ymd(c.start), ymdPtr(c.end), c.title, c.notes).Scan(&id); err != nil {
			return err
		}
		if willNotify {
			if broadcastID, recipients, err = closureBroadcast(ctx, tx, title, body); err != nil {
				return err
			}
			if broadcastID != 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE school_closures SET broadcast_id = $1 WHERE id = $2`, broadcastID, id); err != nil {
					return err
				}
			}
		}
		return tx.Commit()
	}()
	switch {
	case err != nil:
		respondInternalError(w, "Failed to register closure", "AdminHolidaysHandler: create failed", err, "kind", c.kind, "start_date", ymd(c.start))
		return
	case conflict != "":
		respondError(w, http.StatusConflict, conflict)
		return
	}
	slog.Info("Closure registered", "closure_id", id, "kind", c.kind, "start_date", ymd(c.start), "end_date", ymdPtr(c.end), "broadcast_id", broadcastID, "recipients", recipients)
	if broadcastID != 0 {
		notify.PushBroadcast(app.Background, app.FCMClient, app.DB, broadcastID, title, body)
	}
	respond(id, broadcastID != 0, recipients, "Closure registered")
}

// cancelPlan is what cancelling one closure today does.
type cancelPlan struct {
	action  string
	newEnd  *string
	started bool
	title   string
}

// planCancel applies "cancel from today onward": a closure that has not started is deleted, one
// that is running ends yesterday (deleted when it started today), one already over gives 409.
func planCancel(ctx context.Context, q rowQueryer, id int64, today time.Time, lock bool) (cancelPlan, int, string, error) {
	query := `SELECT title, TO_CHAR(start_date, 'YYYY-MM-DD'), TO_CHAR(end_date, 'YYYY-MM-DD') FROM school_closures WHERE id = $1`
	if lock {
		query += ` FOR UPDATE`
	}
	var p cancelPlan
	var start string
	var end sql.NullString
	switch err := q.QueryRowContext(ctx, query, id).Scan(&p.title, &start, &end); {
	case err == sql.ErrNoRows:
		return p, http.StatusNotFound, "Closure not found", nil
	case err != nil:
		return p, 0, "", err
	}
	t := ymd(today)
	switch {
	case end.Valid && end.String < t:
		return p, http.StatusConflict, "This closure is already over", nil
	case start > t:
		p.action = "deleted"
	case start == t:
		p.action, p.started = "deleted", true
	default:
		y := ymd(today.AddDate(0, 0, -1))
		p.action, p.newEnd, p.started = "shortened", &y, true
	}
	return p, 0, "", nil
}

// message is the notification sent when the closure is cancelled; resume is the first school
// weekday on or after today.
func (p cancelPlan) message(resume time.Time, notes *string) (string, string) {
	title, prefix := "تم إلغاء العطلة", "تم إلغاء: "
	if p.started {
		title, prefix = "تم إنهاء العطلة واستئناف الدوام", "عاد الدوام اعتباراً من "+ymd(resume)+": "
	}
	tail := ""
	if notes != nil {
		tail = "\n\n" + *notes
	}
	room := MaxBroadcastBodyChars - utf8.RuneCountInString(prefix) - utf8.RuneCountInString(tail)
	body := prefix + truncateChars(p.title, max(room, 1)) + tail
	return title, truncateChars(body, MaxBroadcastBodyChars)
}

func (app *AppEnv) AdminCancelHolidayHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	var req closureCancelRequest
	if !decodeJSONBody(w, r, &req, "Invalid request body") {
		return
	}
	if absent(req.ID) {
		respondError(w, http.StatusBadRequest, "id is required")
		return
	}
	var id int64
	if err := json.Unmarshal(req.ID, &id); err != nil || id < 1 || id > 1<<31-1 {
		respondError(w, http.StatusBadRequest, "id must be a positive closure id")
		return
	}
	notifyParents, msg := jsonBool("notify", req.Notify, true)
	if msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	notes, msg := optionalText("notes", req.Notes, MaxClosureNotesChars)
	if msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	dryRun, msg := jsonBool("dry_run", req.DryRun, false)
	if msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	ctx := r.Context()
	todayT, _ := time.Parse("2006-01-02", today())
	respond := func(p cancelPlan, notified bool, recipients int, message string) {
		respondJSON(w, http.StatusOK, map[string]any{
			"status":  "success",
			"message": message,
			"data": map[string]any{
				"id": id, "action": p.action, "end_date": p.newEnd,
				"notified": notified, "recipient_count": recipients, "dry_run": dryRun,
			},
		})
	}

	if dryRun {
		p, status, problem, err := planCancel(ctx, app.DB, id, todayT, false)
		if err != nil {
			respondInternalError(w, "Failed to cancel closure", "AdminCancelHolidayHandler: lookup failed", err, "closure_id", id)
			return
		}
		if status != 0 {
			respondError(w, status, problem)
			return
		}
		recipients := 0
		if notifyParents {
			if recipients, err = app.countAllParents(ctx); err != nil {
				respondInternalError(w, "Failed to cancel closure", "AdminCancelHolidayHandler: recipient count failed", err, "closure_id", id)
				return
			}
		}
		respond(p, recipients > 0, recipients, "Preview only")
		return
	}

	var p cancelPlan
	var status int
	var problem, title, body string
	var broadcastID, recipients int
	err := func() error {
		tx, err := app.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := lockClosures(ctx, tx); err != nil {
			return err
		}
		if p, status, problem, err = planCancel(ctx, tx, id, todayT, true); err != nil || status != 0 {
			return err
		}
		if p.newEnd != nil {
			_, err = tx.ExecContext(ctx, `UPDATE school_closures SET end_date = $1::date WHERE id = $2`, *p.newEnd, id)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM school_closures WHERE id = $1`, id)
		}
		if err != nil {
			return err
		}
		if notifyParents {
			title, body = p.message(nextSchoolWeekday(todayT), notes)
			if broadcastID, recipients, err = closureBroadcast(ctx, tx, title, body); err != nil {
				return err
			}
		}
		return tx.Commit()
	}()
	switch {
	case err != nil:
		respondInternalError(w, "Failed to cancel closure", "AdminCancelHolidayHandler: cancel failed", err, "closure_id", id)
		return
	case status != 0:
		respondError(w, status, problem)
		return
	}
	slog.Info("Closure cancelled", "closure_id", id, "action", p.action, "end_date", p.newEnd, "broadcast_id", broadcastID, "recipients", recipients)
	if broadcastID != 0 {
		notify.PushBroadcast(app.Background, app.FCMClient, app.DB, broadcastID, title, body)
	}
	respond(p, broadcastID != 0, recipients, "Closure cancelled")
}

func (app *AppEnv) listClosures(w http.ResponseWriter, r *http.Request) {
	var month *string
	if r.URL.Query().Get("month") != "" {
		m, ok := requestedMonth(w, r, Clock())
		if !ok {
			return
		}
		month = &m
	}
	t := today()
	rows, err := app.DB.QueryContext(r.Context(), `
		SELECT c.id, c.kind, TO_CHAR(c.start_date, 'YYYY-MM-DD'), TO_CHAR(c.end_date, 'YYYY-MM-DD'),
		       c.title, c.notes, c.created_at AT TIME ZONE 'Asia/Baghdad',
		       c.start_date <= $2::date AND (c.end_date IS NULL OR c.end_date >= $2::date),
		       c.broadcast_id IS NOT NULL, b.recipient_count
		FROM school_closures c
		LEFT JOIN broadcasts b ON b.id = c.broadcast_id
		WHERE CASE WHEN $1::text IS NULL THEN c.end_date IS NULL OR c.end_date >= $2::date
		      ELSE c.start_date <= (DATE($1 || '-01') + INTERVAL '1 month - 1 day')::date
		       AND COALESCE(c.end_date, 'infinity'::date) >= DATE($1 || '-01') END
		ORDER BY c.start_date, c.id`, month, t)
	if err != nil {
		respondInternalError(w, "Database error", "AdminHolidaysHandler: query failed", err)
		return
	}
	defer rows.Close()
	items := []ClosureItem{}
	for rows.Next() {
		var it ClosureItem
		var createdAt time.Time
		var recipients sql.NullInt64
		if err := rows.Scan(&it.ID, &it.Kind, &it.StartDate, &it.EndDate, &it.Title, &it.Notes, &createdAt, &it.Active, &it.Notified, &recipients); err != nil {
			respondInternalError(w, "Database error", "AdminHolidaysHandler: scan failed", err)
			return
		}
		if recipients.Valid {
			n := int(recipients.Int64)
			it.RecipientCount = &n
		}
		it.CreatedAt = createdAt.In(tz.Baghdad).Format(time.RFC3339)
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "AdminHolidaysHandler: rows iteration failed", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"status": "success", "data": items})
}

// MobileHolidaysHandler (GET /api/mobile/holidays?month=) lists the closed Sunday-to-Thursday
// dates of the month, one per date.
func (app *AppEnv) MobileHolidaysHandler(w http.ResponseWriter, r *http.Request) {
	month, ok := requestedMonth(w, r, Clock())
	if !ok {
		return
	}
	rows, err := app.DB.QueryContext(r.Context(), `
		SELECT DISTINCT ON (g.d) TO_CHAR(g.d, 'YYYY-MM-DD'), c.kind, c.title, c.notes
		FROM (SELECT generate_series(DATE($1 || '-01'), (DATE($1 || '-01') + INTERVAL '1 month - 1 day')::date, '1 day'::interval)::date AS d) g
		JOIN school_closures c ON c.start_date <= g.d AND (c.end_date IS NULL OR c.end_date >= g.d)
		WHERE EXTRACT(DOW FROM g.d) NOT IN (5, 6)
		ORDER BY g.d, c.kind = 'pause' DESC, c.start_date`, month)
	if err != nil {
		respondInternalError(w, "Database error", "MobileHolidaysHandler: query failed", err, "month", month)
		return
	}
	defer rows.Close()
	days := []ClosedDay{}
	for rows.Next() {
		var d ClosedDay
		if err := rows.Scan(&d.Date, &d.Kind, &d.Title, &d.Notes); err != nil {
			respondInternalError(w, "Database error", "MobileHolidaysHandler: scan failed", err, "month", month)
			return
		}
		days = append(days, d)
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "MobileHolidaysHandler: rows iteration failed", err, "month", month)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"status": "success", "month": month, "data": days})
}
