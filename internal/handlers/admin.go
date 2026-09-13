package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"future_kids/internal/auth"
	"golang.org/x/crypto/bcrypt"
)

type AdminLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
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
