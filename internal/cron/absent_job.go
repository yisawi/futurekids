package cron

import (
	"database/sql"
	"fmt"
	"log/slog"

	"future_kids/internal/notify"
	"future_kids/internal/tz"

	"firebase.google.com/go/v4/messaging"
)

// ProcessDailyAbsences تُنفذ عند الساعة 12:00 ظهراً بتوقيت العراق
func ProcessDailyAbsences(db *sql.DB, fcmClient *messaging.Client) {
	// الاعتماد الصارم على توقيت بغداد
	today := tz.Today()

	slog.Info("ProcessDailyAbsences: starting", "date", today)

	// استعلام يستثني الحاضرين والمجازين معاً
	query := `
		SELECT s.id, s.full_name, p.phone_number, s.fcm_token 
		FROM students s
		LEFT JOIN parents p ON s.parent_id = p.id
		CROSS JOIN LATERAL get_student_status(s.id, $1::DATE) st
		WHERE s.is_active = true AND st.status = 'Absent'
	`

	rows, err := db.Query(query, today)
	if err != nil {
		slog.Error("ProcessDailyAbsences: query failed", "date", today, "error", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id int
		var fullName string
		var parentPhone sql.NullString
		var fcmToken sql.NullString // استخدام NullString لتجنب أعطال فلاتر إذا كان التوكن فارغاً

		if err := rows.Scan(&id, &fullName, &parentPhone, &fcmToken); err != nil {
			slog.Error("ProcessDailyAbsences: scan failed", "date", today, "error", err)
			continue
		}

		// هنا يتم استدعاء كود إرسال الإشعار الخاص بك (Firebase)
		title := "إشعار غياب"
		body := fmt.Sprintf("الطالب %s غائب اليوم", fullName)
		if parentPhone.Valid && parentPhone.String != "" {
			notify.SaveNotificationHistory(db, parentPhone.String, title, body)
		}
		if fcmToken.Valid && fcmToken.String != "" {
			notify.SendPushNotification(fcmClient, fcmToken.String, title, body)
		}

		slog.Info("ProcessDailyAbsences: processed absence", "student_id", id)
	}

	if err = rows.Err(); err != nil {
		slog.Error("ProcessDailyAbsences: rows iteration failed", "date", today, "error", err)
	}

	slog.Info("ProcessDailyAbsences: finished", "date", today)
}
