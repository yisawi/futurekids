package handlers

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"future_kids/internal/tz"
)

// AdminUsernameKey carries the authenticated admin's username (set by AdminMiddleware).
const AdminUsernameKey contextKey = "admin_username"

const activityKey contextKey = "activity"

// Activity log limits, counted in characters as PostgreSQL counts them.
const (
	ActivityLogPageSize   = 50
	ActivityInsertTimeout = 2 * time.Second
	MaxActivityDetail     = 200
	MaxActivityTargetID   = 64
)

// Action codes of logins; every other recorded route has its code in ActivityActions.
const (
	ActionLoginSuccess = "login.success"
	ActionLoginFailed  = "login.failed"
)

const adminLoginPattern = "POST /api/admin/login"

// ActivityActions are the stable action codes of the recorded admin routes, by route pattern.
// A route missing here gets "<last path segment>.<verb>".
var ActivityActions = map[string]string{
	"POST /api/admin/students":        "student.create",
	"PUT /api/admin/students":         "student.update",
	"DELETE /api/admin/students":      "student.deactivate",
	"POST /api/admin/devices":         "device.create",
	"PUT /api/admin/devices":          "device.update",
	"DELETE /api/admin/devices":       "device.disable",
	"POST /api/admin/leaves":          "leave.create",
	"DELETE /api/admin/leaves":        "leave.delete",
	"PUT /api/admin/settings":         "settings.update",
	"PUT /api/admin/schedule":         "schedule.save",
	"POST /api/admin/schedule/import": "schedule.import",
	"POST /api/admin/broadcasts":      "broadcast.send",
	"POST /api/admin/holidays":        "holiday.create",
	"POST /api/admin/holidays/cancel": "holiday.cancel",
	"POST /api/admin/banners":         "banner.create",
	"PUT /api/admin/banners":          "banner.update",
	"DELETE /api/admin/banners":       "banner.delete",
	"GET /api/admin/export/excel":     "export.attendance",
	"GET /api/admin/schedule/export":  "export.schedule",
}

// activityExports are the GET routes recorded because they export children's data.
var activityExports = map[string]bool{
	"GET /api/admin/export/excel":    true,
	"GET /api/admin/schedule/export": true,
}

type activity struct {
	login      string
	actor      string
	targetType string
	targetID   string
	detail     string
}

// NoteActivity attaches the target and detail of the current admin request to its activity log
// row: identifiers and counts only, never names, phone numbers, secrets or free text.
func NoteActivity(r *http.Request, targetType, targetID, detail string) {
	if a, ok := r.Context().Value(activityKey).(*activity); ok {
		a.targetType, a.targetID, a.detail = targetType, targetID, detail
	}
}

// noteLogin records the outcome of an admin login attempt; a failed one is attributed to
// "unknown", never to the submitted username.
func noteLogin(r *http.Request, success bool, username string) {
	if a, ok := r.Context().Value(activityKey).(*activity); ok {
		a.login, a.actor = ActionLoginFailed, "unknown"
		if success {
			a.login, a.actor = ActionLoginSuccess, username
		}
	}
}

// SettingKeysTarget is the target_id of a settings change: the keys, comma-separated.
func SettingKeysTarget(keys ...string) string {
	return truncateChars(strings.Join(keys, ","), MaxActivityTargetID)
}

func positiveIDParam(r *http.Request, name string) string {
	if id, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil && id >= 1 {
		return strconv.Itoa(id)
	}
	return ""
}

func dryRunDetail(dryRun bool, parts ...string) string {
	if dryRun {
		parts = append(parts, "dry_run")
	}
	return strings.Join(parts, " ")
}

type activityWriter struct {
	http.ResponseWriter
	status int
}

func (w *activityWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *activityWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *activityWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// baseWriter returns the server's own writer under the activity wrapper, which
// http.MaxBytesReader needs to close the connection after an oversized body.
func baseWriter(w http.ResponseWriter) http.ResponseWriter {
	if a, ok := w.(*activityWriter); ok {
		return a.ResponseWriter
	}
	return w
}

func activityRecorded(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet:
		return activityExports[r.Pattern]
	case http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// ActivityLog records an admin request in admin_activity_log after its handler has answered:
// every method but GET, HEAD and OPTIONS, the Excel exports, and admin login attempts that
// reached the credential check. The response is passed through untouched; a panic is recorded
// as 500 and re-raised.
func (app *AppEnv) ActivityLog(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		login := r.Pattern == adminLoginPattern
		if !login && !activityRecorded(r) {
			next(w, r)
			return
		}
		act := &activity{}
		aw := &activityWriter{ResponseWriter: w}
		r = r.WithContext(context.WithValue(r.Context(), activityKey, act))
		defer func() {
			p := recover()
			status := aw.status
			if status == 0 {
				status = http.StatusOK
			}
			if p != nil {
				status = http.StatusInternalServerError
			}
			if !login || act.login != "" {
				app.recordActivity(r, act, status)
			}
			if p != nil {
				panic(p)
			}
		}()
		next(aw, r)
	}
}

func defaultActivityAction(r *http.Request) string {
	segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	verb := map[string]string{http.MethodPost: "create", http.MethodPut: "update", http.MethodPatch: "update", http.MethodDelete: "delete", http.MethodGet: "read"}[r.Method]
	if verb == "" {
		verb = strings.ToLower(r.Method)
	}
	return truncateChars(segments[len(segments)-1]+"."+verb, 60)
}

func nullText(s string, max int) any {
	if s == "" {
		return nil
	}
	return truncateChars(s, max)
}

func (app *AppEnv) recordActivity(r *http.Request, a *activity, status int) {
	actor, action := a.actor, a.login
	if actor == "" {
		actor = "admin"
		if u, ok := r.Context().Value(AdminUsernameKey).(string); ok && u != "" {
			actor = u
		}
	}
	if action == "" {
		if action = ActivityActions[r.Pattern]; action == "" {
			action = defaultActivityAction(r)
		}
	}
	ip, _ := app.ClientIP.Resolve(r)
	args := []any{truncateChars(actor, 50), action, r.Method, truncateChars(r.URL.Path, 200), status,
		nullText(ip, 64), nullText(a.targetType, 40), nullText(a.targetID, MaxActivityTargetID), nullText(a.detail, MaxActivityDetail)}
	app.Background.Go("activity log", func() {
		ctx, cancel := context.WithTimeout(context.Background(), ActivityInsertTimeout)
		defer cancel()
		if _, err := app.DB.ExecContext(ctx, `
			INSERT INTO admin_activity_log (actor, action, method, path, status, ip, target_type, target_id, detail)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, args...); err != nil {
			slog.Error("Activity log: insert failed", "action", action, "status", status, "error", err)
		}
	})
}

// ActivityResult is how a status reads in the activity log.
func ActivityResult(status int) string {
	switch {
	case status >= 500:
		return "failed"
	case status >= 400:
		return "rejected"
	}
	return "success"
}

// ActivityLogItem is one recorded admin action.
type ActivityLogItem struct {
	ID         int64   `json:"id"`
	CreatedAt  string  `json:"created_at"`
	Actor      string  `json:"actor"`
	Action     string  `json:"action"`
	Method     string  `json:"method"`
	Path       string  `json:"path"`
	Status     int     `json:"status"`
	Result     string  `json:"result"`
	IP         *string `json:"ip"`
	TargetType *string `json:"target_type"`
	TargetID   *string `json:"target_id"`
	Detail     *string `json:"detail"`
}

// AdminActivityLogHandler (GET /api/admin/activity-log) lists the activity log newest first.
func (app *AppEnv) AdminActivityLogHandler(w http.ResponseWriter, r *http.Request) {
	var before sql.NullInt64
	if v := r.URL.Query().Get("before"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id < 1 {
			respondError(w, http.StatusBadRequest, "before must be a positive activity log id")
			return
		}
		before = sql.NullInt64{Int64: id, Valid: true}
	}
	rows, err := app.DB.QueryContext(r.Context(), `
		SELECT id, created_at AT TIME ZONE 'Asia/Baghdad', actor, action, method, path, status, ip, target_type, target_id, detail
		FROM admin_activity_log
		WHERE ($1::bigint IS NULL OR id < $1)
		ORDER BY id DESC
		LIMIT $2`, before, ActivityLogPageSize+1)
	if err != nil {
		respondInternalError(w, "Database error", "AdminActivityLogHandler: query failed", err)
		return
	}
	defer rows.Close()
	items := []ActivityLogItem{}
	for rows.Next() {
		var it ActivityLogItem
		var createdAt time.Time
		if err := rows.Scan(&it.ID, &createdAt, &it.Actor, &it.Action, &it.Method, &it.Path, &it.Status, &it.IP, &it.TargetType, &it.TargetID, &it.Detail); err != nil {
			respondInternalError(w, "Database error", "AdminActivityLogHandler: scan failed", err)
			return
		}
		it.CreatedAt = createdAt.In(tz.Baghdad).Format(time.RFC3339)
		it.Result = ActivityResult(it.Status)
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "AdminActivityLogHandler: rows iteration failed", err)
		return
	}
	hasMore := len(items) > ActivityLogPageSize
	var nextBefore *int64
	if hasMore {
		items = items[:ActivityLogPageSize]
		nextBefore = &items[ActivityLogPageSize-1].ID
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"status":      "success",
		"data":        items,
		"has_more":    hasMore,
		"next_before": nextBefore,
	})
}
