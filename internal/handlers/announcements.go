package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"future_kids/internal/notify"
	"future_kids/internal/phone"
	"future_kids/internal/tz"
)

// Announcement limits, counted in characters as PostgreSQL counts them.
const (
	MaxAnnouncementTitleChars = 100
	MaxAnnouncementBodyChars  = 500
	AnnouncementsPageSize     = 50
	AnnouncementDuplicateWait = 60 * time.Second
)

type announcementAudienceRequest struct {
	Type        *string `json:"type"`
	Grade       *string `json:"grade"`
	Section     *string `json:"section"`
	ParentPhone *string `json:"parent_phone"`
}

type announcementRequest struct {
	Title    json.RawMessage              `json:"title"`
	Body     json.RawMessage              `json:"body"`
	Audience *announcementAudienceRequest `json:"audience"`
	DryRun   json.RawMessage              `json:"dry_run"`
}

// AnnouncementAudience is an announcement's audience in the sent log. Fields its type does not
// use are null; parent_name and parent_phone are the parent's current values.
type AnnouncementAudience struct {
	Type        string  `json:"type"`
	Grade       *string `json:"grade"`
	Section     *string `json:"section"`
	ParentName  *string `json:"parent_name"`
	ParentPhone *string `json:"parent_phone"`
}

// AnnouncementLogItem is one sent announcement.
type AnnouncementLogItem struct {
	ID             int                  `json:"id"`
	Title          string               `json:"title"`
	Body           string               `json:"body"`
	Audience       AnnouncementAudience `json:"audience"`
	RecipientCount int                  `json:"recipient_count"`
	ReadCount      int                  `json:"read_count"`
	CreatedAt      string               `json:"created_at"`
}

// announcementText decodes a JSON string field, trims it and checks it is 1 to max characters
// of valid UTF-8 without NUL.
func announcementText(name string, raw json.RawMessage, max int) (string, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", name + " is required"
	}
	if !utf8.Valid(raw) {
		return "", name + " must be text"
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", name + " must be a string"
	}
	if strings.ContainsRune(v, 0) {
		return "", name + " must be text"
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", name + " is required"
	}
	return v, tooLong(name, v, max)
}

// audienceClassField trims an optional grade or section; it must not be blank when sent.
func audienceClassField(name string, v *string) (*string, string) {
	if v == nil {
		return nil, ""
	}
	t := strings.TrimSpace(*v)
	if t == "" {
		return nil, name + " must not be blank"
	}
	if msg := tooLong(name, t, 50); msg != "" {
		return nil, msg
	}
	return &t, ""
}

// parseAudience checks the audience fields for its type and returns the audience and, for a
// parent, the canonical phone number to look up.
func parseAudience(a *announcementAudienceRequest) (notify.Audience, string, string) {
	if a == nil {
		return notify.Audience{}, "", "audience is required"
	}
	if a.Type == nil {
		return notify.Audience{}, "", "audience.type is required"
	}
	switch *a.Type {
	case notify.AudienceAll:
		if a.Grade != nil || a.Section != nil || a.ParentPhone != nil {
			return notify.Audience{}, "", "audience.grade, audience.section and audience.parent_phone must not be sent when audience.type is all"
		}
		return notify.Audience{Type: notify.AudienceAll}, "", ""
	case notify.AudienceParent:
		if a.Grade != nil || a.Section != nil {
			return notify.Audience{}, "", "audience.grade and audience.section must not be sent when audience.type is parent"
		}
		if a.ParentPhone == nil {
			return notify.Audience{}, "", "audience.parent_phone is required when audience.type is parent"
		}
		canonical, ok := phone.Normalize(*a.ParentPhone)
		if !ok {
			return notify.Audience{}, "", "audience." + InvalidParentPhoneMessage
		}
		return notify.Audience{Type: notify.AudienceParent}, canonical, ""
	case notify.AudienceClass:
		if a.ParentPhone != nil {
			return notify.Audience{}, "", "audience.parent_phone must not be sent when audience.type is class"
		}
		grade, msg := audienceClassField("audience.grade", a.Grade)
		if msg != "" {
			return notify.Audience{}, "", msg
		}
		section, msg := audienceClassField("audience.section", a.Section)
		if msg != "" {
			return notify.Audience{}, "", msg
		}
		if grade == nil && section == nil {
			return notify.Audience{}, "", "audience.grade or audience.section is required when audience.type is class"
		}
		return notify.Audience{Type: notify.AudienceClass, Grade: grade, Section: section}, "", ""
	}
	return notify.Audience{}, "", "audience.type must be all, parent or class"
}

func (app *AppEnv) AdminAnnouncementsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		app.createAnnouncement(w, r)
	case http.MethodGet:
		app.listAnnouncements(w, r)
	default:
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (app *AppEnv) createAnnouncement(w http.ResponseWriter, r *http.Request) {
	var req announcementRequest
	if !decodeJSONBody(w, r, &req, "Invalid request body") {
		return
	}
	title, msg := announcementText("title", req.Title, MaxAnnouncementTitleChars)
	if msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	body, msg := announcementText("body", req.Body, MaxAnnouncementBodyChars)
	if msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	audience, parentPhone, msg := parseAudience(req.Audience)
	if msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	dryRun := false
	switch string(bytes.TrimSpace(req.DryRun)) {
	case "", "null", "false":
	case "true":
		dryRun = true
	default:
		respondError(w, http.StatusBadRequest, "dry_run must be true or false")
		return
	}

	if audience.Type == notify.AudienceParent {
		var id int
		switch err := app.DB.QueryRowContext(r.Context(), `SELECT id FROM parents WHERE phone_number = $1`, parentPhone).Scan(&id); {
		case err == sql.ErrNoRows:
			respondError(w, http.StatusNotFound, "No parent has this phone number")
			return
		case err != nil:
			respondInternalError(w, "Failed to send announcement", "AdminAnnouncementsHandler: parent lookup failed", err)
			return
		}
		audience.ParentID = &id
	}

	if dryRun {
		n, err := notify.CountRecipients(r.Context(), app.DB, audience)
		if err != nil {
			respondInternalError(w, "Failed to send announcement", "AdminAnnouncementsHandler: recipient count failed", err, "audience", audience.Type)
			return
		}
		if n == 0 {
			respondError(w, http.StatusBadRequest, "No parents match this audience")
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "success",
			"message": "Preview only",
			"data":    map[string]interface{}{"id": nil, "recipient_count": n, "dry_run": true},
		})
		return
	}

	var id, recipients int
	duplicate := false
	err := func() error {
		tx, err := app.DB.BeginTx(r.Context(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := notify.LockAnnouncements(r.Context(), tx); err != nil {
			return err
		}
		if duplicate, err = notify.RecentDuplicate(r.Context(), tx, title, body, audience, AnnouncementDuplicateWait); err != nil || duplicate {
			return err
		}
		if id, recipients, err = notify.CreateAnnouncement(r.Context(), tx, title, body, audience); err != nil {
			return err
		}
		return tx.Commit()
	}()
	switch {
	case duplicate:
		respondError(w, http.StatusConflict, "The same announcement was sent to this audience less than a minute ago")
		return
	case errors.Is(err, notify.ErrNoRecipients):
		respondError(w, http.StatusBadRequest, "No parents match this audience")
		return
	case err != nil:
		respondInternalError(w, "Failed to send announcement", "AdminAnnouncementsHandler: send failed", err, "audience", audience.Type)
		return
	}
	slog.Info("Announcement sent", "announcement_id", id, "audience", audience.Type, "recipients", recipients)
	notify.PushAnnouncement(app.Background, app.FCMClient, app.DB, id, title, body)
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "success",
		"message": "Announcement sent",
		"data":    map[string]interface{}{"id": id, "recipient_count": recipients, "dry_run": false},
	})
}

func (app *AppEnv) listAnnouncements(w http.ResponseWriter, r *http.Request) {
	var before sql.NullInt64
	if v := r.URL.Query().Get("before"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id < 1 {
			respondError(w, http.StatusBadRequest, "before must be a positive announcement id")
			return
		}
		before = sql.NullInt64{Int64: id, Valid: true}
	}

	rows, err := app.DB.QueryContext(r.Context(), `
		SELECT a.id, a.title, a.body, a.audience_type, a.audience_grade, a.audience_section,
		       p.full_name, p.phone_number, a.recipient_count, a.created_at AT TIME ZONE 'Asia/Baghdad'
		FROM announcements a
		LEFT JOIN parents p ON p.id = a.audience_parent_id
		WHERE ($1::bigint IS NULL OR a.id < $1)
		ORDER BY a.id DESC
		LIMIT $2`, before, AnnouncementsPageSize+1)
	if err != nil {
		respondInternalError(w, "Database error", "AdminAnnouncementsHandler: query failed", err)
		return
	}
	defer rows.Close()
	items := []AnnouncementLogItem{}
	for rows.Next() {
		var it AnnouncementLogItem
		var grade, section, parentName, parentPhone sql.NullString
		var createdAt time.Time
		if err := rows.Scan(&it.ID, &it.Title, &it.Body, &it.Audience.Type, &grade, &section, &parentName, &parentPhone, &it.RecipientCount, &createdAt); err != nil {
			respondInternalError(w, "Database error", "AdminAnnouncementsHandler: scan failed", err)
			return
		}
		for _, f := range []struct {
			src sql.NullString
			dst **string
		}{{grade, &it.Audience.Grade}, {section, &it.Audience.Section}, {parentName, &it.Audience.ParentName}, {parentPhone, &it.Audience.ParentPhone}} {
			if f.src.Valid {
				v := f.src.String
				*f.dst = &v
			}
		}
		it.CreatedAt = createdAt.In(tz.Baghdad).Format(time.RFC3339)
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "AdminAnnouncementsHandler: rows iteration failed", err)
		return
	}
	hasMore := len(items) > AnnouncementsPageSize
	var nextBefore *int
	if hasMore {
		items = items[:AnnouncementsPageSize]
		nextBefore = &items[AnnouncementsPageSize-1].ID
	}

	if len(items) > 0 {
		ids := make([]int64, len(items))
		index := map[int]int{}
		for i, it := range items {
			ids[i] = int64(it.ID)
			index[it.ID] = i
		}
		counts, err := app.DB.QueryContext(r.Context(), `
			SELECT announcement_id, COUNT(*) FILTER (WHERE is_read)
			FROM notifications
			WHERE announcement_id = ANY($1::int[])
			GROUP BY announcement_id`, ids)
		if err != nil {
			respondInternalError(w, "Database error", "AdminAnnouncementsHandler: read count query failed", err)
			return
		}
		defer counts.Close()
		for counts.Next() {
			var id, n int
			if err := counts.Scan(&id, &n); err != nil {
				respondInternalError(w, "Database error", "AdminAnnouncementsHandler: read count scan failed", err)
				return
			}
			items[index[id]].ReadCount = n
		}
		if err := counts.Err(); err != nil {
			respondInternalError(w, "Database error", "AdminAnnouncementsHandler: read count rows iteration failed", err)
			return
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":      "success",
		"data":        items,
		"has_more":    hasMore,
		"next_before": nextBefore,
	})
}
