package handlers

import (
	"context"
	"database/sql"
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

// AuthMiddleware protects routes by validating the JWT token
func AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. Search for the Authorization header
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, `{"status":"error","message":"غير مصرح بالوصول: التوكن مفقود"}`, http.StatusUnauthorized)
			return
		}

		// 2. Validate the Bearer format
		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			http.Error(w, `{"status":"error","message":"غير مصرح بالوصول: صيغة التوكن غير صحيحة"}`, http.StatusUnauthorized)
			return
		}

		// 3. Validate the token via the auth package
		tokenString := parts[1]
		claims, err := auth.ValidateToken(tokenString)
		if err != nil {
			http.Error(w, `{"status":"error","message":"غير مصرح بالوصول: التوكن غير صالح أو منتهي الصلاحية"}`, http.StatusUnauthorized)
			return
		}

		// 4. Extract phone number and pass it to the context
		ctx := context.WithValue(r.Context(), "phone", claims["phone"])
		r = r.WithContext(ctx)

		// 5. Allow access to the next handler
		next.ServeHTTP(w, r)
	}
}
