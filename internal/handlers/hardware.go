package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"future_kids/internal/notify"

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
// ZKTeco firmware uses one of two line formats:
//
//   - Positional (old firmware, what most physical devices actually send):
//     PIN\tDateTime\tVerified\tStatus\t...
//     e.g. "1\t2026-09-18 06:03:37\t255\t4\t0\t0\t0\t0\t0\t0"
//
//   - Key=Value (newer firmware / cloud versions):
//     PIN=1001\tDateTime=2026-09-18 14:32:11\tVerified=1\tStatus=0
//
// The function detects the format automatically per line, extracts PIN and
// DateTime, and skips malformed lines with a Warn log explaining the reason.
// It never returns an error so the caller can always ACK the device with "OK".
func parseATTLOG(deviceSN, rawBody string) []AttendanceEvent {
	var events []AttendanceEvent

	lines := strings.Split(strings.TrimSpace(rawBody), "\n")

	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		fields := strings.Split(line, "\t")

		var pin, dateTimeStr string

		// ── Detect format: if first field contains '=' → key=value format ──
		if strings.ContainsRune(fields[0], '=') {
			// Key=Value format: PIN=x\tDateTime=y\t...
			kv := make(map[string]string)
			for _, field := range fields {
				idx := strings.IndexByte(field, '=')
				if idx < 0 {
					continue
				}
				kv[strings.TrimSpace(field[:idx])] = strings.TrimSpace(field[idx+1:])
			}
			var ok1, ok2 bool
			pin, ok1 = kv["PIN"]
			dateTimeStr, ok2 = kv["DateTime"]
			if !ok1 || !ok2 {
				slog.Warn("parseATTLOG: key=value line missing PIN or DateTime — SKIPPING",
					"line_index", i,
					"device_sn", deviceSN,
				)
				continue
			}
		} else {
			// Positional format: PIN\tDateTime\tVerified\tStatus\t...
			// Field[0] = PIN, Field[1] = "YYYY-MM-DD HH:MM:SS" (single tab-delimited field)
			if len(fields) < 2 {
				slog.Warn("parseATTLOG: positional line has fewer than 2 tab-fields — SKIPPING",
					"line_index", i,
					"device_sn", deviceSN,
					"field_count", len(fields),
				)
				continue
			}
			pin = strings.TrimSpace(fields[0])
			dateTimeStr = strings.TrimSpace(fields[1])
		}

		if pin == "" {
			slog.Warn("parseATTLOG: PIN is empty after extraction — SKIPPING",
				"line_index", i, "device_sn", deviceSN)
			continue
		}

		checkTime, err := time.Parse("2006-01-02 15:04:05", dateTimeStr)
		if err != nil {
			slog.Warn("parseATTLOG: time.Parse failed — SKIPPING",
				"line_index", i,
				"device_sn", deviceSN,
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
	_ = q.Get("c")
	_ = q.Get("Stamp")

	if deviceSN == "" {
		slog.Warn("ADMSHandler: missing SN — ACKing anyway")
		writeADMSOK(w)
		return
	}

	// ── Device authorization (Protocol-Safe) ──────────────────────────────────
	var isActive bool
	err := app.DB.QueryRowContext(r.Context(), "SELECT is_active FROM devices WHERE serial_number = $1", deviceSN).Scan(&isActive)

	if err == sql.ErrNoRows {
		slog.Warn("ADMSHandler: unregistered device attempted ADMS push", "device_sn", deviceSN, "remote_addr", r.RemoteAddr)
		writeADMSOK(w)
		return
	} else if err != nil {
		slog.Error("ADMSHandler: database error verifying device status", "device_sn", deviceSN, "error", err)
		writeADMSOK(w)
		return
	}

	if !isActive {
		slog.Warn("ADMSHandler: disabled device attempted ADMS push", "device_sn", deviceSN, "remote_addr", r.RemoteAddr)
		writeADMSOK(w)
		return
	}

	// Device is valid and active. Update last_sync asynchronously so we don't delay the ADMS response.
	go func(sn string) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("recovered from panic in ADMSHandler last_sync goroutine", "device_sn", sn, "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, updateErr := app.DB.ExecContext(ctx, "UPDATE devices SET last_sync = CURRENT_TIMESTAMP WHERE serial_number = $1", sn)
		if updateErr != nil {
			slog.Error("Failed to update device last_sync in ADMSHandler", "device_sn", sn, "error", updateErr)
		}
	}(deviceSN)

	// ── Table routing ─────────────────────────────────────────────────────────
	// Only ATTLOG contains attendance data. All other tables (OPERLOG, USER,
	// BLACKLIST, …) are ACKed immediately without parsing.
	if table != "ATTLOG" {
		slog.Info("[DEBUG] ADMSHandler: non-ATTLOG table — ACKing without parse",
			"device_sn", deviceSN,
			"table", table,
		)
		writeADMSOK(w)
		return
	}

	// ── Read raw body ──────────────────────────────────────────────────────────
	defer r.Body.Close()
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("ADMSHandler: io.ReadAll failed",
			"device_sn", deviceSN, "error", err)
		writeADMSOK(w)
		return
	}

	rawBody := string(bodyBytes)

	// ── Parse ──────────────────────────────────────────────────────────────────
	events := parseATTLOG(deviceSN, rawBody)

	if len(events) == 0 {
		slog.Warn("ADMSHandler: parser returned 0 events",
			"device_sn", deviceSN,
		)
		writeADMSOK(w)
		return
	}

	// ── Persist & notify ───────────────────────────────────────────────────────
	insertedCount := 0
	for _, ev := range events {
		// 1. Resolve true internal students.id from the ZKTeco PIN (rfid_tag)
		var internalStudentID int
		lookupErr := app.DB.QueryRowContext(
			r.Context(),
			"SELECT id FROM students WHERE rfid_tag = $1",
			ev.StudentID,
		).Scan(&internalStudentID)

		if lookupErr == sql.ErrNoRows {
			slog.Warn("ADMSHandler: Unknown device PIN received — skipping punch",
				"device_sn", ev.DeviceSN,
			)
			continue // Gracefully skip unmapped punches
		} else if lookupErr != nil {
			slog.Error("ADMSHandler: DB error during PIN lookup",
				"device_sn", ev.DeviceSN,
				"error", lookupErr,
			)
			continue
		}

		// 2. Override the event's StudentID with the true internal ID for safe insertion
		ev.StudentID = strconv.Itoa(internalStudentID)

		inserted, err := saveAttendanceLog(app.DB, ev)
		if err != nil {
			// Covers type mismatches and all other SQL errors.
			slog.Error("ADMSHandler: saveAttendanceLog FAILED",
				"device_sn", ev.DeviceSN,
				"error", err,
			)
			continue
		}
		if !inserted {
			continue
		}
		insertedCount++

		// Fetch student details for the push notification.
		studentIDInt, convErr := strconv.Atoi(ev.StudentID)
		if convErr != nil {
			slog.Warn("ADMSHandler: student_id is not a valid integer — cannot send notification",
				"error", convErr)
			continue
		}

		var studentName string
		var fcmToken sql.NullString
		var parentPhone sql.NullString

		notifyErr := app.DB.QueryRowContext(
			r.Context(),
			`SELECT s.full_name, s.fcm_token, p.phone_number 
			 FROM students s 
			 LEFT JOIN parents p ON s.parent_id = p.id 
			 WHERE s.id = $1`,
			studentIDInt,
		).Scan(&studentName, &fcmToken, &parentPhone)

		if notifyErr == sql.ErrNoRows {
			slog.Warn("ADMSHandler: student not found in DB for notification",
				"student_id_int", studentIDInt)
			continue
		} else if notifyErr != nil {
			slog.Error("ADMSHandler: error querying student for notification",
				"student_id", ev.StudentID, "error", notifyErr)
			continue
		}

		title := "إشعار حضور"
		body := fmt.Sprintf("تم تسجيل حضور الطالب %s بنجاح الساعة %s",
			studentName, ev.CheckTime.Format("15:04"))

		slog.Info("[DEBUG] ADMSHandler: sending notifications",
			"student_name", studentName,
			"has_fcm_token", fcmToken.Valid && fcmToken.String != "",
			"has_parent_phone", parentPhone.Valid && parentPhone.String != "",
		)

		if parentPhone.Valid && parentPhone.String != "" {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("Recovered panic in async goroutine: %v", r)
					}
				}()
				notify.SaveNotificationHistory(app.DB, parentPhone.String, title, body)
			}()
		}
		if fcmToken.Valid && fcmToken.String != "" {
			notify.SendPushNotification(app.FCMClient, fcmToken.String, title, body)
		}
	}

	log.Printf("ADMS [Device: %s]: Successfully processed %d attendance records", deviceSN, insertedCount)

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
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// حماية الذاكرة: رفض أي حمولة أكبر من 1 ميجابايت (يمنع هجمات DDoS من أجهزة مخترقة)
	r.Body = http.MaxBytesReader(w, r.Body, 1048576)

	var req HardwarePushPayload
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid payload")
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
		respondError(w, http.StatusInternalServerError, "Database error")
		return
	}

	// نتحقق مما إذا كان تم العثور على الطالب فعلاً
	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		// البصمة مكررة أو الـ RFID غير مسجل. في كلتا الحالتين نرد بنجاح للجهاز لكي لا يعلق
		respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "message": "Ignored or Duplicate"})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "message": "Punched successfully"})
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
