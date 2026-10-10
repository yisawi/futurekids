package notify

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"future_kids/internal/background"

	"firebase.google.com/go/v4/messaging"
)

// Audience types of a broadcast.
const (
	AudienceAll    = "all"
	AudienceParent = "parent"
	AudienceClass  = "class"
)

// Audience selects the parents a broadcast reaches: every parent (AudienceAll), the parent
// ParentID (AudienceParent), or the parents of the active students whose grade and/or section
// equal Grade and Section exactly (AudienceClass; a nil field matches any value).
type Audience struct {
	Type     string
	Grade    *string
	Section  *string
	ParentID *int
}

// ErrNoRecipients means no parent matches the audience; nothing was written.
var ErrNoRecipients = errors.New("no parents match this audience")

// PushBatchSize is the most device tokens one FCM multicast call takes. PushBatchTimeout bounds
// each call, so a stuck batch ends before the server's graceful shutdown window does.
const PushBatchSize = 500

var PushBatchTimeout = 20 * time.Second

// recipientsSQL selects the recipient parents p of an audience given as $1 type, $2 parent id,
// $3 grade and $4 section. A parent with several matching children appears once.
const recipientsSQL = `
	FROM parents p
	WHERE CASE $1::text
		WHEN 'all' THEN true
		WHEN 'parent' THEN p.id = $2::int
		ELSE EXISTS (
			SELECT 1 FROM students s
			WHERE s.parent_id = p.id AND s.is_active = true
			  AND ($3::text IS NULL OR s.grade = $3::text)
			  AND ($4::text IS NULL OR s.section = $4::text))
	END`

type execQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (a Audience) args() []any {
	return []any{a.Type, a.ParentID, a.Grade, a.Section}
}

// CountRecipients returns how many parents the audience reaches.
func CountRecipients(ctx context.Context, q execQuerier, a Audience) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) `+recipientsSQL, a.args()...).Scan(&n)
	return n, err
}

// LockBroadcasts serializes broadcast creation until tx ends.
func LockBroadcasts(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `LOCK TABLE broadcasts IN SHARE ROW EXCLUSIVE MODE`)
	return err
}

// RecentDuplicate reports whether a broadcast with the same title, body and audience was
// created less than within ago. Call it after LockBroadcasts in the same transaction.
func RecentDuplicate(ctx context.Context, tx *sql.Tx, title, body string, a Audience, within time.Duration) (bool, error) {
	var found bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM broadcasts
			WHERE title = $1 AND body = $2 AND audience_type = $3
			  AND audience_parent_id IS NOT DISTINCT FROM $4::int
			  AND audience_grade IS NOT DISTINCT FROM $5::text
			  AND audience_section IS NOT DISTINCT FROM $6::text
			  AND created_at > LOCALTIMESTAMP - make_interval(secs => $7))`,
		title, body, a.Type, a.ParentID, a.Grade, a.Section, within.Seconds()).Scan(&found)
	return found, err
}

// CreateBroadcast records a broadcast in the caller's transaction and gives every
// recipient parent an unread notification with its title and body, in one statement. It returns
// the broadcast id and the number of recipients, or ErrNoRecipients (roll back then). After
// the transaction commits, call PushBroadcast with the id.
func CreateBroadcast(ctx context.Context, tx *sql.Tx, title, body string, a Audience) (id, recipients int, err error) {
	if err = tx.QueryRowContext(ctx, `
		INSERT INTO broadcasts (title, body, audience_type, audience_parent_id, audience_grade, audience_section, recipient_count)
		VALUES ($1, $2, $3, $4, $5, $6, 0)
		RETURNING id`, title, body, a.Type, a.ParentID, a.Grade, a.Section).Scan(&id); err != nil {
		return 0, 0, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO notifications (parent_id, parent_phone, title, body, broadcast_id)
		SELECT p.id, p.phone_number, $5, $6, $7 `+recipientsSQL+`
		ORDER BY p.id`, append(a.args(), title, body, id)...)
	if err != nil {
		return 0, 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	if n == 0 {
		return 0, 0, ErrNoRecipients
	}
	if _, err = tx.ExecContext(ctx, `UPDATE broadcasts SET recipient_count = $1 WHERE id = $2`, n, id); err != nil {
		return 0, 0, err
	}
	return id, int(n), nil
}

// PushBroadcast sends the broadcast as a push to every device of its recipients, in the
// background, in batches of at most PushBatchSize. Tokens FCM reports as unregistered are
// deleted; other failures are only logged. Without an FCM client it logs and does nothing.
func PushBroadcast(bg *background.Group, client *messaging.Client, db *sql.DB, id int, title, body string) {
	if client == nil {
		slog.Info("Broadcast push skipped: Firebase is not configured", "broadcast_id", id)
		return
	}
	bg.Go("broadcast push", func() {
		tokens, err := broadcastTokens(db, id)
		if err != nil {
			slog.Error("Broadcast push: device tokens query failed", "broadcast_id", id, "error", err)
			return
		}
		sent, failed, cleared, batches := 0, 0, 0, 0
		for start := 0; start < len(tokens); start += PushBatchSize {
			batch := tokens[start:min(start+PushBatchSize, len(tokens))]
			batches++
			ctx, cancel := context.WithTimeout(context.Background(), PushBatchTimeout)
			resp, err := client.SendEachForMulticast(ctx, &messaging.MulticastMessage{
				Tokens:       batch,
				Notification: &messaging.Notification{Title: title, Body: body},
			})
			cancel()
			slog.Debug("Broadcast push batch", "broadcast_id", id, "batch_size", len(batch))
			if err != nil {
				failed += len(batch)
				slog.Error("Broadcast push: batch failed", "broadcast_id", id, "batch_size", len(batch), "error", err)
				continue
			}
			for i, r := range resp.Responses {
				switch {
				case r.Success:
					sent++
				case messaging.IsUnregistered(r.Error):
					failed++
					cleared++
					clearDeadToken(db, batch[i])
				default:
					failed++
					slog.Error("Broadcast push: send failed", "broadcast_id", id, "token", TokenFingerprint(batch[i]), "error", r.Error)
				}
			}
		}
		slog.Info("Broadcast push finished", "broadcast_id", id, "tokens", len(tokens), "batches", batches, "sent", sent, "failed", failed, "cleared", cleared)
	})
}

func broadcastTokens(db *sql.DB, id int) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `
		SELECT dt.token
		FROM notifications n
		JOIN device_tokens dt ON dt.parent_id = n.parent_id
		WHERE n.broadcast_id = $1
		ORDER BY dt.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tokens []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}
