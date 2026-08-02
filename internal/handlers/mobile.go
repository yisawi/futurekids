package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"future_kids/internal/auth"
	"log/slog"
	"net/http"
	"time"
)

// Struct that will be JSON for Flutter
type AttendanceRecord struct {
	StudentID int       `json:"student_id"` // change it to 'int' based on our DB
	FullName  string    `json:"full_name"`
	CheckTime time.Time `json:"check_time"`
	DeviceSN  string    `json:"device_sn"`
}

// LoginRequest represents the expected JSON payload from the Flutter app for login
type LoginRequest struct {
	PhoneNumber string `json:"phone_number"`
	FCMToken    string `json:"fcm_token"`
}

// this func is now bound to AppEnv to access app.DB.
func (app *AppEnv) GetTodayAttendanceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Query for GET Today's Data with Student names
	query := `
        SELECT s.id, s.full_name, a.check_time, a.device_sn
        FROM attendance_logs a
        JOIN students s ON a.student_id = s.id
        WHERE DATE(a.check_time) = CURRENT_DATE
        ORDER BY a.check_time DESC;
    `

	// Set timeout for Query
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := app.DB.QueryContext(ctx, query)
	if err != nil {
		slog.Error("Database query failed", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var records []AttendanceRecord

	// Read the data line-by-line
	for rows.Next() {
		var rec AttendanceRecord
		if err := rows.Scan(&rec.StudentID, &rec.FullName, &rec.CheckTime, &rec.DeviceSN); err != nil {
			slog.Error("Row scan failed", "error", err)
			continue
		}
		records = append(records, rec)
	}

	if err := rows.Err(); err != nil {
		slog.Error("Error during rows iteration", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// This step ensures an empty array [] is sent instead of null if no one attends today.
	if records == nil {
		records = []AttendanceRecord{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(records); err != nil {
		slog.Error("Failed to encode json", "error", err)
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// MobileLoginHandler handles the authentication request from the mobile app
func (app *AppEnv) MobileLoginHandler(w http.ResponseWriter, r *http.Request) {
	// 1. Ensure the request method is POST only
	if r.Method != http.MethodPost {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// 2. Decode the incoming JSON payload
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"status":"error","message":"Invalid request body"}`, http.StatusBadRequest)
		return
	}

	// --- Real Database Validation ---
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var studentID int
	var parentPhone string

	// 3. Check if the phone number exists in the students table
	query := `SELECT id, parent_phone FROM students WHERE parent_phone = $1 LIMIT 1`
	err := app.DB.QueryRowContext(ctx, query, req.PhoneNumber).Scan(&studentID, &parentPhone)

	if err != nil {
		if err == sql.ErrNoRows {
			// الهاتف غير موجود في قاعدة البيانات
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"status":"error","message":"رقم الهاتف غير مسجل في النظام"}`))
			return
		}
		// خطأ داخلي في قاعدة البيانات
		slog.Error("Database query failed during login", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	// 4. Update the FCM Token for this guardian's phone number
	updateQuery := `UPDATE students SET fcm_token = $1 WHERE parent_phone = $2`
	_, err = app.DB.ExecContext(ctx, updateQuery, req.FCMToken, req.PhoneNumber)
	if err != nil {
		slog.Error("Failed to update FCM token", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	// 5. Generate the Real JWT Token
	tokenString, err := auth.GenerateToken(req.PhoneNumber)
	if err != nil {
		slog.Error("Failed to generate JWT", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	// 6. Send the success response with the real token
	response := map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"access_token": tokenString, // هنا يتم حقن التوكن الحقيقي المشفر
			"guardian": map[string]interface{}{
				"phone":   parentPhone,
				"message": "تم التحقق من الهاتف وتحديث مفتاح الإشعارات بنجاح",
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}
