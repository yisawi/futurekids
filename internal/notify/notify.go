package notify

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

// SendPushNotification sends an FCM message in the background. If FCM reports the token as
// unregistered (app uninstalled or token rotated), the token is cleared from every student
// that carries it so no further pushes are attempted. Tokens never appear in logs; use
// TokenFingerprint to correlate.
func SendPushNotification(client *messaging.Client, db *sql.DB, token, title, body string) {
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
				slog.Error("SendPushNotification: recovered panic in send goroutine", "panic", r)
			}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		response, err := client.Send(ctx, msg)
		switch {
		case err == nil:
			slog.Info("Sent FCM message", "token", TokenFingerprint(token), "message_id", response)
		case messaging.IsUnregistered(err):
			clearDeadToken(db, token)
		default:
			slog.Error("Failed to send FCM message", "token", TokenFingerprint(token), "error", err)
		}
	}()
}

// clearDeadToken removes an FCM token that FCM no longer accepts from every student.
func clearDeadToken(db *sql.DB, token string) {
	if db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := db.ExecContext(ctx, "UPDATE students SET fcm_token = NULL WHERE fcm_token = $1", token)
	if err != nil {
		slog.Error("FCM token is unregistered but could not be cleared", "token", TokenFingerprint(token), "error", err)
		return
	}
	n, _ := res.RowsAffected()
	slog.Warn("FCM token is no longer registered; cleared it", "token", TokenFingerprint(token), "students", n)
}

// TokenFingerprint returns a short, non-reversible identifier for an FCM token, safe to log.
func TokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "fcm:" + hex.EncodeToString(sum[:6])
}
