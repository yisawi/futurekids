package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"firebase.google.com/go/v4/messaging"
)

type AppEnv struct {
	DB        *sql.DB
	FCMClient *messaging.Client // added to control notifications.
}

type AttendanceEvent struct {
	DeviceSN  string    // device Serial Number
	StudentID string    // student ID
	CheckTime time.Time // time
}

// parseATTLOG parses the ADMS ATTLOG payload sent by ZKTeco devices.
//
// Each line is tab-separated key=value pairs, for example:
//
//	PIN=1001\tDateTime=2026-09-18 14:32:11\tVerified=1\tStatus=0
//
// The function extracts PIN (→ StudentID) and DateTime, skips malformed lines,
// and never returns an error — bad lines are logged and skipped so the caller
// can always ACK the device with "OK".
func parseATTLOG(deviceSN, rawBody string) []AttendanceEvent {
	var events []AttendanceEvent

	lines := strings.Split(strings.TrimSpace(rawBody), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Build a key→value map from the tab-separated fields.
		kv := make(map[string]string)
		fields := strings.Split(line, "\t")
		for _, field := range fields {
			idx := strings.IndexByte(field, '=')
			if idx < 0 {
				continue // not a key=value pair, skip
			}
			key := strings.TrimSpace(field[:idx])
			val := strings.TrimSpace(field[idx+1:])
			kv[key] = val
		}

		pin, hasPIN := kv["PIN"]
		dateTimeStr, hasDateTime := kv["DateTime"]
		if !hasPIN || !hasDateTime {
			slog.Warn("ATTLOG line missing PIN or DateTime, skipping",
				"device_sn", deviceSN,
				"line", line,
			)
			continue
		}

		checkTime, err := time.Parse("2006-01-02 15:04:05", dateTimeStr)
		if err != nil {
			slog.Warn("ATTLOG invalid DateTime format, skipping",
				"device_sn", deviceSN,
				"pin", pin,
				"datetime", dateTimeStr,
				"error", err,
			)
			continue
		}

		events = append(events, AttendanceEvent{
			DeviceSN:  deviceSN,
			StudentID: pin,
			CheckTime: checkTime,
		})
	}

	return events
}

// ADMSHandler is the authoritative POST handler for /iclock/cdata.
//
// The device pushes different table types to the same endpoint. Only
// table=ATTLOG carries attendance records; everything else (OPERLOG, USER, etc.)
// is ACKed immediately so the device clears its buffer without a parse attempt.
//
// IMPORTANT: This handler MUST always respond with HTTP 200 plain-text "OK".
// Any other response causes the device to retry indefinitely.
func (app *AppEnv) ADMSHandler(w http.ResponseWriter, r *http.Request) {
	// The ADMS protocol only POSTs data; reject anything else.
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	deviceSN := q.Get("SN")
	table := q.Get("table")

	if deviceSN == "" {
		// Even with a missing SN we must respond OK so the device doesn't stall.
		slog.Warn("ADMS POST received without SN parameter")
		writeADMSOK(w)
		return
	}

	// ── Table routing ────────────────────────────────────────────────────────
	// Only ATTLOG contains attendance data. All other tables (OPERLOG, USER,
	// BLACKLIST, …) are ACKed immediately — we log them but do not parse.
	if table != "ATTLOG" {
		slog.Info("ADMS non-attendance table received, ACKing",
			"device_sn", deviceSN,
			"table", table,
		)
		writeADMSOK(w)
		return
	}

	// ── Read body ─────────────────────────────────────────────────────────────
	defer r.Body.Close()
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("ADMS: failed to read ATTLOG body", "device_sn", deviceSN, "error", err)
		// Still ACK so the device doesn't stall; we will miss this batch.
		writeADMSOK(w)
		return
	}

	// ── Parse ATTLOG lines ────────────────────────────────────────────────────
	events := parseATTLOG(deviceSN, string(bodyBytes))
	slog.Info("ADMS ATTLOG received",
		"device_sn", deviceSN,
		"lines_parsed", len(events),
	)

	// ── Persist & notify ──────────────────────────────────────────────────────
	insertedCount := 0
	for _, ev := range events {
		inserted, err := saveAttendanceLog(app.DB, ev)
		if err != nil {
			// Log but continue — one bad row must not block the others.
			slog.Error("Failed to save attendance log",
				"device_sn", deviceSN,
				"student_id", ev.StudentID,
				"error", err,
			)
			continue
		}
		if !inserted {
			// Duplicate — device re-sent a record we already have.
			slog.Info("Duplicate ATTLOG record, skipping",
				"device_sn", deviceSN,
				"student_id", ev.StudentID,
				"check_time", ev.CheckTime,
			)
			continue
		}
		insertedCount++

		// Fetch student details for the push notification.
		studentIDInt, _ := strconv.Atoi(ev.StudentID)
		var studentName string
		var fcmToken sql.NullString
		var parentPhone sql.NullString

		err = app.DB.QueryRowContext(
			r.Context(),
			"SELECT full_name, fcm_token, parent_phone FROM students WHERE id = $1",
			studentIDInt,
		).Scan(&studentName, &fcmToken, &parentPhone)

		if err == nil {
			title := "إشعار حضور"
			body := fmt.Sprintf("تم تسجيل حضور الطالب %s بنجاح الساعة %s",
				studentName, ev.CheckTime.Format("15:04"))

			if parentPhone.Valid && parentPhone.String != "" {
				go saveNotificationHistory(app.DB, parentPhone.String, title, body)
			}
			if fcmToken.Valid && fcmToken.String != "" {
				sendPushNotification(app.FCMClient, fcmToken.String, title, body)
			}
		} else if err != sql.ErrNoRows {
			slog.Error("Error fetching student for notification",
				"student_id", ev.StudentID,
				"error", err,
			)
		}
	}

	slog.Info("ADMS ATTLOG processed",
		"device_sn", deviceSN,
		"received", len(events),
		"inserted", insertedCount,
	)

	// Always ACK with plain-text OK — the device clears its buffer on receipt.
	writeADMSOK(w)
}

type HardwarePushPayload struct {
	DeviceSN string `json:"device_sn"`
	RFIDTag  string `json:"rfid_tag"`
	PushTime string `json:"push_time"` // صيغة: YYYY-MM-DD HH:MM:SS
}

func (app *AppEnv) HardwareAttendancePushHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"status":"error"}`, http.StatusMethodNotAllowed)
		return
	}

	// حماية الذاكرة: رفض أي حمولة أكبر من 1 ميجابايت (يمنع هجمات DDoS من أجهزة مخترقة)
	r.Body = http.MaxBytesReader(w, r.Body, 1048576)

	var req HardwarePushPayload
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"status":"error","message":"Invalid payload"}`, http.StatusBadRequest)
		return
	}

	// استعلام ذري: يبحث عن الطالب بالـ RFID ويدخل الحضور.
	// ON CONFLICT DO NOTHING يضمن عدم تكرار السجل إذا أعاد الجهاز الإرسال.
	query := `
		WITH student AS (
			SELECT id FROM students WHERE rfid_tag = $1 LIMIT 1
		)
		INSERT INTO attendance_logs (student_id, device_sn, check_time, status)
		SELECT id, $2, $3, 'Present' FROM student
		ON CONFLICT (student_id, check_time) DO NOTHING;
	`

	res, err := app.DB.ExecContext(r.Context(), query, req.RFIDTag, req.DeviceSN, req.PushTime)
	if err != nil {
		// في حال فشل قاعدة البيانات، نرد بخطأ 500 ليحتفظ الجهاز بالبصمة ويعيد إرسالها لاحقاً
		http.Error(w, `{"status":"error"}`, http.StatusInternalServerError)
		return
	}

	// نتحقق مما إذا كان تم العثور على الطالب فعلاً
	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		// البصمة مكررة أو الـ RFID غير مسجل. في كلتا الحالتين نرد بنجاح للجهاز لكي لا يعلق
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","message":"Ignored or Duplicate"}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"success","message":"Punched successfully"}`))
}

func saveNotificationHistory(db *sql.DB, phone, title, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := db.ExecContext(
		ctx,
		"INSERT INTO notifications (parent_phone, title, body) VALUES ($1, $2, $3)",
		phone,
		title,
		body,
	)
	if err != nil {
		slog.Error("Failed to save notification history", "error", err)
	}
}

// func to connect with PostegreSQL
func saveAttendanceLog(db *sql.DB, ev AttendanceEvent) (bool, error) {

	query := `
				  INSERT INTO attendance_logs (student_id, device_sn, check_time)
		VALUES ($1, $2, $3)
		ON CONFLICT (student_id, check_time) DO NOTHING;
	`
	// Using a short timeout to protect the server from database hangs
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result, err := db.ExecContext(ctx, query, ev.StudentID, ev.DeviceSN, ev.CheckTime)
	if err != nil {
		return false, err
	}

	rowsAffected, _ := result.RowsAffected()
	return rowsAffected > 0, nil

}

// Used for sending notification
func sendPushNotification(client *messaging.Client, token, title, body string) {
	if token == "" || client == nil {
		return // Skip sending if the student does not have a registered phone or the client is not configured.
	}

	msg := &messaging.Message{
		Token: token,
		Notification: &messaging.Notification{
			Title: title,
			Body:  body,
		},
	}

	// Send the notification in the background so as not to delay the server's response to the data device.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		response, err := client.Send(ctx, msg)
		if err != nil {
			slog.Error("Failed to send FCM message", "token", token, "error", err)
			return
		}
		slog.Info("Successfully sent FCM message", "response", response)
	}()
}
