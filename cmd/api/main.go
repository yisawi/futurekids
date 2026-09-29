package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	firebase "firebase.google.com/go/v4"
	"github.com/robfig/cron/v3"
	"google.golang.org/api/option"

	"future_kids/internal/auth"
	"future_kids/internal/background"
	"future_kids/internal/config"
	cronpkg "future_kids/internal/cron"
	"future_kids/internal/database"
	"future_kids/internal/handlers"
	"future_kids/internal/ratelimit"
	"future_kids/internal/server"
	"future_kids/internal/tz"
)

func main() {
	os.Exit(run())
}

// run starts the server and blocks until SIGINT/SIGTERM, then shuts down gracefully.
// It returns the process exit code.
func run() int {
	// Configuring the error logging system (Logger) to use JSON format
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// 1. Load the config
	cfg, err := config.LoadConfig()
	if err != nil {
		slog.Error("Invalid configuration", "error", err)
		return 1
	}
	auth.InitAuth(cfg.JWTSecret)

	// 2. Connect to the database
	db, err := database.NewConnection(cfg.DBUrl, cfg.DBPool)
	if err != nil {
		slog.Error("Failed to connect to database", "error", err)
		return 1
	}
	// Until shutdown takes over, every failure path closes the pool itself.
	fail := func(msg string, args ...any) int {
		slog.Error(msg, args...)
		db.Close()
		return 1
	}

	// Initialize Firebase FCM
	ctx := context.Background()
	var opt option.ClientOption

	if cfg.FirebaseCredentialsJSON != "" {
		opt = option.WithCredentialsJSON([]byte(cfg.FirebaseCredentialsJSON))
	} else if _, err := os.Stat(cfg.FirebaseCredentialsPath); err == nil {
		opt = option.WithCredentialsFile(cfg.FirebaseCredentialsPath)
	} else {
		return fail("No Firebase credentials: set FIREBASE_CREDENTIALS_JSON, or FIREBASE_CREDENTIALS_PATH to a service-account file",
			"path", cfg.FirebaseCredentialsPath, "error", err)
	}

	fbApp, err := firebase.NewApp(ctx, nil, opt)
	if err != nil {
		return fail("Failed to initialize Firebase", "error", err)
	}

	fcmClient, err := fbApp.Messaging(ctx)
	if err != nil {
		return fail("Failed to get FCM client", "error", err)
	}

	// Passing the database connection and the notification client together
	bg := &background.Group{}
	appEnv := &handlers.AppEnv{
		DB:           db,
		FCMClient:    fcmClient,
		LoginLimiter: ratelimit.NewLoginLimiter(5, 15*time.Minute),
		Background:   bg,
	}

	// 3. Setup the HTTP Server
	mux := http.NewServeMux()

	// A test path to verfiy if the server is working or not
	mux.HandleFunc("GET /health", handlers.HealthHandler)

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
	mux.HandleFunc("/api/attendance/push", handlers.HardwareLoggerMiddleware(appEnv.ADMSHandler)) // ADMS alias: ADMSHandler authenticates the device itself and never returns JSON errors
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

	if tz.FromTZDatabase {
		slog.Info("Timezone loaded", "location", tz.Baghdad.String())
	} else {
		slog.Warn("No tz database on this system; using fixed UTC+3 for Asia/Baghdad (identical: Iraq has no DST)")
	}
	if cfg.OnRailway && cfg.RailwayDraining <= cfg.ShutdownTimeout {
		slog.Warn("Railway will SIGKILL this process before graceful shutdown can finish; set RAILWAY_DEPLOYMENT_DRAINING_SECONDS above SHUTDOWN_TIMEOUT",
			"railway_draining", cfg.RailwayDraining.String(), "shutdown_timeout", cfg.ShutdownTimeout.String())
	}

	c := cron.New(cron.WithLocation(tz.Baghdad))
	if _, err := c.AddFunc(cfg.AbsenceCronSchedule, func() {
		cronpkg.ProcessDailyAbsences(appEnv.DB, appEnv.FCMClient, bg)
	}); err != nil {
		return fail("Invalid ABSENCE_CRON_SCHEDULE", "schedule", cfg.AbsenceCronSchedule, "error", err)
	}

	// Stop on SIGINT (Ctrl-C) or SIGTERM (Railway deploys). A second signal kills the process.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	srv := server.New(":"+cfg.Port, mux, server.DefaultTimeouts)
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fail("Server failed to start", "addr", srv.Addr, "error", err)
	}
	c.Start()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	slog.Info("Server started", "addr", ln.Addr().String(), "absence_cron", cfg.AbsenceCronSchedule, "shutdown_timeout", cfg.ShutdownTimeout.String())

	select {
	case err := <-serveErr:
		<-c.Stop().Done()
		return fail("Server stopped unexpectedly", "error", err)
	case <-sigCtx.Done():
	}
	stopSignals()
	return shutdown(srv, c, bg, db, cfg.ShutdownTimeout)
}

// shutdown drains the process within timeout, in order: stop accepting connections and let
// in-flight requests finish, stop the absence cron and wait for a running job, wait for
// background work (push notifications, notification history, device last_sync), then close
// the database. It returns 0 when every phase finished in time. Otherwise it logs an ERROR
// and returns 1 without waiting further: PostgreSQL rolls back any transaction left open when
// the process exits, so an interrupted ADMS batch is never stored partially.
func shutdown(srv *http.Server, c *cron.Cron, bg *background.Group, db *sql.DB, timeout time.Duration) int {
	start := time.Now()
	slog.Info("Shutdown: signal received", "timeout", timeout.String())
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	clean := true

	if err := srv.Shutdown(ctx); err != nil {
		clean = false
		slog.Error("Shutdown: timeout exceeded while finishing in-flight requests; closing their connections", "error", err)
		srv.Close()
	} else {
		slog.Info("Shutdown: HTTP server stopped")
	}

	if finished(c.Stop().Done(), ctx) {
		slog.Info("Shutdown: cron stopped")
	} else {
		clean = false
		slog.Error("Shutdown: timeout exceeded while waiting for the running absence job")
	}

	if pending, err := bg.Wait(ctx); err != nil {
		clean = false
		slog.Error("Shutdown: timeout exceeded while waiting for background work", "pending", pending)
	} else {
		slog.Info("Shutdown: background work finished")
	}

	if !clean {
		slog.Error("Shutdown incomplete: exiting now; open transactions are rolled back by PostgreSQL", "duration", time.Since(start).String())
		return 1
	}
	db.Close()
	slog.Info("Shutdown: database closed")
	slog.Info("Shutdown complete", "duration", time.Since(start).String())
	return 0
}

// finished reports whether done closes before ctx expires, preferring done when both are ready.
func finished(done <-chan struct{}, ctx context.Context) bool {
	select {
	case <-done:
		return true
	default:
	}
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}
