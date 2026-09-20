package notify

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"firebase.google.com/go/v4/messaging"
)

func SaveNotificationHistory(db *sql.DB, phone, title, body string) {
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

// SendPushNotification sends an FCM message in the background.
func SendPushNotification(client *messaging.Client, token, title, body string) {
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

	// Send the notification in the background so as not to delay the server's response.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("recovered from panic in SendPushNotification", "panic", r)
			}
		}()

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
