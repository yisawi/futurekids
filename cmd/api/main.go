package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	firebase "firebase.google.com/go/v4"
	"github.com/robfig/cron/v3"
	"google.golang.org/api/option"

	"future_kids/internal/auth"
	"future_kids/internal/config"
	cronpkg "future_kids/internal/cron"
	"future_kids/internal/database"
	"future_kids/internal/handlers"
)

func main() {
	// Configuring the error logging system (Logger) to use JSON format
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// 1. Load the config
	cfg := config.LoadConfig()
	auth.InitAuth(cfg.JWTSecret)

	// 2. Connect to the database
	db, err := database.NewConnection(cfg.DBUrl)
	if err != nil {
		slog.Error("Failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// تحديد أقصى عدد للاتصالات المفتوحة (يمنع خنق السيرفر)
	db.SetMaxOpenConns(25)
	// تحديد أقصى عدد للاتصالات الخاملة (يحافظ على الذاكرة)
	db.SetMaxIdleConns(25)
	// إغلاق الاتصالات التي ظلت خاملة لفترة طويلة
	db.SetConnMaxLifetime(15 * time.Minute)

	// Initialize Firebase FCM
	ctx := context.Background()
	var opt option.ClientOption

	if firebaseJSON := os.Getenv("FIREBASE_CREDENTIALS_JSON"); firebaseJSON != "" {
		opt = option.WithCredentialsJSON([]byte(firebaseJSON))
	} else if _, err := os.Stat("firebase-credentials.json"); err == nil {
		opt = option.WithCredentialsFile("firebase-credentials.json")
	} else {
		slog.Error("CRITICAL ERROR: Neither FIREBASE_CREDENTIALS_JSON nor firebase-credentials.json found")
		os.Exit(1)
	}

	fbApp, err := firebase.NewApp(ctx, nil, opt)
	if err != nil {
		slog.Error("Failed to initialize Firebase", "error", err)
		os.Exit(1)
	}

	fcmClient, err := fbApp.Messaging(ctx)
	if err != nil {
		slog.Error("Failed to get FCM client", "error", err)
		os.Exit(1)
	}

	// Passing the database connection and the notification client together
	appEnv := &handlers.AppEnv{
		DB:        db,
		FCMClient: fcmClient,
	}

	// 3. Setup the HTTP Server
	mux := http.NewServeMux()

	// A test path to verfiy if the server is working or not
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Server is healthy and running!"))
	})

	// -------------------------------------------------------------------------
	// ZKTeco ADMS device routes — PUBLIC, no auth middleware.
	// The device firmware speaks raw ADMS; it cannot carry JWT tokens.
	// Routes live at the root level, NOT under /api or /v1.
	//
	// GET  /iclock/cdata       — device registration / heartbeat on boot (stateless, no DB)
	// POST /iclock/cdata       — device pushes attendance data (→ appEnv.ADMSHandler, needs DB)
	// GET  /iclock/getrequest  — device polls for server commands (stateless, no DB)
	//
	// NOTE: POST must use appEnv.ADMSHandler, NOT the stateless ADMSCdataHandler stub.
	// -------------------------------------------------------------------------
	mux.HandleFunc("GET /iclock/cdata", handlers.ADMSCdataHandler)
	mux.HandleFunc("POST /iclock/cdata", appEnv.ADMSHandler) // ← real handler: parses ATTLOG + saves to DB
	mux.HandleFunc("GET /iclock/getrequest", handlers.ADMSGetRequestHandler)

	// Hardware routes: ADMS (ZKTeco text format) and JSON push
	mux.HandleFunc("/api/attendance/push", handlers.HardwareLoggerMiddleware(appEnv.DeviceAuthMiddleware(appEnv.ADMSHandler)))
	mux.HandleFunc("/api/attendance/push/json", handlers.HardwareLoggerMiddleware(appEnv.DeviceAuthMiddleware(appEnv.HardwareAttendancePushHandler)))
	// The path for Flutter App without the middleware
	mux.HandleFunc("/api/mobile/attendance/today", handlers.AuthMiddleware(appEnv.MobileTodayAttendanceHandler))
	mux.HandleFunc("/api/mobile/attendance/monthly", handlers.AuthMiddleware(appEnv.MobileMonthlyAttendanceHandler))
	mux.HandleFunc("/api/mobile/attendance/summary", handlers.AuthMiddleware(appEnv.MobileAttendanceSummaryHandler))
	mux.HandleFunc("/api/mobile/students", handlers.AuthMiddleware(appEnv.MobileStudentsHandler))
	mux.HandleFunc("/api/mobile/schedule", handlers.AuthMiddleware(appEnv.MobileScheduleHandler))
	mux.HandleFunc("/api/mobile/notifications", handlers.AuthMiddleware(appEnv.MobileNotificationsHandler))
	mux.HandleFunc("/api/mobile/banners", handlers.AuthMiddleware(appEnv.GetActiveBannersHandler))

	// Public mobile login route; all other mobile routes require AuthMiddleware.
	mux.HandleFunc("/api/mobile/login", appEnv.MobileLoginHandler)
	mux.HandleFunc("/api/admin/login", appEnv.AdminLoginHandler)
	mux.HandleFunc("/api/admin/dashboard", handlers.AdminMiddleware(appEnv.AdminDashboardHandler))
	mux.HandleFunc("/api/admin/students", handlers.AdminMiddleware(appEnv.AdminStudentsHandler))
	mux.HandleFunc("/api/admin/leaves", handlers.AdminMiddleware(appEnv.AdminCreateLeaveHandler))
	mux.HandleFunc("/api/admin/attendance", handlers.AdminMiddleware(appEnv.AdminDailyAttendanceHandler))
	mux.HandleFunc("/api/admin/export/excel", handlers.AdminMiddleware(appEnv.AdminExportExcelHandler))
	mux.HandleFunc("/api/admin/settings", handlers.AdminMiddleware(appEnv.AdminSettingsHandler))
	mux.HandleFunc("/api/admin/devices", handlers.AdminMiddleware(appEnv.AdminDevicesHandler))

	// مسار الموبايل العام (بدون AuthMiddleware)
	mux.HandleFunc("/api/mobile/settings", appEnv.MobileSettingsHandler)

	loc, err := time.LoadLocation("Asia/Baghdad")
	if err != nil {
		slog.Error("Failed to load Baghdad timezone", "error", err)
		os.Exit(1)
	}
	c := cron.New(cron.WithLocation(loc))
	if _, err := c.AddFunc("0 12 * * *", func() {
		cronpkg.ProcessDailyAbsences(appEnv.DB, appEnv.FCMClient)
	}); err != nil {
		slog.Error("Failed to schedule cron job", "error", err)
		os.Exit(1)
	}

	c.Start()
	defer c.Stop()

	slog.Info("Starting server", "port", cfg.Port)

	// Start the Server
	err = http.ListenAndServe(":"+cfg.Port, mux)
	if err != nil {
		slog.Error("Server failed to start", "error", err)
		os.Exit(1)
	}

}
