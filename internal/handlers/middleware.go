package handlers

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"future_kids/internal/auth"
)

// HardwareLoggerMiddleware logs the details of requests coming from the hardware devices
func HardwareLoggerMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// passing to the main func ADMSHandler
		next.ServeHTTP(w, r)

		// check the time taken to process the request
		duration := time.Since(start)
		deviceSN := r.URL.Query().Get("SN")
		if deviceSN == "" {
			deviceSN = "UNKNOWN"
		}

		// record the events with all details
		slog.Info("Hardware HTTP Request",
			"method", r.Method,
			"path", r.URL.Path,
			"device_sn", deviceSN,
			"ip", r.RemoteAddr,
			"duration_ms", duration.Milliseconds(),
		)
	}
}

// DeviceAuthMiddleware ensures requests come from an approved active attendance device and
// passes the verified serial number to the next handler under DeviceSNKey.
func (app *AppEnv) DeviceAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deviceSN := r.URL.Query().Get("SN")
		if deviceSN == "" {
			slog.Warn("DeviceAuthMiddleware: missing device SN", "path", r.URL.Path, "remote_addr", r.RemoteAddr)
			respondError(w, http.StatusUnauthorized, "Device SN is required")
			return
		}

		var isActive bool
		err := app.DB.QueryRowContext(
			r.Context(),
			"SELECT COALESCE(is_active, true) FROM devices WHERE serial_number = $1",
			deviceSN,
		).Scan(&isActive)
		if err != nil {
			switch {
			case err == sql.ErrNoRows:
				slog.Warn("DeviceAuthMiddleware: unregistered device rejected", "device_sn", deviceSN, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
				respondError(w, http.StatusUnauthorized, "Unauthorized Device")
			case isTransientDBError(err):
				respondRetry(w, "DeviceAuthMiddleware: database unavailable", err, "device_sn", deviceSN)
			default:
				respondInternalError(w, "Internal Server Error", "DeviceAuthMiddleware: query failed", err, "device_sn", deviceSN)
			}
			return
		}

		if !isActive {
			slog.Warn("DeviceAuthMiddleware: disabled device rejected", "device_sn", deviceSN, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
			respondError(w, http.StatusForbidden, "Device is disabled")
			return
		}

		app.Background.Go("device last_sync", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			_, err := app.DB.ExecContext(
				ctx,
				"UPDATE devices SET last_sync = CURRENT_TIMESTAMP WHERE serial_number = $1",
				deviceSN,
			)
			if err != nil {
				slog.Error("Failed to update device last_sync", "device_sn", deviceSN, "error", err)
			}
		})

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), DeviceSNKey, deviceSN)))
	}
}

// contextKey types the values middleware passes to handlers through the request context.
type contextKey string

// ParentIDKey carries the authenticated parent's DB id (set by AuthMiddleware).
const ParentIDKey contextKey = "parent_id"

// DeviceSNKey carries the verified device serial number (set by DeviceAuthMiddleware).
const DeviceSNKey contextKey = "device_sn"

// extractBearerToken pulls the token string from an "Authorization: Bearer <token>" header.
// Returns the token and true on success, empty string and false if the header is missing or malformed.
func extractBearerToken(r *http.Request) (string, bool) {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || parts[0] != "Bearer" {
		return "", false
	}
	return parts[1], true
}

// AuthMiddleware protects mobile routes: it validates the JWT, checks that its session version
// is still the parent's current one (a PIN change signs out older tokens), and injects
// parent_id into the context.
func (app *AppEnv) AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. Extract Authorization header
		token, ok := extractBearerToken(r)
		if !ok {
			slog.Warn("AuthMiddleware: missing bearer token", "path", r.URL.Path, "remote_addr", r.RemoteAddr)
			respondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		// 2. Validate token via auth package
		claims, err := auth.ValidateToken(token)
		if err != nil {
			slog.Warn("AuthMiddleware: invalid token", "path", r.URL.Path, "remote_addr", r.RemoteAddr, "error", err)
			respondError(w, http.StatusUnauthorized, "Invalid or expired token")
			return
		}

		// 3. Enforce parent role
		role, err := auth.Role(claims)
		if err != nil {
			rejectMalformedClaims(w, r, "AuthMiddleware", err)
			return
		}
		if role != "parent" {
			slog.Warn("AuthMiddleware: token without parent role", "path", r.URL.Path, "remote_addr", r.RemoteAddr, "role", role)
			respondError(w, http.StatusForbidden, "Access denied")
			return
		}

		// 4. Inject parent_id into context so handlers cannot be spoofed
		parentID, err := auth.ParentID(claims)
		if err != nil {
			rejectMalformedClaims(w, r, "AuthMiddleware", err)
			return
		}
		version, err := auth.SessionVersion(claims)
		if err != nil {
			rejectMalformedClaims(w, r, "AuthMiddleware", err)
			return
		}
		var current int
		switch err := app.DB.QueryRowContext(r.Context(), `SELECT session_version FROM parents WHERE id = $1`, parentID).Scan(&current); {
		case err == sql.ErrNoRows:
			slog.Warn("AuthMiddleware: token for a parent that no longer exists", "path", r.URL.Path, "remote_addr", r.RemoteAddr, "parent_id", parentID)
			respondError(w, http.StatusUnauthorized, "Invalid or expired token")
			return
		case err != nil:
			respondInternalError(w, "Internal server error", "AuthMiddleware: session lookup failed", err, "parent_id", parentID)
			return
		case current != version:
			slog.Warn("AuthMiddleware: token issued before the parent's PIN changed", "path", r.URL.Path, "remote_addr", r.RemoteAddr, "parent_id", parentID, "token_version", version, "current_version", current)
			respondError(w, http.StatusUnauthorized, "Invalid or expired token")
			return
		}
		ctx := context.WithValue(r.Context(), ParentIDKey, parentID)

		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

// AdminMiddleware permits only valid JWTs carrying the admin role whose session version is
// still the admin's current one (rotating the password signs out older tokens).
func (app *AppEnv) AdminMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := extractBearerToken(r)
		if !ok {
			slog.Warn("AdminMiddleware: missing bearer token", "path", r.URL.Path, "remote_addr", r.RemoteAddr)
			respondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		claims, err := auth.ValidateToken(token)
		if err != nil {
			slog.Warn("AdminMiddleware: invalid token", "path", r.URL.Path, "remote_addr", r.RemoteAddr, "error", err)
			respondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		role, err := auth.Role(claims)
		if err != nil {
			rejectMalformedClaims(w, r, "AdminMiddleware", err)
			return
		}
		if role != "admin" {
			slog.Warn("AdminMiddleware: token without admin role", "path", r.URL.Path, "remote_addr", r.RemoteAddr, "role", role)
			respondError(w, http.StatusForbidden, "Forbidden")
			return
		}
		username, err := auth.Username(claims)
		if err != nil {
			rejectMalformedClaims(w, r, "AdminMiddleware", err)
			return
		}
		version, err := auth.SessionVersion(claims)
		if err != nil {
			rejectMalformedClaims(w, r, "AdminMiddleware", err)
			return
		}
		var current int
		switch err := app.DB.QueryRowContext(r.Context(), `SELECT session_version FROM admins WHERE username = $1`, username).Scan(&current); {
		case err == sql.ErrNoRows:
			slog.Warn("AdminMiddleware: token for an admin that no longer exists", "path", r.URL.Path, "remote_addr", r.RemoteAddr, "username", username)
			respondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		case err != nil:
			respondInternalError(w, "Internal server error", "AdminMiddleware: session lookup failed", err, "username", username)
			return
		case current != version:
			slog.Warn("AdminMiddleware: token issued before the admin password changed", "path", r.URL.Path, "remote_addr", r.RemoteAddr, "username", username, "token_version", version, "current_version", current)
			respondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		next.ServeHTTP(w, r)
	}
}

// rejectMalformedClaims logs a validly signed token whose claims are missing or mistyped
// and answers 401, the same as any other unusable token.
func rejectMalformedClaims(w http.ResponseWriter, r *http.Request, middleware string, err error) {
	attrs := []any{"path", r.URL.Path, "remote_addr", r.RemoteAddr, "error", err}
	var ce *auth.ClaimError
	if errors.As(err, &ce) {
		attrs = append(attrs, "claim", ce.Claim)
	}
	slog.Warn(middleware+": malformed token claims", attrs...)
	respondError(w, http.StatusUnauthorized, "Invalid or expired token")
}
