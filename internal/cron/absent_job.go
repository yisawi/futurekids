package cron

import (
	"database/sql"
	"log"
	"time"
)

// ProcessDailyAbsences تُنفذ عند الساعة 12:00 ظهراً بتوقيت العراق
func ProcessDailyAbsences(db *sql.DB) {
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
		SELECT id, full_name, parent_phone, fcm_token 
		FROM students 
		WHERE id NOT IN (
			SELECT student_id FROM attendance_logs WHERE DATE(check_time) = $1
		) 
		AND id NOT IN (
			SELECT student_id FROM student_leaves WHERE leave_date = $1
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
		var fullName, parentPhone string
		var fcmToken sql.NullString // استخدام NullString لتجنب أعطال فلاتر إذا كان التوكن فارغاً

		if err := rows.Scan(&id, &fullName, &parentPhone, &fcmToken); err != nil {
			log.Printf("Row scan error: %v", err)
			continue
		}

		// هنا يتم استدعاء كود إرسال الإشعار الخاص بك (Firebase)
		// Example:
		// if fcmToken.Valid && fcmToken.String != "" {
		//     SendPushNotification(fcmToken.String, "إشعار غياب", "الطالب " + fullName + " غائب هذا اليوم.")
		// }

		log.Printf("Processed absence for student ID: %d, Name: %s", id, fullName)
	}

	if err = rows.Err(); err != nil {
		log.Printf("Rows iteration error: %v", err)
	}

	log.Println("Finished processing daily absences.")
}
