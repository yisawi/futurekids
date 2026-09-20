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
	"future_kids/internal/notify"
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
	slog.Info("[DEBUG] parseATTLOG: starting",
		"device_sn", deviceSN,
		"total_lines", len(lines),
	)

	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			slog.Info("[DEBUG] parseATTLOG: skipping empty line", "line_index", i)
			continue
		}

		slog.Info("[DEBUG] parseATTLOG: processing line",
			"line_index", i,
			"raw_line", line,
		)

		fields := strings.Split(line, "\t")
		slog.Info("[DEBUG] parseATTLOG: split result",
			"line_index", i,
			"field_count", len(fields),
			"fields", fields,
		)

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
				slog.Warn("[DEBUG] parseATTLOG: key=value line missing PIN or DateTime — SKIPPING",
					"line_index", i,
					"device_sn", deviceSN,
					"parsed_kv", kv,
					"raw_line", line,
				)
				continue
			}
			slog.Info("[DEBUG] parseATTLOG: detected key=value format",
				"line_index", i, "pin", pin, "datetime", dateTimeStr)
		} else {
			// Positional format: PIN\tDateTime\tVerified\tStatus\t...
			// Field[0] = PIN, Field[1] = "YYYY-MM-DD HH:MM:SS" (single tab-delimited field)
			if len(fields) < 2 {
				slog.Warn("[DEBUG] parseATTLOG: positional line has fewer than 2 tab-fields — SKIPPING",
					"line_index", i,
					"device_sn", deviceSN,
					"field_count", len(fields),
					"raw_line", line,
				)
				continue
			}
			pin = strings.TrimSpace(fields[0])
			dateTimeStr = strings.TrimSpace(fields[1])
			slog.Info("[DEBUG] parseATTLOG: detected positional format",
				"line_index", i, "pin", pin, "datetime", dateTimeStr)
		}

		if pin == "" {
			slog.Warn("[DEBUG] parseATTLOG: PIN is empty after extraction — SKIPPING",
				"line_index", i, "device_sn", deviceSN, "raw_line", line)
			continue
		}

		checkTime, err := time.Parse("2006-01-02 15:04:05", dateTimeStr)
		if err != nil {
			slog.Warn("[DEBUG] parseATTLOG: time.Parse failed — SKIPPING",
				"line_index", i,
				"device_sn", deviceSN,
				"pin", pin,
				"datetime_raw", dateTimeStr,
				"error", err,
			)
			continue
		}

		slog.Info("[DEBUG] parseATTLOG: parsed event OK",
			"line_index", i,
			"pin", pin,
			"check_time", checkTime,
		)
		events = append(events, AttendanceEvent{
			DeviceSN:  deviceSN,
			StudentID: pin,
			CheckTime: checkTime,
		})
	}

	slog.Info("[DEBUG] parseATTLOG: finished",
		"device_sn", deviceSN,
		"events_parsed", len(events),
	)
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
	cmd := q.Get("c")
	stamp := q.Get("Stamp")

	// ── [DEBUG] Dump all incoming query params ─────────────────────────────
	slog.Info("[DEBUG] ADMSHandler: incoming POST",
		"device_sn", deviceSN,
		"table", table,
		"command", cmd,
		"stamp", stamp,
		"raw_url", r.URL.String(),
		"remote_addr", r.RemoteAddr,
	)

	if deviceSN == "" {
		slog.Warn("[DEBUG] ADMSHandler: missing SN — ACKing anyway")
		writeADMSOK(w)
		return
	}

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
		slog.Error("[DEBUG] ADMSHandler: io.ReadAll failed",
			"device_sn", deviceSN, "error", err)
		writeADMSOK(w)
		return
	}

	rawBody := string(bodyBytes)
	slog.Info("[DEBUG] ADMSHandler: raw ATTLOG body",
		"device_sn", deviceSN,
		"body_bytes", len(bodyBytes),
		"raw_body", rawBody,
	)

	// ── Parse ──────────────────────────────────────────────────────────────────
	events := parseATTLOG(deviceSN, rawBody)
	slog.Info("[DEBUG] ADMSHandler: parseATTLOG result",
		"device_sn", deviceSN,
		"events_count", len(events),
	)

	if len(events) == 0 {
		slog.Warn("[DEBUG] ADMSHandler: parser returned 0 events — check [DEBUG] parseATTLOG logs above for skip reasons",
			"device_sn", deviceSN,
			"raw_body", rawBody,
		)
		writeADMSOK(w)
		return
	}

	// ── Persist & notify ───────────────────────────────────────────────────────
	insertedCount := 0
	for _, ev := range events {
		slog.Info("[DEBUG] ADMSHandler: attempting DB insert",
			"device_sn", ev.DeviceSN,
			"student_id", ev.StudentID,
			"check_time", ev.CheckTime,
		)

		inserted, err := saveAttendanceLog(app.DB, ev)
		if err != nil {
			// Covers FK violations (student_id not in students table),
			// type mismatches, and all other SQL errors.
			slog.Error("[DEBUG] ADMSHandler: saveAttendanceLog FAILED — possible FK violation if student_id not in students table",
				"device_sn", ev.DeviceSN,
				"student_id", ev.StudentID,
				"check_time", ev.CheckTime,
				"sql_error", err.Error(),
			)
			continue
		}
		if !inserted {
			slog.Info("[DEBUG] ADMSHandler: ON CONFLICT DO NOTHING triggered — duplicate record already exists",
				"device_sn", ev.DeviceSN,
				"student_id", ev.StudentID,
				"check_time", ev.CheckTime,
			)
			continue
		}
		insertedCount++
		slog.Info("[DEBUG] ADMSHandler: DB insert SUCCESS",
			"student_id", ev.StudentID,
			"check_time", ev.CheckTime,
		)

		// Fetch student details for the push notification.
		studentIDInt, convErr := strconv.Atoi(ev.StudentID)
		if convErr != nil {
			slog.Warn("[DEBUG] ADMSHandler: student_id is not a valid integer — cannot send notification",
				"student_id", ev.StudentID, "error", convErr)
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
			slog.Warn("[DEBUG] ADMSHandler: student not found in DB for notification",
				"student_id_int", studentIDInt)
			continue
		} else if notifyErr != nil {
			slog.Error("[DEBUG] ADMSHandler: error querying student for notification",
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
			go notify.SaveNotificationHistory(app.DB, parentPhone.String, title, body)
		}
		if fcmToken.Valid && fcmToken.String != "" {
			notify.SendPushNotification(app.FCMClient, fcmToken.String, title, body)
		}
	}

	slog.Info("[DEBUG] ADMSHandler: ATTLOG processing complete",
		"device_sn", deviceSN,
		"events_received", len(events),
		"events_inserted", insertedCount,
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


