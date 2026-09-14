package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
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

type StudentPayload struct {
	ID          int    `json:"id,omitempty"`
	Name        string `json:"name"`
	ParentPhone string `json:"parent_phone"`
	ParentPin   string `json:"parent_pin"`
	RfidTag     string `json:"rfid_tag"`
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

func (app *AppEnv) AdminStudentsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		query := `SELECT id, full_name, parent_phone, COALESCE(parent_pin, '1234'), COALESCE(rfid_tag, '') FROM students ORDER BY id DESC`
		rows, err := app.DB.QueryContext(r.Context(), query)
		if err != nil {
			slog.Error("Failed to fetch students", "error", err)
			http.Error(w, `{"status":"error","message":"Database error"}`, http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var students []StudentPayload
		for rows.Next() {
			var student StudentPayload
			if err := rows.Scan(&student.ID, &student.Name, &student.ParentPhone, &student.ParentPin, &student.RfidTag); err != nil {
				slog.Error("Failed to scan student", "error", err)
				continue
			}
			students = append(students, student)
		}
		if err := rows.Err(); err != nil {
			slog.Error("Failed while reading students", "error", err)
			http.Error(w, `{"status":"error","message":"Database error"}`, http.StatusInternalServerError)
			return
		}
		if students == nil {
			students = []StudentPayload{}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "success", "data": students})

	case http.MethodPost:
		var student StudentPayload
		if err := json.NewDecoder(r.Body).Decode(&student); err != nil {
			http.Error(w, `{"status":"error","message":"Invalid request body"}`, http.StatusBadRequest)
			return
		}
		if student.ParentPin == "" {
			student.ParentPin = "1234"
		}
		if student.RfidTag == "" {
			student.RfidTag = fmt.Sprintf("admin-%d", time.Now().UnixNano())
		}

		query := `INSERT INTO students (full_name, parent_phone, parent_pin, rfid_tag) VALUES ($1, $2, $3, $4) RETURNING id`
		if err := app.DB.QueryRowContext(r.Context(), query, student.Name, student.ParentPhone, student.ParentPin, student.RfidTag).Scan(&student.ID); err != nil {
			slog.Error("Failed to create student", "error", err)
			http.Error(w, `{"status":"error","message":"Failed to create student"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "success", "message": "Student created", "data": student})

	case http.MethodPut:
		var student StudentPayload
		if err := json.NewDecoder(r.Body).Decode(&student); err != nil || student.ID == 0 {
			http.Error(w, `{"status":"error","message":"Invalid request body or missing ID"}`, http.StatusBadRequest)
			return
		}
		if student.RfidTag == "" {
			student.RfidTag = fmt.Sprintf("admin-%d", time.Now().UnixNano())
		}

		query := `UPDATE students SET full_name = $1, parent_phone = $2, parent_pin = $3, rfid_tag = $4 WHERE id = $5`
		result, err := app.DB.ExecContext(r.Context(), query, student.Name, student.ParentPhone, student.ParentPin, student.RfidTag, student.ID)
		if err != nil {
			slog.Error("Failed to update student", "error", err)
			http.Error(w, `{"status":"error","message":"Failed to update student"}`, http.StatusInternalServerError)
			return
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			slog.Error("Failed to inspect student update", "error", err)
			http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
			return
		}
		if rowsAffected == 0 {
			http.Error(w, `{"status":"error","message":"Student not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "success", "message": "Student updated"})

	case http.MethodDelete:
		id, err := strconv.Atoi(r.URL.Query().Get("id"))
		if err != nil || id == 0 {
			http.Error(w, `{"status":"error","message":"Invalid student ID"}`, http.StatusBadRequest)
			return
		}

		result, err := app.DB.ExecContext(r.Context(), `DELETE FROM students WHERE id = $1`, id)
		if err != nil {
			slog.Error("Failed to delete student", "error", err)
			http.Error(w, `{"status":"error","message":"Cannot delete student. Check related attendance records."}`, http.StatusConflict)
			return
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			slog.Error("Failed to inspect student deletion", "error", err)
			http.Error(w, `{"status":"error","message":"Internal server error"}`, http.StatusInternalServerError)
			return
		}
		if rowsAffected == 0 {
			http.Error(w, `{"status":"error","message":"Student not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "success", "message": "Student deleted"})

	default:
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

type LeavePayload struct {
	StudentID int    `json:"student_id"`
	LeaveDate string `json:"leave_date"` // Format: YYYY-MM-DD
	Notes     string `json:"notes"`
}

func (app *AppEnv) AdminCreateLeaveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req LeavePayload
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"status":"error","message":"Invalid request payload"}`, http.StatusBadRequest)
		return
	}

	if req.StudentID == 0 || req.LeaveDate == "" {
		http.Error(w, `{"status":"error","message":"student_id and leave_date are required"}`, http.StatusBadRequest)
		return
	}

	// استخدام ON CONFLICT لتحديث الملاحظات إذا كانت الإجازة مسجلة مسبقاً لنفس اليوم
	query := `
		INSERT INTO student_leaves (student_id, leave_date, notes) 
		VALUES ($1, $2, $3)
		ON CONFLICT (student_id, leave_date) 
		DO UPDATE SET notes = EXCLUDED.notes
		RETURNING id
	`

	var leaveID int
	err := app.DB.QueryRowContext(r.Context(), query, req.StudentID, req.LeaveDate, req.Notes).Scan(&leaveID)

	if err != nil {
		slog.Error("Failed to create leave record", "error", err)
		http.Error(w, `{"status":"error","message":"Failed to create leave record"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Leave recorded successfully",
		"data":    map[string]int{"leave_id": leaveID},
	})
}
