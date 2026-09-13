package handlers

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"future_kids/internal/auth"
	"golang.org/x/crypto/bcrypt"
)

type AdminLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type DashboardStats struct {
	TotalStudents int `json:"total_students"`
	PresentToday  int `json:"present_today"`
	AbsentToday   int `json:"absent_today"`
	ExcusedToday  int `json:"excused_today"`
}

func (app *AppEnv) AdminLoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req AdminLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"status":"error","message":"Invalid request"}`, http.StatusBadRequest)
		return
	}

	// جلب الهاش المخزن في قاعدة البيانات
	var storedHash string
	query := `SELECT password_hash FROM admins WHERE username = $1`
	err := app.DB.QueryRowContext(r.Context(), query, req.Username).Scan(&storedHash)

	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, `{"status":"error","message":"بيانات الدخول غير صحيحة"}`, http.StatusUnauthorized)
			return
		}
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	// مقارنة كلمة المرور المدخلة مع الهاش
	err = bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(req.Password))
	if err != nil {
		http.Error(w, `{"status":"error","message":"بيانات الدخول غير صحيحة"}`, http.StatusUnauthorized)
		return
	}

	// إصدار توكن الإدارة
	tokenString, err := auth.GenerateAdminToken(req.Username)
	if err != nil {
		http.Error(w, `{"status":"error","message":"Could not generate token"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data": map[string]string{
			"token": tokenString,
		},
	})
}

func (app *AppEnv) AdminDashboardHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	loc, err := time.LoadLocation("Asia/Baghdad")
	if err != nil {
		slog.Error("Failed to load Baghdad timezone", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}
	today := time.Now().In(loc).Format("2006-01-02")

	query := `
		SELECT
			(SELECT COUNT(*) FROM students) AS total_students,
			(SELECT COUNT(DISTINCT student_id)
			 FROM attendance_logs
			 WHERE DATE(check_time) = $1 AND status = 'present') AS present_today,
			(SELECT COUNT(DISTINCT student_id)
			 FROM attendance_logs
			 WHERE DATE(check_time) = $1 AND status = 'absent') AS absent_today,
			(SELECT COUNT(DISTINCT student_id)
			 FROM student_leaves
			 WHERE leave_date = $1) AS excused_today;
	`

	var stats DashboardStats
	if err := app.DB.QueryRowContext(r.Context(), query, today).Scan(
		&stats.TotalStudents,
		&stats.PresentToday,
		&stats.AbsentToday,
		&stats.ExcusedToday,
	); err != nil {
		slog.Error("Failed to fetch admin dashboard statistics", "error", err)
		http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data":   stats,
	}); err != nil {
		slog.Error("Failed to encode admin dashboard response", "error", err)
	}
}
