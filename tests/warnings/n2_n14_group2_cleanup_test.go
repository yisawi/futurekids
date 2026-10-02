package warnings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"future_kids/internal/auth"
	"future_kids/internal/config"
	"future_kids/internal/handlers"
	"future_kids/internal/notify"

	firebase "firebase.google.com/go/v4"
	"github.com/golang-jwt/jwt/v5"
	excelize "github.com/xuri/excelize/v2"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/api/option"
)

// g2FakeFCM answers FCM send requests by token: "dead-*" → UNREGISTERED, "bad-*" →
// INVALID_ARGUMENT, anything else → success.
func g2FakeFCM(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Message struct {
				Token string `json:"token"`
			} `json:"message"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(req.Message.Token, "dead-"):
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":404,"message":"Requested entity was not found.","status":"NOT_FOUND","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`)
		case strings.HasPrefix(req.Message.Token, "bad-"):
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"code":400,"message":"The registration token is not a valid FCM registration token","status":"INVALID_ARGUMENT"}}`)
		default:
			io.WriteString(w, `{"name":"projects/fk-test/messages/1"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGroup2Cleanup verifies notices N2, N3, N4, N5, N8, N9, N11, N13 and N14.
func TestGroup2Cleanup(t *testing.T) {
	capture := &w10Capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	root := filepath.Join("..", "..")
	db, dsn := setupThrowawayDB(t, "g2")
	auth.InitAuth("g2-test-secret")
	adminToken, _ := auth.GenerateAdminToken("admin", 0)
	app := &handlers.AppEnv{DB: db}

	exec_ := func(t *testing.T, qs ...string) {
		t.Helper()
		for _, q := range qs {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("exec failed: %v\nquery: %s", err, q)
			}
		}
	}
	call := func(t *testing.T, h http.HandlerFunc, method, target, token, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}
	readSrc := func(t *testing.T, rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	buildBin := func(t *testing.T, pkg string) string {
		t.Helper()
		out := filepath.Join(t.TempDir(), filepath.Base(pkg))
		cmd := exec.Command("go", "build", "-o", out, "./"+pkg)
		cmd.Dir = root
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, b)
		}
		return out
	}
	cleanEnv := func(extra ...string) []string {
		return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, extra...)
	}

	t.Run("N2/Firebase credentials path comes from config", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://u@localhost/app")
		for _, c := range []struct{ env, want string }{
			{"", config.DefaultFirebaseCredentialsPath},
			{"   ", config.DefaultFirebaseCredentialsPath},
			{"/etc/fk/firebase.json", "/etc/fk/firebase.json"},
		} {
			t.Setenv("FIREBASE_CREDENTIALS_PATH", c.env)
			t.Setenv("FIREBASE_CREDENTIALS_JSON", `{"type":"service_account"}`)
			cfg, err := config.LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.FirebaseCredentialsPath != c.want || cfg.FirebaseCredentialsJSON != `{"type":"service_account"}` {
				t.Errorf("FIREBASE_CREDENTIALS_PATH=%q → path %q json %q, want path %q", c.env, cfg.FirebaseCredentialsPath, cfg.FirebaseCredentialsJSON, c.want)
			}
		}
		main := readSrc(t, "cmd/api/main.go")
		if strings.Contains(main, "os.Getenv(") || strings.Contains(main, `"firebase-credentials.json"`) {
			t.Error("cmd/api/main.go still reads Firebase settings itself instead of from config")
		}

		bin := buildBin(t, "cmd/api")
		missing := "/nonexistent/fk-g2/firebase.json"
		cmd := exec.Command(bin)
		cmd.Dir = t.TempDir()
		cmd.Env = cleanEnv("DATABASE_URL="+dsn, "JWT_SECRET=g2", "PORT=0", "FIREBASE_CREDENTIALS_PATH="+missing)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), missing) || !strings.Contains(string(out), "No Firebase credentials") {
			t.Errorf("server with FIREBASE_CREDENTIALS_PATH=%s: err=%v, want exit naming that path; output:\n%s", missing, err, out)
		}
	})

	t.Run("N3/migrate-pins uses pgx and hashes PINs", func(t *testing.T) {
		gomod, gosum := readSrc(t, "go.mod"), readSrc(t, "go.sum")
		if strings.Contains(gomod, "lib/pq") || strings.Contains(gosum, "lib/pq") {
			t.Error("lib/pq is still a dependency")
		}
		if src := readSrc(t, "cmd/migrate-pins/main.go"); strings.Contains(src, "lib/pq") || !strings.Contains(src, `"pgx"`) {
			t.Error("cmd/migrate-pins does not use the pgx driver")
		}

		existing, _ := bcrypt.GenerateFromPassword([]byte("2468"), bcrypt.MinCost)
		exec_(t, `INSERT INTO parents (full_name, phone_number, pin_code) VALUES ('Plain A', '+9647000000701', '1234'), ('Plain B', '+9647000000702', '98765'), ('Hashed', '+9647000000703', '`+string(existing)+`')`)
		bin := buildBin(t, "cmd/migrate-pins")
		run := func() string {
			cmd := exec.Command(bin)
			cmd.Env = cleanEnv("DATABASE_URL=" + dsn)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("migrate-pins failed: %v\n%s", err, out)
			}
			return string(out)
		}
		first := run()
		pins := map[string]string{}
		rows, _ := db.Query(`SELECT phone_number, pin_code FROM parents WHERE phone_number LIKE '+96470000007%'`)
		for rows.Next() {
			var phone, pin string
			rows.Scan(&phone, &pin)
			pins[phone] = pin
		}
		rows.Close()
		for phone, plain := range map[string]string{"+9647000000701": "1234", "+9647000000702": "98765", "+9647000000703": "2468"} {
			if bcrypt.CompareHashAndPassword([]byte(pins[phone]), []byte(plain)) != nil {
				t.Errorf("%s: stored PIN no longer verifies against %q", phone, plain)
			}
		}
		if pins["+9647000000703"] != string(existing) {
			t.Error("an already-hashed PIN was rehashed")
		}
		if !strings.Contains(first, "Successfully migrated: 2") {
			t.Errorf("first run should migrate 2 PINs:\n%s", first)
		}
		if second := run(); !strings.Contains(second, "Successfully migrated: 0") {
			t.Errorf("second run should migrate nothing:\n%s", second)
		}
	})

	exec_(t,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('G2-DEV', 'Gate', true)`,
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (901, 'G2 Parent', '+9647000000901', 'h')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id, grade, section) VALUES (901, 'G2 Kid', 'G2-TAG-1', 901, 'G1', 'A'), (902, 'G2 Orphan', 'G2-TAG-2', NULL, NULL, NULL)`,
	)

	t.Run("N4/attendance_logs.status is gone and punches still store", func(t *testing.T) {
		if n := countRows(t, db, `SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'attendance_logs' AND column_name = 'status'`); n != 0 {
			t.Error("attendance_logs.status still exists after migration 000019")
		}
		rec := call(t, app.DeviceAuthMiddleware(app.HardwareAttendancePushHandler), "POST", "/api/attendance/push/json?SN=G2-DEV", "",
			`{"device_sn":"G2-DEV","rfid_tag":"G2-TAG-2","push_time":"2026-04-01 07:15:00"}`)
		if rec.Code != http.StatusOK {
			t.Errorf("JSON push: HTTP %d %s", rec.Code, rec.Body.String())
		}
		rec = call(t, app.ADMSHandler, "POST", "/iclock/cdata?SN=G2-DEV&table=ATTLOG", "", "G2-TAG-2\t2026-04-01 12:05:00\t1\t1\n")
		if rec.Code != http.StatusOK || countRows(t, db, `SELECT COUNT(*) FROM attendance_logs WHERE student_id = 902`) != 2 {
			t.Errorf("ADMS push: HTTP %d; punches stored = %d, want 2", rec.Code, countRows(t, db, `SELECT COUNT(*) FROM attendance_logs WHERE student_id = 902`))
		}
		for _, f := range []string{"internal/handlers/hardware.go", "internal/handlers/admin.go", "internal/handlers/mobile.go", "internal/cron/absent_job.go"} {
			if regexp.MustCompile(`check_time, status\)|\bstatus\) VALUES|a\.status\b|'Present' FROM`).MatchString(readSrc(t, f)) {
				t.Errorf("%s still reads or writes attendance_logs.status", f)
			}
		}
	})

	t.Run("N5/GET /iclock/cdata is a heartbeat; POST goes to ADMSHandler", func(t *testing.T) {
		rec := call(t, handlers.ADMSCdataHandler, "GET", "/iclock/cdata?SN=G2-DEV&options=all", "", "")
		if rec.Code != http.StatusOK || rec.Body.String() != "OK" || rec.Header().Get("Content-Type") != "text/plain" {
			t.Errorf("GET heartbeat: HTTP %d %q", rec.Code, rec.Body.String())
		}
		src := readSrc(t, "internal/handlers/zkteco_handler.go")
		if strings.Contains(src, "MethodPost") || strings.Contains(src, "io.ReadAll") {
			t.Error("ADMSCdataHandler still carries the dead POST branch")
		}
		main := readSrc(t, "cmd/api/main.go")
		if !strings.Contains(main, `mux.HandleFunc("GET /iclock/cdata", handlers.ADMSCdataHandler)`) ||
			!strings.Contains(main, `mux.HandleFunc("POST /iclock/cdata", appEnv.ADMSHandler)`) {
			t.Error("main.go must route GET /iclock/cdata to ADMSCdataHandler and POST to ADMSHandler")
		}
	})

	t.Run("N8/unregistered FCM tokens are deleted and never logged raw", func(t *testing.T) {
		fcm := g2FakeFCM(t)
		fbApp, err := firebase.NewApp(context.Background(), &firebase.Config{ProjectID: "fk-test"}, option.WithEndpoint(fcm.URL), option.WithoutAuthentication())
		if err != nil {
			t.Fatal(err)
		}
		client, err := fbApp.Messaging(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		exec_(t, `INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (911, 'Token Parent', '+9647000000911', 'x'), (912, 'Punch Parent', '+9647000000912', 'x')`)
		exec_(t, `INSERT INTO students (id, full_name, rfid_tag, parent_id) VALUES (911, 'Sib 1', 'G2-FCM-1', 911), (912, 'Sib 2', 'G2-FCM-2', 911), (915, 'Via punch', 'G2-FCM-5', 912)`)
		exec_(t, `INSERT INTO device_tokens (parent_id, token) VALUES (911, 'dead-token-shared'), (911, 'live-token-1'), (911, 'bad-token-1'), (912, 'dead-token-punch')`)
		before := len(capture.snapshot())
		for _, tok := range []string{"dead-token-shared", "live-token-1", "bad-token-1"} {
			notify.SendPushNotification(nil, client, db, tok, "t", "b")
		}
		appWithFCM := &handlers.AppEnv{DB: db, FCMClient: client}
		call(t, appWithFCM.ADMSHandler, "POST", "/iclock/cdata?SN=G2-DEV&table=ATTLOG", "", "G2-FCM-5\t2026-04-02 07:15:00\t1\t1\n")

		stored := func(tok string) bool {
			var n int
			db.QueryRow(`SELECT COUNT(*) FROM device_tokens WHERE token = $1`, tok).Scan(&n)
			return n == 1
		}
		deadline := time.Now().Add(5 * time.Second)
		for (stored("dead-token-shared") || stored("dead-token-punch")) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		for tok, want := range map[string]bool{"dead-token-shared": false, "dead-token-punch": false, "live-token-1": true, "bad-token-1": true} {
			if got := stored(tok); got != want {
				t.Errorf("device token %s stored = %v, want %v", notify.TokenFingerprint(tok), got, want)
			}
		}
		var studentTokens int
		db.QueryRow(`SELECT COUNT(*) FROM students WHERE fcm_token IS NOT NULL`).Scan(&studentTokens)
		if studentTokens != 0 {
			t.Errorf("students.fcm_token written: %d rows", studentTokens)
		}
		logs := capture.snapshot()[before:]
		var clearedShared, badLogged, liveLogged bool
		for _, l := range logs {
			if strings.Contains(l.Msg+fmt.Sprint(l.Attrs), "-token-") {
				t.Errorf("raw FCM token in log: %s %v", l.Msg, l.Attrs)
			}
			if l.Level == slog.LevelWarn && strings.HasPrefix(l.Msg, "FCM token is no longer registered") && l.Attrs["token"] == notify.TokenFingerprint("dead-token-shared") && l.Attrs["devices"] == "1" {
				clearedShared = true
			}
			if l.Level == slog.LevelError && l.Msg == "Failed to send FCM message" && l.Attrs["token"] == notify.TokenFingerprint("bad-token-1") {
				badLogged = true
			}
			if l.Msg == "Sent FCM message" && l.Attrs["token"] == notify.TokenFingerprint("live-token-1") {
				liveLogged = true
			}
		}
		if !clearedShared || !badLogged || !liveLogged {
			t.Errorf("log events: deleted-dead=%v invalid-argument-error=%v sent=%v, want all true (logs %v)", clearedShared, badLogged, liveLogged, logs)
		}
		fp := notify.TokenFingerprint("dead-token-shared")
		if fp != notify.TokenFingerprint("dead-token-shared") || strings.Contains(fp, "dead") || len(fp) != len("fcm:")+12 {
			t.Errorf("fingerprint %q is not a stable, opaque identifier", fp)
		}
	})

	t.Run("N9/school name from settings, parentless students included, check-out exported", func(t *testing.T) {
		rec := call(t, app.AdminMiddleware(app.AdminStudentsHandler), "GET", "/api/admin/students", adminToken, "")
		var list struct {
			Data []map[string]any `json:"data"`
		}
		json.Unmarshal(rec.Body.Bytes(), &list)
		found := false
		for _, s := range list.Data {
			if s["name"] == "G2 Orphan" {
				found = s["parent_name"] == "" && s["parent_phone"] == ""
			}
		}
		if !found {
			t.Error("student without a parent is missing from GET /api/admin/students (or has non-empty parent fields)")
		}

		excel := func(t *testing.T) (*excelize.File, int) {
			t.Helper()
			rec := call(t, app.AdminMiddleware(app.AdminExportExcelHandler), "GET", "/api/admin/export/excel?date=2026-04-01", adminToken, "")
			if rec.Code != http.StatusOK {
				return nil, rec.Code
			}
			f, err := excelize.OpenReader(bytes.NewReader(rec.Body.Bytes()))
			if err != nil {
				t.Fatalf("open xlsx: %v", err)
			}
			return f, rec.Code
		}
		f, _ := excel(t)
		if a2, _ := f.GetCellValue("Sheet1", "A2"); a2 != "مدرسة الرحمن الابتدائية الأهلية" {
			t.Errorf("A2 = %q, want the seeded school_name", a2)
		}
		if h, _ := f.GetCellValue("Sheet1", "I5"); h != "وقت الخروج" {
			t.Errorf("I5 = %q, want the check-out column header", h)
		}
		orphanRow := 0
		rows, _ := f.GetRows("Sheet1")
		for i, r := range rows {
			if len(r) > 1 && r[1] == "G2 Orphan" {
				orphanRow = i + 1
			}
		}
		if orphanRow == 0 {
			t.Fatal("student without a parent is missing from the Excel export")
		}
		parent, _ := f.GetCellValue("Sheet1", fmt.Sprintf("E%d", orphanRow))
		in, _ := f.GetCellValue("Sheet1", fmt.Sprintf("H%d", orphanRow))
		out, _ := f.GetCellValue("Sheet1", fmt.Sprintf("I%d", orphanRow))
		if parent != "-" || in != "07:15 AM" || out != "12:05 PM" {
			t.Errorf("orphan row: parent=%q in=%q out=%q, want '-', 07:15 AM, 12:05 PM", parent, in, out)
		}

		call(t, app.AdminMiddleware(app.AdminSettingsHandler), "PUT", "/api/admin/settings", adminToken, `{"key":"school_name","value":"مدرسة المستقبل"}`)
		f, _ = excel(t)
		if a2, _ := f.GetCellValue("Sheet1", "A2"); a2 != "مدرسة المستقبل" {
			t.Errorf("after editing school_name, A2 = %q", a2)
		}
		exec_(t, `DELETE FROM settings WHERE setting_key = 'school_name'`)
		f, code := excel(t)
		if code != http.StatusOK {
			t.Fatalf("export without school_name: HTTP %d", code)
		}
		if a2, _ := f.GetCellValue("Sheet1", "A2"); a2 != "" {
			t.Errorf("without school_name, A2 = %q, want blank", a2)
		}
		exec_(t, `INSERT INTO settings (setting_key, setting_value) VALUES ('school_name', 'مدرسة الرحمن الابتدائية الأهلية')`)

		parentToken, _ := auth.GenerateParentToken(901, "+9647000000901", 0)
		exec_(t, `INSERT INTO attendance_logs (student_id, device_sn, check_time) VALUES (901, 'G2-DEV', '2026-04-01 07:20:00'), (901, 'G2-DEV', '2026-04-01 12:10:00')`)
		rec = call(t, app.AuthMiddleware(app.MobileMonthlyAttendanceHandler), "GET", "/api/mobile/attendance/monthly?month=2026-04", parentToken, "")
		var monthly struct {
			Data []struct {
				Records []map[string]any `json:"records"`
			} `json:"data"`
		}
		json.Unmarshal(rec.Body.Bytes(), &monthly)
		ok := false
		for _, s := range monthly.Data {
			for _, r := range s.Records {
				if _, has := r["check_out_time"]; !has {
					t.Fatalf("monthly record without check_out_time key: %v", r)
				}
				if r["date"] == "2026-04-01" && r["check_time"] == "07:20 AM" && r["check_out_time"] == "12:10 PM" {
					ok = true
				}
			}
		}
		if !ok {
			t.Errorf("monthly records lack the 2026-04-01 check-in/check-out pair (HTTP %d)", rec.Code)
		}
	})

	t.Run("N11/malformed date and month parameters return 400", func(t *testing.T) {
		parentToken, _ := auth.GenerateParentToken(901, "+9647000000901", 0)
		admin, parent := app.AdminMiddleware, app.AuthMiddleware
		cases := []struct {
			name, method, target, token, body, msg string
			h                                      http.HandlerFunc
		}{
			{"daily bad date", "GET", "/api/admin/attendance?date=2026-02-30", adminToken, "", "date must be formatted as YYYY-MM-DD", admin(app.AdminDailyAttendanceHandler)},
			{"daily garbage date", "GET", "/api/admin/attendance?date=yesterday", adminToken, "", "date must be formatted as YYYY-MM-DD", admin(app.AdminDailyAttendanceHandler)},
			{"excel bad date", "GET", "/api/admin/export/excel?date=2026-4-1", adminToken, "", "date must be formatted as YYYY-MM-DD", admin(app.AdminExportExcelHandler)},
			{"summary month 13", "GET", "/api/mobile/attendance/summary?month=2026-13", parentToken, "", "month must be formatted as YYYY-MM", parent(app.MobileAttendanceSummaryHandler)},
			{"summary unpadded month", "GET", "/api/mobile/attendance/summary?month=2026-9", parentToken, "", "month must be formatted as YYYY-MM", parent(app.MobileAttendanceSummaryHandler)},
			{"monthly garbage", "GET", "/api/mobile/attendance/monthly?month=99", parentToken, "", "month must be formatted as YYYY-MM", parent(app.MobileMonthlyAttendanceHandler)},
			{"leave bad date", "POST", "/api/admin/leaves", adminToken, `{"student_id":901,"leave_date":"01/04/2026"}`, "leave_date must be formatted as YYYY-MM-DD", admin(app.AdminCreateLeaveHandler)},
		}
		for _, c := range cases {
			rec := call(t, c.h, c.method, c.target, c.token, c.body)
			var resp map[string]string
			json.Unmarshal(rec.Body.Bytes(), &resp)
			if rec.Code != http.StatusBadRequest || resp["message"] != c.msg {
				t.Errorf("%s: HTTP %d %q, want 400 %q", c.name, rec.Code, resp["message"], c.msg)
			}
		}
		for _, c := range []struct {
			target string
			h      http.HandlerFunc
			token  string
		}{
			{"/api/admin/attendance?date=2026-04-01", admin(app.AdminDailyAttendanceHandler), adminToken},
			{"/api/admin/attendance", admin(app.AdminDailyAttendanceHandler), adminToken},
			{"/api/mobile/attendance/summary?month=2026-04", parent(app.MobileAttendanceSummaryHandler), parentToken},
			{"/api/mobile/attendance/monthly", parent(app.MobileMonthlyAttendanceHandler), parentToken},
		} {
			if rec := call(t, c.h, "GET", c.target, c.token, ""); rec.Code != http.StatusOK {
				t.Errorf("valid %s: HTTP %d", c.target, rec.Code)
			}
		}
		if rec := call(t, admin(app.AdminCreateLeaveHandler), "POST", "/api/admin/leaves", adminToken, `{"student_id":901,"leave_date":"2026-04-05"}`); rec.Code != http.StatusOK {
			t.Errorf("valid leave: HTTP %d %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(readSrc(t, "internal/handlers/mobile.go"), "TotalAbsent < 0") {
			t.Error("impossible TotalAbsent < 0 check is still present")
		}
	})

	t.Run("N13/token lifetimes match the documented constants", func(t *testing.T) {
		if auth.AdminTokenTTL != 7*24*time.Hour || auth.ParentTokenTTL != 30*24*time.Hour {
			t.Errorf("TTLs admin=%v parent=%v", auth.AdminTokenTTL, auth.ParentTokenTTL)
		}
		parentToken, _ := auth.GenerateParentToken(1, "x", 0)
		for name, c := range map[string]struct {
			token string
			ttl   time.Duration
		}{"admin": {adminToken, auth.AdminTokenTTL}, "parent": {parentToken, auth.ParentTokenTTL}} {
			claims := jwt.MapClaims{}
			if _, _, err := jwt.NewParser().ParseUnverified(c.token, claims); err != nil {
				t.Fatal(err)
			}
			exp, _ := claims.GetExpirationTime()
			iat, _ := claims.GetIssuedAt()
			if got := exp.Sub(iat.Time); got != c.ttl {
				t.Errorf("%s token lifetime = %v, want %v", name, got, c.ttl)
			}
		}
		src := readSrc(t, "internal/auth/jwt.go")
		if strings.Contains(src, "short-lived") || !strings.Contains(src, "AdminTokenTTL, 7 days") || !strings.Contains(src, "ParentTokenTTL, 30 days") {
			t.Error("jwt.go comments do not match the token lifetimes")
		}
	})

	t.Run("N14/public settings endpoint returns only allow-listed keys", func(t *testing.T) {
		exec_(t, `INSERT INTO settings (setting_key, setting_value) VALUES ('internal_api_key', 'secret-value'), ('admin_threshold', '3') ON CONFLICT (setting_key) DO NOTHING`)
		rec := call(t, app.MobileSettingsHandler, "GET", "/api/mobile/settings", "", "")
		var pub struct {
			Data map[string]string `json:"data"`
		}
		json.Unmarshal(rec.Body.Bytes(), &pub)
		var keys []string
		for k := range pub.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if rec.Code != http.StatusOK || strings.Join(keys, ",") != "school_name,whatsapp_number" {
			t.Errorf("public settings keys = %v (HTTP %d), want exactly school_name and whatsapp_number", keys, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "secret-value") {
			t.Error("internal setting leaked through the public endpoint")
		}
		rec = call(t, app.AdminMiddleware(app.AdminSettingsHandler), "GET", "/api/admin/settings", adminToken, "")
		if !strings.Contains(rec.Body.String(), "internal_api_key") {
			t.Error("admin settings endpoint should still list every key")
		}
	})

	if t.Failed() {
		t.Log("FAIL: Group 2 cleanup incomplete (see subtest errors above)")
	} else {
		t.Log("PASS: Firebase config centralized, one DB driver, dead code and column removed, FCM tokens safe, reports complete, params validated, settings allow-listed")
	}
}
