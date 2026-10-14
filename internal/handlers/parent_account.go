package handlers

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"future_kids/internal/ratelimit"

	"golang.org/x/crypto/bcrypt"
)

// Messages of the parent account endpoints; clients tell the 400s apart by them.
const (
	AccountPINRequiredMessage    = "pin is required"
	IncorrectPINMessage          = "Incorrect PIN"
	AccountDeletedMessage        = "Account deleted"
	PushEnabledRequiredMessage   = "push_enabled is required (true or false)"
	PushDisabledTokenMessage     = "Push notifications are turned off for this account; device token not registered"
	DeviceTokenRegisteredMessage = "Device token registered"
)

// Activity log entry written when a parent deletes the account. It names the children that were
// unlinked (never the parent), so the school knows whom to re-link.
const (
	ActionParentAccountDelete = "parent.account_delete"
	ParentActor               = "parent"
	NoLinkedStudentsDetail    = "no linked students"
)

// errParentGone means the authenticated parent's row no longer exists (the account was deleted
// after the token was checked).
var errParentGone = errors.New("parent no longer exists")

// respondParentGone answers a request whose parent was deleted while it ran, as AuthMiddleware
// answers the next one.
func respondParentGone(w http.ResponseWriter, r *http.Request, handler string, parentID int) {
	slog.Warn(handler+": parent no longer exists", "path", r.URL.Path, "parent_id", parentID)
	respondError(w, http.StatusUnauthorized, "Invalid or expired token")
}

type AccountDeleteRequest struct {
	Pin string `json:"pin"`
}

type unlinkedStudent struct {
	id   int
	name string
}

// AccountDeleteHandler (POST /api/mobile/account/delete) deletes the authenticated parent after
// checking the PIN as login does, counted by the same per-phone limiter. The parent's children
// and their attendance stay, with no parent; device tokens and notifications go with the parent.
// Nothing about the parent (phone, name, PIN) is logged or recorded.
func (app *AppEnv) AccountDeleteHandler(w http.ResponseWriter, r *http.Request) {
	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok {
		respondError(w, http.StatusUnauthorized, "Unauthorized context")
		return
	}
	var req AccountDeleteRequest
	if !decodeJSONBody(w, r, &req, "Invalid request body") {
		return
	}
	if req.Pin == "" {
		respondError(w, http.StatusBadRequest, AccountPINRequiredMessage)
		return
	}
	if app.LoginLimiter == nil {
		slog.Error("AccountDeleteHandler: LoginLimiter is not configured")
		respondError(w, http.StatusInternalServerError, "Internal server error")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// The limiter key is the canonical phone number, shared with login.
	var phone string
	switch err := app.DB.QueryRowContext(ctx, `SELECT phone_number FROM parents WHERE id = $1`, parentID).Scan(&phone); {
	case err == sql.ErrNoRows:
		respondParentGone(w, r, "AccountDeleteHandler", parentID)
		return
	case err != nil:
		respondInternalError(w, "Internal server error", "AccountDeleteHandler: parent lookup failed", err, "parent_id", parentID)
		return
	}
	allowed, retryAfter := app.LoginLimiter.Allow(phone)
	if !allowed {
		logRateLimited("AccountDeleteHandler", r, "parent_id", parentID)
		respondTooManyAttempts(w, retryAfter)
		return
	}
	outcome := ratelimit.Released
	defer func() { app.LoginLimiter.Finish(phone, outcome) }()

	tx, err := app.DB.BeginTx(ctx, nil)
	if err != nil {
		respondInternalError(w, "Internal server error", "AccountDeleteHandler: begin failed", err, "parent_id", parentID)
		return
	}
	defer tx.Rollback()

	var pinHash string
	switch err := tx.QueryRowContext(ctx, `SELECT pin_code FROM parents WHERE id = $1 FOR UPDATE`, parentID).Scan(&pinHash); {
	case err == sql.ErrNoRows:
		respondParentGone(w, r, "AccountDeleteHandler", parentID)
		return
	case err != nil:
		respondInternalError(w, "Internal server error", "AccountDeleteHandler: parent lock failed", err, "parent_id", parentID)
		return
	}
	// Any comparison error is a wrong PIN, exactly as at login (bcrypt reads at most 72 bytes).
	if err := bcrypt.CompareHashAndPassword([]byte(pinHash), []byte(req.Pin)); err != nil {
		outcome = ratelimit.Failed
		respondError(w, http.StatusBadRequest, IncorrectPINMessage)
		return
	}

	students, err := unlinkStudents(ctx, tx, parentID)
	if err != nil {
		respondInternalError(w, "Internal server error", "AccountDeleteHandler: unlink failed", err, "parent_id", parentID)
		return
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM parents WHERE id = $1`, parentID); err != nil {
		respondInternalError(w, "Internal server error", "AccountDeleteHandler: delete failed", err, "parent_id", parentID)
		return
	}
	if err := recordAccountDelete(ctx, tx, r, students); err != nil {
		respondInternalError(w, "Internal server error", "AccountDeleteHandler: activity log failed", err, "parent_id", parentID)
		return
	}
	if err := tx.Commit(); err != nil {
		respondInternalError(w, "Internal server error", "AccountDeleteHandler: commit failed", err, "parent_id", parentID)
		return
	}
	outcome = ratelimit.Succeeded
	slog.Info("Parent account deleted", "parent_id", parentID, "students", len(students))
	respondJSON(w, http.StatusOK, map[string]string{"status": "success", "message": AccountDeletedMessage})
}

// unlinkStudents removes parentID from all of its students, active or not, and returns them.
func unlinkStudents(ctx context.Context, tx *sql.Tx, parentID int) ([]unlinkedStudent, error) {
	rows, err := tx.QueryContext(ctx, `UPDATE students SET parent_id = NULL WHERE parent_id = $1 RETURNING id, full_name`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var students []unlinkedStudent
	for rows.Next() {
		var s unlinkedStudent
		if err := rows.Scan(&s.id, &s.name); err != nil {
			return nil, err
		}
		students = append(students, s)
	}
	return students, rows.Err()
}

// recordAccountDelete writes the activity log rows of a deleted account inside tx: one per
// unlinked student (its id and name), or one without a target when there was none. The parent's
// IP address is not stored.
func recordAccountDelete(ctx context.Context, tx *sql.Tx, r *http.Request, students []unlinkedStudent) error {
	ids, names := []int64{}, []string{}
	for _, s := range students {
		ids = append(ids, int64(s.id))
		names = append(names, truncateChars(s.name, MaxActivityDetail))
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO admin_activity_log (actor, action, method, path, status, ip, target_type, target_id, detail)
		SELECT $1::text, $2::text, $3::text, $4::text, $5::smallint, NULL, 'student', s.id::text, s.name
		FROM unnest($6::bigint[], $7::text[]) AS s(id, name)
		UNION ALL
		SELECT $1::text, $2::text, $3::text, $4::text, $5::smallint, NULL, NULL, NULL, $8::text
		WHERE cardinality($6::bigint[]) = 0`,
		ParentActor, ActionParentAccountDelete, r.Method, truncateChars(r.URL.Path, 200), http.StatusOK, ids, names, NoLinkedStudentsDetail)
	return err
}

// NotificationSettingsRequest is the body of PUT /api/mobile/notification-settings.
type NotificationSettingsRequest struct {
	PushEnabled *bool `json:"push_enabled"`
}

func respondNotificationSettings(w http.ResponseWriter, enabled bool) {
	respondJSON(w, http.StatusOK, map[string]any{"status": "success", "data": map[string]bool{"push_enabled": enabled}})
}

// NotificationSettingsHandler serves GET and PUT /api/mobile/notification-settings: the parent's
// switch for push notifications. Turning it off deletes all of the parent's device tokens in the
// same transaction, and registration stores none while it is off, so no push reaches the parent;
// notifications are still written to the parent's list. Turning it on stores no token: the app
// registers its token again.
func (app *AppEnv) NotificationSettingsHandler(w http.ResponseWriter, r *http.Request) {
	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok {
		respondError(w, http.StatusUnauthorized, "Unauthorized context")
		return
	}
	if r.Method == http.MethodGet {
		var enabled bool
		switch err := app.DB.QueryRowContext(r.Context(), `SELECT push_enabled FROM parents WHERE id = $1`, parentID).Scan(&enabled); {
		case err == sql.ErrNoRows:
			respondParentGone(w, r, "NotificationSettingsHandler", parentID)
		case err != nil:
			respondInternalError(w, "Internal server error", "NotificationSettingsHandler: query failed", err, "parent_id", parentID)
		default:
			respondNotificationSettings(w, enabled)
		}
		return
	}

	var req NotificationSettingsRequest
	if !decodeJSONBody(w, r, &req, "Invalid request body") {
		return
	}
	if req.PushEnabled == nil {
		respondError(w, http.StatusBadRequest, PushEnabledRequiredMessage)
		return
	}
	enabled := *req.PushEnabled
	err := func() error {
		tx, err := app.DB.BeginTx(r.Context(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		res, err := tx.ExecContext(r.Context(), `UPDATE parents SET push_enabled = $2 WHERE id = $1`, parentID, enabled)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return errParentGone
		}
		if !enabled {
			if _, err := tx.ExecContext(r.Context(), `DELETE FROM device_tokens WHERE parent_id = $1`, parentID); err != nil {
				return err
			}
		}
		return tx.Commit()
	}()
	switch {
	case errors.Is(err, errParentGone):
		respondParentGone(w, r, "NotificationSettingsHandler", parentID)
	case err != nil:
		respondInternalError(w, "Internal server error", "NotificationSettingsHandler: update failed", err, "parent_id", parentID, "push_enabled", enabled)
	default:
		slog.Info("Push notifications switched", "parent_id", parentID, "push_enabled", enabled)
		respondNotificationSettings(w, enabled)
	}
}
