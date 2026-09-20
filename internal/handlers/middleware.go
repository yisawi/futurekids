package handlers

import (
	"context"
	"database/sql"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"future_kids/internal/auth" // تأكد من أن اسم المشروع هنا يطابق اسم مشروعك الفعلي
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

// DeviceAuthMiddleware ensures requests come from an approved active attendance device.
func (app *AppEnv) DeviceAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deviceSN := r.URL.Query().Get("SN")
		if deviceSN == "" {
			http.Error(w, "Device SN is required", http.StatusUnauthorized)
			return
		}

		var isActive bool
		err := app.DB.QueryRowContext(
			r.Context(),
			"SELECT is_active FROM devices WHERE serial_number = $1",
			deviceSN,
		).Scan(&isActive)
		if err != nil {
			if err == sql.ErrNoRows {
				slog.Warn("Unauthorized device attempted connection", "device_sn", deviceSN, "ip", r.RemoteAddr)
				http.Error(w, "Unauthorized Device", http.StatusUnauthorized)
			} else {
				slog.Error("Database error during device validation", "error", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
			return
		}

		if !isActive {
			slog.Warn("Disabled device attempted connection", "device_sn", deviceSN)
			http.Error(w, "Device is disabled", http.StatusForbidden)
			return
		}

		go func(sn string) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Recovered panic in async goroutine: %v", r)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			_, err := app.DB.ExecContext(
				ctx,
				"UPDATE devices SET last_sync = CURRENT_TIMESTAMP WHERE serial_number = $1",
				sn,
			)
			if err != nil {
				slog.Error("Failed to update device last_sync", "device_sn", sn, "error", err)
			}
		}(deviceSN)

		next.ServeHTTP(w, r)
	}
}

// ParentIDKey is the typed context key used to pass the authenticated parent's DB id.
type contextKey string

const ParentIDKey contextKey = "parent_id"

// AuthMiddleware protects mobile routes by validating the JWT token and injecting parent_id into context.
func AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. Extract Authorization header
		authHeader := r.Header.Get("Authorization")
		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			http.Error(w, `{"status":"error","message":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}

		// 2. Validate token via auth package
		claims, err := auth.ValidateToken(parts[1])
		if err != nil {
			http.Error(w, `{"status":"error","message":"Invalid or expired token"}`, http.StatusUnauthorized)
			return
		}

		// 3. Enforce parent role
		if role, ok := claims["role"].(string); !ok || role != "parent" {
			http.Error(w, `{"status":"error","message":"Access denied"}`, http.StatusForbidden)
			return
		}

		// 4. Inject parent_id into context so handlers cannot be spoofed
		parentID := int(claims["parent_id"].(float64))
		ctx := context.WithValue(r.Context(), ParentIDKey, parentID)

		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

// AdminMiddleware permits only valid JWTs carrying the admin role.
func AdminMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		parts := strings.Fields(authHeader)
		if len(parts) != 2 || parts[0] != "Bearer" {
			http.Error(w, `{"status":"error","message":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}

		claims, err := auth.ValidateToken(parts[1])
		if err != nil {
			http.Error(w, `{"status":"error","message":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if role, ok := claims["role"].(string); !ok || role != "admin" {
			http.Error(w, `{"status":"error","message":"Forbidden"}`, http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	}
}
