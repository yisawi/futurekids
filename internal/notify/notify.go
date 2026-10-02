package notify

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	"future_kids/internal/background"

	"firebase.google.com/go/v4/messaging"
)

// SaveNotificationHistory stores a notification for the parent with parentID. It also stores
// the parent's phone number, which code from before notifications were keyed by parent reads.
func SaveNotificationHistory(db *sql.DB, parentID int, phone, title, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := db.ExecContext(ctx,
		"INSERT INTO notifications (parent_id, parent_phone, title, body) VALUES ($1, $2, $3, $4)",
		parentID, phone, title, body)
	if err != nil {
		slog.Error("Failed to save notification history", "parent_id", parentID, "error", err)
	}
}

// SendPushNotification sends an FCM message in the background. If FCM reports the token as
// unregistered (app uninstalled or token rotated), that device token is deleted so no further
// pushes are attempted. Tokens never appear in logs; use
// TokenFingerprint to correlate.
func SendPushNotification(bg *background.Group, client *messaging.Client, db *sql.DB, token, title, body string) {
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
	bg.Go("FCM push", func() {
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
	})
}

// ParentDeviceTokensSQL selects, for the parent whose id is the expression %s, all of that
// parent's device tokens as one comma-separated value (NULL when there are none).
const ParentDeviceTokensSQL = `(SELECT string_agg(dt.token, ',' ORDER BY dt.id) FROM device_tokens dt WHERE dt.parent_id = %s)`

// DeviceTokens splits a value selected with ParentDeviceTokensSQL. Tokens never contain commas.
func DeviceTokens(list sql.NullString) []string {
	if !list.Valid || list.String == "" {
		return nil
	}
	return strings.Split(list.String, ",")
}

// SendToDevices sends one push to each device token, in the background.
func SendToDevices(bg *background.Group, client *messaging.Client, db *sql.DB, tokens []string, title, body string) {
	for _, token := range tokens {
		SendPushNotification(bg, client, db, token, title, body)
	}
}

// clearDeadToken deletes the one device token FCM reports as no longer registered.
func clearDeadToken(db *sql.DB, token string) {
	if db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := db.ExecContext(ctx, "DELETE FROM device_tokens WHERE token = $1", token)
	if err != nil {
		slog.Error("FCM token is unregistered but could not be cleared", "token", TokenFingerprint(token), "error", err)
		return
	}
	n, _ := res.RowsAffected()
	slog.Warn("FCM token is no longer registered; deleted it", "token", TokenFingerprint(token), "devices", n)
}

// TokenFingerprint returns a short, non-reversible identifier for an FCM token, safe to log.
func TokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "fcm:" + hex.EncodeToString(sum[:6])
}
