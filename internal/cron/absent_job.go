package cron

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"firebase.google.com/go/v4/messaging"
	"future_kids/internal/notify"
)

// ProcessDailyAbsences تُنفذ عند الساعة 12:00 ظهراً بتوقيت العراق
func ProcessDailyAbsences(db *sql.DB, fcmClient *messaging.Client) {
	// الاعتماد الصارم على توقيت بغداد
	loc, err := time.LoadLocation("Asia/Baghdad")
	if err != nil {
		log.Printf("Error loading timezone: %v", err)
		return
	}
	today := time.Now().In(loc).Format("2006-01-02")

	log.Printf("Starting daily absence processing for date: %s", today)

	// استعلام يستثني الحاضرين والمجازين معاً
	query := `
		SELECT s.id, s.full_name, p.phone_number, s.fcm_token 
		FROM students s
		LEFT JOIN parents p ON s.parent_id = p.id
		WHERE NOT EXISTS (
			SELECT 1 FROM attendance_logs al 
			WHERE al.student_id = s.id AND DATE(al.check_time) = $1
		) 
		AND NOT EXISTS (
			SELECT 1 FROM student_leaves sl 
			WHERE sl.student_id = s.id AND sl.leave_date = $1
		)
	`

	rows, err := db.Query(query, today)
	if err != nil {
		log.Printf("Failed to fetch absent students: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id int
		var fullName string
		var parentPhone sql.NullString
		var fcmToken sql.NullString // استخدام NullString لتجنب أعطال فلاتر إذا كان التوكن فارغاً

		if err := rows.Scan(&id, &fullName, &parentPhone, &fcmToken); err != nil {
			log.Printf("Row scan error: %v", err)
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

		log.Printf("Processed absence for student ID: %d, Name: %s", id, fullName)
	}

	if err = rows.Err(); err != nil {
		log.Printf("Rows iteration error: %v", err)
	}

	log.Println("Finished processing daily absences.")
}
