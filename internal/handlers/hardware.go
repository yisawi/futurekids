package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"future_kids/internal/notify"
	"future_kids/internal/ratelimit"

	"firebase.google.com/go/v4/messaging"
	"github.com/jackc/pgx/v5/pgconn"
)

type AppEnv struct {
	DB           *sql.DB
	FCMClient    *messaging.Client       // added to control notifications.
	LoginLimiter *ratelimit.LoginLimiter // failed parent-login attempts per phone number
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
// IMPORTANT: This handler responds with HTTP 200 plain-text "OK" in every case except one:
// a transient database failure (see writeADMSRetry), where 503 makes the device keep the
// batch and resend it. Any other non-200 response would make the device retry forever.
func (app *AppEnv) ADMSHandler(w http.ResponseWriter, r *http.Request) {
	// The ADMS protocol only POSTs data; reject anything else.
	if r.Method != http.MethodPost {
		// Defensive programming: Always return OK to ADMS hardware to prevent retry loops.
		slog.Warn("ADMSHandler: unexpected HTTP method — returning OK to prevent device retry loop",
			"method", r.Method,
			"remote_addr", r.RemoteAddr,
		)
		writeADMSOK(w)
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
	err := app.DB.QueryRowContext(r.Context(), "SELECT COALESCE(is_active, true) FROM devices WHERE serial_number = $1", deviceSN).Scan(&isActive)

	if err == sql.ErrNoRows {
		slog.Warn("ADMSHandler: unregistered device attempted ADMS push", "device_sn", deviceSN, "remote_addr", r.RemoteAddr)
		writeADMSOK(w)
		return
	} else if err != nil {
		if isTransientDBError(err) {
			slog.Error("ADMSHandler: database unavailable verifying device — replying 503 so the device resends", "device_sn", deviceSN, "error", err)
			writeADMSRetry(w)
			return
		}
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
		slog.Debug("ADMSHandler: non-ATTLOG table — ACKing without parse",
			"device_sn", deviceSN,
			"table", table,
		)
		writeADMSOK(w)
		return
	}

	// ── Read raw body ──────────────────────────────────────────────────────────
	defer r.Body.Close()
	bodyBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxADMSBodyBytes))
	if err != nil {
		// Oversized batches are ACKed (RULES.md §5): an error status would make the device resend them forever.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			slog.Warn("ADMSHandler: body exceeds limit — dropping batch and ACKing",
				"device_sn", deviceSN, "remote_addr", r.RemoteAddr, "limit_bytes", tooLarge.Limit)
			writeADMSOK(w)
			return
		}
		slog.Error("ADMSHandler: failed to read body — replying 503 so the device resends",
			"device_sn", deviceSN, "error", err)
		writeADMSRetry(w)
		return
	}

	rawBody := string(bodyBytes)

	// ── Parse ──────────────────────────────────────────────────────────────────
	events := parseATTLOG(deviceSN, rawBody)
	lines := 0
	for _, l := range strings.Split(rawBody, "\n") {
		if strings.TrimSpace(l) != "" {
			lines++
		}
	}

	if len(events) == 0 {
		slog.Warn("ADMSHandler: no valid records in batch — ACKing",
			"device_sn", deviceSN, "lines", lines,
		)
		writeADMSOK(w)
		return
	}

	// ── Persist (one transaction) ──────────────────────────────────────────────
	res := app.persistATTLOG(r.Context(), deviceSN, events)
	summary := []any{
		"device_sn", deviceSN,
		"lines", lines,
		"parsed", len(events),
		"malformed", lines - len(events),
		"unknown_pins", len(res.unknownPINs),
		"rejected", res.rejected,
	}
	if res.err != nil {
		slog.Error("ADMSHandler: batch rolled back — replying 503 so the device resends it",
			append(summary, "error", res.err)...)
		writeADMSRetry(w)
		return
	}
	if len(res.unknownPINs) > 0 {
		slog.Warn("ADMSHandler: punches for unknown device PINs skipped", "device_sn", deviceSN, "pins", res.unknownPINs)
	}
	slog.Info("ADMSHandler: batch stored",
		append(summary, "inserted", len(res.inserted), "duplicates", res.duplicates)...)

	// ── Notify (only for committed punches; failures never change the response) ──
	for _, p := range res.inserted {
		app.notifyPunch(r.Context(), p.studentID, p.checkTime)
	}

	// Always ACK with plain-text OK — the device clears its buffer on receipt.
	writeADMSOK(w)
}

// storedPunch is an attendance row committed by persistATTLOG.
type storedPunch struct {
	studentID int
	checkTime time.Time
}

// attlogResult summarizes one ATTLOG batch. When err is set the whole batch was rolled back.
type attlogResult struct {
	inserted    []storedPunch
	duplicates  int
	unknownPINs []string
	rejected    int
	err         error
}

// admsTxTimeout bounds how long one batch may hold its transaction.
const admsTxTimeout = 20 * time.Second

// persistATTLOG stores a batch in one transaction. Unknown PINs are skipped. The batch is
// inserted in one statement; if Postgres rejects it for a data reason, records are retried
// one by one and each rejected record is skipped and logged. Any transient database error
// rolls the whole batch back and is returned, so no partial batch is ever committed.
func (app *AppEnv) persistATTLOG(ctx context.Context, deviceSN string, events []AttendanceEvent) attlogResult {
	var res attlogResult
	ctx, cancel := context.WithTimeout(ctx, admsTxTimeout)
	defer cancel()

	tx, err := app.DB.BeginTx(ctx, nil)
	if err != nil {
		res.err = err
		return res
	}
	defer tx.Rollback()

	pins := make([]string, 0, len(events))
	seen := make(map[string]bool, len(events))
	for _, ev := range events {
		if !seen[ev.StudentID] {
			seen[ev.StudentID] = true
			pins = append(pins, ev.StudentID)
		}
	}
	ids := make(map[string]int, len(pins))
	rows, err := tx.QueryContext(ctx, `SELECT rfid_tag, id FROM students WHERE rfid_tag = ANY($1::text[])`, pins)
	if err != nil {
		res.err = err
		return res
	}
	for rows.Next() {
		var tag string
		var id int
		if err := rows.Scan(&tag, &id); err != nil {
			rows.Close()
			res.err = err
			return res
		}
		ids[tag] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		res.err = err
		return res
	}

	var batch []admsPunch
	for _, ev := range events {
		id, ok := ids[ev.StudentID]
		if !ok {
			if seen[ev.StudentID] {
				seen[ev.StudentID] = false
				res.unknownPINs = append(res.unknownPINs, ev.StudentID)
			}
			continue
		}
		batch = append(batch, admsPunch{pin: ev.StudentID, studentID: id, checkTime: ev.CheckTime})
	}

	inserted, err := insertPunchesBulk(ctx, tx, deviceSN, batch)
	rejected := 0
	if err != nil && !isTransientDBError(err) {
		slog.Warn("ADMSHandler: batch insert rejected — retrying record by record",
			"device_sn", deviceSN, "records", len(batch), "error", err)
		inserted, rejected, err = insertPunchesEach(ctx, tx, deviceSN, batch)
	}
	if err != nil {
		res.err = err
		return res
	}
	if err := tx.Commit(); err != nil {
		res.err = err
		return res
	}
	res.inserted = inserted
	res.rejected = rejected
	res.duplicates = len(batch) - len(inserted) - rejected
	return res
}

type admsPunch struct {
	pin       string
	studentID int
	checkTime time.Time
}

const insertPunchesSQL = `INSERT INTO attendance_logs (student_id, device_sn, check_time)
	SELECT u.student_id, $2, u.check_time FROM unnest($1::int[], $3::timestamp[]) AS u(student_id, check_time)
	ON CONFLICT (student_id, check_time) DO NOTHING
	RETURNING student_id, check_time`

// insertPunchesBulk inserts the batch in one statement inside a savepoint. On a non-transient
// error it rolls back to the savepoint so the caller can retry record by record.
func insertPunchesBulk(ctx context.Context, tx *sql.Tx, deviceSN string, batch []admsPunch) ([]storedPunch, error) {
	if len(batch) == 0 {
		return nil, nil
	}
	ids := make([]int, len(batch))
	times := make([]time.Time, len(batch))
	for i, p := range batch {
		ids[i], times[i] = p.studentID, p.checkTime
	}
	if _, err := tx.ExecContext(ctx, "SAVEPOINT attlog_bulk"); err != nil {
		return nil, err
	}
	stored, err := func() ([]storedPunch, error) {
		rows, err := tx.QueryContext(ctx, insertPunchesSQL, ids, deviceSN, times)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var stored []storedPunch
		for rows.Next() {
			var sp storedPunch
			if err := rows.Scan(&sp.studentID, &sp.checkTime); err != nil {
				return nil, err
			}
			stored = append(stored, sp)
		}
		return stored, rows.Err()
	}()
	if err != nil {
		if !isTransientDBError(err) {
			if _, rbErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT attlog_bulk"); rbErr != nil {
				return nil, rbErr
			}
		}
		return nil, err
	}
	return stored, nil
}

// insertPunchesEach inserts records one by one, each in its own savepoint. A record rejected
// for a data reason is rolled back, logged and skipped; a transient error aborts the batch.
func insertPunchesEach(ctx context.Context, tx *sql.Tx, deviceSN string, batch []admsPunch) ([]storedPunch, int, error) {
	var stored []storedPunch
	rejected := 0
	for i, p := range batch {
		if _, err := tx.ExecContext(ctx, "SAVEPOINT attlog_row"); err != nil {
			return nil, rejected, err
		}
		var sp storedPunch
		err := tx.QueryRowContext(ctx, `INSERT INTO attendance_logs (student_id, device_sn, check_time)
			VALUES ($1, $2, $3)
			ON CONFLICT (student_id, check_time) DO NOTHING
			RETURNING student_id, check_time`, p.studentID, deviceSN, p.checkTime).Scan(&sp.studentID, &sp.checkTime)
		switch {
		case err == nil:
			stored = append(stored, sp)
		case errors.Is(err, sql.ErrNoRows):
		case isTransientDBError(err):
			return nil, rejected, err
		default:
			if _, rbErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT attlog_row"); rbErr != nil {
				return nil, rejected, rbErr
			}
			rejected++
			slog.Error("ADMSHandler: punch rejected by the database — skipped",
				"device_sn", deviceSN, "record", i+1, "records", len(batch),
				"pin", p.pin, "check_time", p.checkTime.Format("2006-01-02 15:04:05"), "error", err)
			continue
		}
		if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT attlog_row"); err != nil {
			return nil, rejected, err
		}
	}
	return stored, rejected, nil
}

// isTransientDBError reports whether err is a temporary database condition worth retrying:
// connection or driver failures, timeouts, shutdowns, resource exhaustion, deadlocks and
// serialization or lock-wait failures. Data and constraint errors are not transient.
func isTransientDBError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return true
	}
	switch pgErr.Code[:2] {
	case "08", "53", "57", "58":
		return true
	}
	switch pgErr.Code {
	case "40001", "40P01", "55P03":
		return true
	}
	return false
}

// notifyPunch sends the check-in or check-out notification for a committed punch when it is
// the first punch of its window. Failures are logged and never affect the device response.
func (app *AppEnv) notifyPunch(ctx context.Context, studentID int, checkTime time.Time) {
	// Notify only if this punch is the one get_student_status selected for its window
	// (first check-in or first check-out). Spam, dead-zone, and same-minute duplicate
	// punches are stored but never notified (RULES.md §5). first_check/last_check are
	// minute-precision text, so an earlier punch in the same minute also counts as a duplicate.
	var isCheckIn, isCheckOut bool
	statusErr := app.DB.QueryRowContext(
		ctx,
		`SELECT
			COALESCE(st.first_check = TO_CHAR($2::timestamp, 'HH12:MI AM'), false),
			COALESCE(st.last_check = TO_CHAR($2::timestamp, 'HH12:MI AM'), false)
		 FROM get_student_status($1, $2::timestamp::date) st
		 WHERE NOT EXISTS (
			SELECT 1 FROM attendance_logs
			WHERE student_id = $1 AND check_time < $2::timestamp
			AND date_trunc('minute', check_time) = date_trunc('minute', $2::timestamp)
		 )`,
		studentID, checkTime,
	).Scan(&isCheckIn, &isCheckOut)
	if statusErr != nil && statusErr != sql.ErrNoRows {
		slog.Error("ADMSHandler: notification skipped — window status query failed",
			"student_id", studentID, "error", statusErr)
		return
	}
	if !isCheckIn && !isCheckOut {
		return
	}

	var studentName string
	var fcmToken sql.NullString
	var parentPhone sql.NullString
	notifyErr := app.DB.QueryRowContext(
		ctx,
		`SELECT s.full_name, s.fcm_token, p.phone_number
		 FROM students s
		 LEFT JOIN parents p ON s.parent_id = p.id
		 WHERE s.id = $1`,
		studentID,
	).Scan(&studentName, &fcmToken, &parentPhone)
	if notifyErr != nil {
		slog.Error("ADMSHandler: notification skipped — student query failed",
			"student_id", studentID, "error", notifyErr)
		return
	}

	title := "إشعار دخول"
	body := fmt.Sprintf("تم تسجيل دخول الطالب %s الساعة %s",
		studentName, checkTime.Format("15:04"))
	if isCheckOut {
		title = "إشعار خروج"
		body = fmt.Sprintf("تم تسجيل خروج الطالب %s الساعة %s",
			studentName, checkTime.Format("15:04"))
	}

	slog.Debug("ADMSHandler: sending notifications",
		"student_id", studentID,
		"has_fcm_token", fcmToken.Valid && fcmToken.String != "",
		"has_parent_phone", parentPhone.Valid && parentPhone.String != "",
	)

	if parentPhone.Valid && parentPhone.String != "" {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("ADMSHandler: recovered panic in notification history goroutine", "panic", r)
				}
			}()
			notify.SaveNotificationHistory(app.DB, parentPhone.String, title, body)
		}()
	}
	if fcmToken.Valid && fcmToken.String != "" {
		notify.SendPushNotification(app.FCMClient, fcmToken.String, title, body)
	}
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

	// The only trusted device identity is the one DeviceAuthMiddleware verified.
	deviceSN, ok := r.Context().Value(DeviceSNKey).(string)
	if !ok || deviceSN == "" {
		slog.Error("HardwareAttendancePushHandler: no verified device identity (DeviceAuthMiddleware not applied)", "path", r.URL.Path)
		respondError(w, http.StatusInternalServerError, "Internal server error")
		return
	}

	var req HardwarePushPayload
	if !decodeJSONBody(w, r, &req, "Invalid payload") {
		return
	}
	req.DeviceSN = strings.TrimSpace(req.DeviceSN)
	req.RFIDTag = strings.TrimSpace(req.RFIDTag)

	if req.DeviceSN == "" {
		respondError(w, http.StatusBadRequest, "device_sn is required")
		return
	}
	if req.DeviceSN != deviceSN {
		slog.Warn("HardwareAttendancePushHandler: device_sn does not match the authenticated device — possible spoofing",
			"authenticated_sn", deviceSN, "claimed_sn", req.DeviceSN, "remote_addr", r.RemoteAddr)
		respondError(w, http.StatusForbidden, "device_sn does not match the authenticated device")
		return
	}
	if req.RFIDTag == "" {
		respondError(w, http.StatusBadRequest, "rfid_tag is required")
		return
	}
	pushTime, err := time.Parse("2006-01-02 15:04:05", strings.TrimSpace(req.PushTime))
	if err != nil {
		respondError(w, http.StatusBadRequest, "push_time must be formatted as YYYY-MM-DD HH:MM:SS")
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

	res, err := app.DB.ExecContext(r.Context(), query, req.RFIDTag, deviceSN, pushTime)
	if err != nil {
		if isTransientDBError(err) {
			// 503 tells the device to keep the punch and resend it once the database recovers.
			respondRetry(w, "HardwareAttendancePushHandler: database unavailable", err, "device_sn", deviceSN, "rfid_tag", req.RFIDTag)
			return
		}
		respondInternalError(w, "Database error", "HardwareAttendancePushHandler: exec failed", err, "device_sn", deviceSN, "rfid_tag", req.RFIDTag)
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
