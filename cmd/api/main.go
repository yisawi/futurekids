package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	firebase "firebase.google.com/go/v4"
	"google.golang.org/api/option"

	"future_kids/internal/config"
	"future_kids/internal/database"
	"future_kids/internal/handlers"
)

func main() {
	// Configuring the error logging system (Logger) to use JSON format
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// 1. Load the config
	cfg := config.LoadConfig()

	// 2. Connect to the database
	db, err := database.NewConnection(cfg.DBUrl)
	if err != nil {
		slog.Error("Failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// Initialize Firebase FCM
	ctx := context.Background()
	opt := option.WithCredentialsFile("firebase-credentials.json")
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

	// the path encapsulated with the middleware
	mux.HandleFunc("/api/attendance/push", handlers.HardwareLoggerMiddleware(appEnv.ADMSHandler))
	// The path for Flutter App without the middleware
	mux.HandleFunc("/api/mobile/attendance/today", handlers.AuthMiddleware(appEnv.GetTodayAttendanceHandler))
	mux.HandleFunc("/api/mobile/attendance/monthly", handlers.AuthMiddleware(appEnv.GetMonthlyAttendanceHandler))

	// The path for mobile app authentication (Login)
	mux.HandleFunc("/api/v1/auth/login", appEnv.MobileLoginHandler)

	slog.Info("Starting server", "port", cfg.Port)

	// Start the Server
	err = http.ListenAndServe(":"+cfg.Port, mux)
	if err != nil {
		slog.Error("Server failed to start", "error", err)
		os.Exit(1)
	}

}
