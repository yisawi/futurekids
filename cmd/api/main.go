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

	// Hardware route protected by request logging and active-device validation.
	mux.HandleFunc("/api/attendance/push", handlers.HardwareLoggerMiddleware(appEnv.DeviceAuthMiddleware(appEnv.ADMSHandler)))
	// The path for Flutter App without the middleware
	mux.HandleFunc("/api/mobile/attendance/today", handlers.AuthMiddleware(appEnv.GetTodayAttendanceHandler))
	mux.HandleFunc("/api/mobile/attendance/monthly", handlers.AuthMiddleware(appEnv.GetMonthlyAttendanceHandler))
	mux.HandleFunc("/api/mobile/attendance/summary", handlers.AuthMiddleware(appEnv.GetAttendanceSummaryHandler))
	mux.HandleFunc("/api/mobile/students", handlers.AuthMiddleware(appEnv.GetParentStudentsHandler))
	mux.HandleFunc("/api/mobile/schedule", handlers.AuthMiddleware(appEnv.GetWeeklyScheduleHandler))
	mux.HandleFunc("/api/mobile/notifications", handlers.AuthMiddleware(appEnv.GetNotificationsHandler))

	// The path for mobile app authentication (Login)
	mux.HandleFunc("/api/v1/auth/login", appEnv.MobileLoginHandler)

	loc, err := time.LoadLocation("Asia/Baghdad")
	if err != nil {
		slog.Error("Failed to load Baghdad timezone", "error", err)
		os.Exit(1)
	}
	c := cron.New(cron.WithLocation(loc))
	if _, err := c.AddFunc("0 12 * * *", func() {
		cronpkg.ProcessDailyAbsences(appEnv.DB)
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
