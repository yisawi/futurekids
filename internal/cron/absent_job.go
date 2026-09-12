package cron

import (
	"database/sql"
	"log/slog"
	"time"
)

// ProcessDailyAbsences تُنفذ عند الساعة 12:00 ظهراً بتوقيت العراق
func ProcessDailyAbsences(db *sql.DB) {
	// تحديد التوقيت المحلي بدقة
	loc, err := time.LoadLocation("Asia/Baghdad")
	if err != nil {
		slog.Error("Failed to load Baghdad timezone", "error", err)
		return
	}

	today := time.Now().In(loc).Format("2006-01-02")
	slog.Info("Running daily absence job", "date", today)

	// استعلام ذكي:
	// 1. يبحث عن الطلاب الذين لم يحضروا اليوم وليس لديهم إجازة.
	// 2. يدرجهم كـ "غائب" في جدول الحضور.
	// 3. يرجع أرقام هواتف أولياء أمورهم لإرسال الإشعارات.
	query := `
		WITH missing_students AS (
			SELECT id, parent_phone, full_name 
			FROM students
			WHERE id NOT IN (
				SELECT student_id 
				FROM attendance_logs 
				-- تحويل وقت السيرفر العالمي إلى توقيت بغداد قبل استخراج التاريخ
				WHERE DATE(check_time AT TIME ZONE 'UTC' AT TIME ZONE 'Asia/Baghdad') = $1
			)
			  AND id NOT IN (
				SELECT student_id 
				FROM student_leaves 
				WHERE leave_date = $1
			)
		),
		inserted_absences AS (
			INSERT INTO attendance_logs (student_id, device_sn, check_time, status)
			SELECT id, 'SYSTEM_CRON', NOW(), 'absent'
			FROM missing_students
			RETURNING student_id
		)
		SELECT full_name, parent_phone FROM missing_students;
	`

	rows, err := db.Query(query, today)
	if err != nil {
		slog.Error("Failed to execute absence query", "error", err)
		return
	}
	defer rows.Close()

	// إرسال إشعارات FCM للطلاب الغائبين
	for rows.Next() {
		var fullName, parentPhone string
		if err := rows.Scan(&fullName, &parentPhone); err != nil {
			continue
		}

		title := "إشعار غياب"
		body := "الطالب " + fullName + " غائب هذا اليوم ولم يلتحق بالمدرسة."

		// هنا تستدعي دالة الإرسال الخاصة بك:
		// go fcm.SendNotification(parentPhone, title, body)

		slog.Info("Absence recorded", "student", fullName, "phone", parentPhone, "title", title, "body", body)
	}
	if err := rows.Err(); err != nil {
		slog.Error("Error during absence rows iteration", "error", err)
	}
}
