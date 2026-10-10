package cron

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"future_kids/internal/background"
	"future_kids/internal/handlers"
	"future_kids/internal/notify"

	"firebase.google.com/go/v4/messaging"
)

// ProcessDailyAbsences تُنفذ عند الساعة 12:00 ظهراً بتوقيت العراق
func ProcessDailyAbsences(db *sql.DB, fcmClient *messaging.Client, bg *background.Group) {
	// الاعتماد الصارم على توقيت بغداد
	today := handlers.Clock().Format("2006-01-02")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	day, err := handlers.DayOn(ctx, db, today)
	cancel()
	if err != nil {
		slog.Error("ProcessDailyAbsences: day lookup failed; nothing sent", "date", today, "error", err)
		return
	}
	if !day.School() {
		slog.Info("ProcessDailyAbsences: skipped, not a school day", "date", today, "day_type", day.Type)
		return
	}

	slog.Info("ProcessDailyAbsences: starting", "date", today)

	// استعلام يستثني الحاضرين والمجازين معاً
	query := `
		SELECT s.id, s.full_name, p.id, p.phone_number, ` + fmt.Sprintf(notify.ParentDeviceTokensSQL, "s.parent_id") + `
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
		var parentID sql.NullInt64
		var parentPhone sql.NullString
		var deviceTokens sql.NullString

		if err := rows.Scan(&id, &fullName, &parentID, &parentPhone, &deviceTokens); err != nil {
			slog.Error("ProcessDailyAbsences: scan failed", "date", today, "error", err)
			continue
		}

		// هنا يتم استدعاء كود إرسال الإشعار الخاص بك (Firebase)
		title := "إشعار غياب"
		body := fmt.Sprintf("الطالب %s غائب اليوم", fullName)
		if parentID.Valid {
			notify.SaveNotificationHistory(db, int(parentID.Int64), parentPhone.String, title, body)
		}
		notify.SendToDevices(bg, fcmClient, db, notify.DeviceTokens(deviceTokens), title, body)

		slog.Info("ProcessDailyAbsences: processed absence", "student_id", id)
	}

	if err = rows.Err(); err != nil {
		slog.Error("ProcessDailyAbsences: rows iteration failed", "date", today, "error", err)
	}

	slog.Info("ProcessDailyAbsences: finished", "date", today)
}
