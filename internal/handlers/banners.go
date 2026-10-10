package handlers

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"future_kids/internal/tz"
)

// Banner uploads: the picture is at most MaxBannerImageBytes; the request body may add
// 64 KiB for the text parts and the multipart framing. Uploads may take BannerUploadTimeout,
// longer than the server's default deadlines, because pictures are sent over phone networks.
const (
	MaxBannerImageBytes      int64 = 2 << 20
	MaxBannerBodyBytes             = MaxBannerImageBytes + 64<<10
	BannerUploadTimeout            = 2 * time.Minute
	MaxBannerTitleChars            = 255
	MaxBannerActionLinkChars       = 2048
	maxBannerTextPartBytes   int64 = 16 << 10
)

// Cache-Control of the banner picture routes: parents' phones may reuse a picture for five
// minutes; the dashboard revalidates every time.
const (
	PublicBannerImageCache = "public, max-age=300"
	AdminBannerImageCache  = "private, no-cache"
)

var bannerImageTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true}

var bannerChecksumRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// AdminBanner is one banner as the dashboard sees it. Image fields are nil when the banner has
// no uploaded picture (rows entered before uploads existed).
type AdminBanner struct {
	ID               int     `json:"id"`
	Title            *string `json:"title"`
	ActionLink       *string `json:"action_link"`
	IsActive         bool    `json:"is_active"`
	CreatedAt        *string `json:"created_at"`
	ImageURL         string  `json:"image_url"`
	ImageContentType *string `json:"image_content_type"`
	ImageSizeBytes   *int64  `json:"image_size_bytes"`
}

// bannerImageURL is the image_url clients receive: the public picture route for an uploaded
// picture, otherwise the URL stored with the banner.
func bannerImageURL(id int, uploaded bool, stored string) string {
	if uploaded {
		return "/api/mobile/banners/image?id=" + strconv.Itoa(id)
	}
	return stored
}

type bannerImage struct {
	contentType string
	checksum    string
	data        []byte
}

// bannerForm holds the parts of a banner upload. A nil field was not sent; a sent title or
// action_link that is blank is an invalid (NULL) NullString.
type bannerForm struct {
	image      *bannerImage
	title      *sql.NullString
	actionLink *sql.NullString
	isActive   *bool
}

func (f *bannerForm) empty() bool {
	return f.image == nil && f.title == nil && f.actionLink == nil && f.isActive == nil
}

// formError is a rejected multipart/form-data request. bodyLimit is set when the whole body is
// larger than its limit; warn, when set, is logged as WARN with warnLimit before answering.
type formError struct {
	status    int
	msg       string
	bodyLimit int64
	warn      string
	warnLimit int64
}

func (e *formError) respond(w http.ResponseWriter, r *http.Request) {
	if e.bodyLimit > 0 {
		respondBodyTooLarge(w, r, e.bodyLimit)
		return
	}
	if e.warn != "" {
		slog.Warn(e.warn, "path", r.URL.Path, "remote_addr", r.RemoteAddr, "limit_bytes", e.warnLimit)
	}
	respondError(w, e.status, e.msg)
}

func formPartError(err error, bodyLimit int64) *formError {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return &formError{status: http.StatusRequestEntityTooLarge, bodyLimit: bodyLimit}
	}
	return &formError{status: http.StatusBadRequest, msg: "Invalid multipart body"}
}

// readFormParts streams a multipart/form-data body of at most maxBody bytes and returns the parts
// named in limits, each at most its limit; other parts are discarded and a part sent twice is
// rejected. A part over its limit stops reading with overLimit(name). After an error the rest of
// the body is drained, so a body over maxBody is answered with 413.
func readFormParts(w http.ResponseWriter, r *http.Request, maxBody int64, limits map[string]int64, overLimit func(name string) *formError) (map[string][]byte, *formError) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	raw, ferr := streamFormParts(r, maxBody, limits, overLimit)
	if ferr != nil && ferr.bodyLimit == 0 {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) && ferr.status != http.StatusRequestEntityTooLarge {
				ferr = &formError{status: http.StatusRequestEntityTooLarge, bodyLimit: maxBody}
			}
		}
	}
	return raw, ferr
}

func streamFormParts(r *http.Request, maxBody int64, limits map[string]int64, overLimit func(name string) *formError) (map[string][]byte, *formError) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, &formError{status: http.StatusBadRequest, msg: "Request must be multipart/form-data"}
	}
	raw := map[string][]byte{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return raw, nil
		}
		if err != nil {
			return nil, formPartError(err, maxBody)
		}
		name := part.FormName()
		limit, known := limits[name]
		if !known {
			if _, err := io.Copy(io.Discard, part); err != nil {
				return nil, formPartError(err, maxBody)
			}
			continue
		}
		if _, dup := raw[name]; dup {
			return nil, &formError{status: http.StatusBadRequest, msg: name + " must be sent once"}
		}
		data, err := io.ReadAll(io.LimitReader(part, limit+1))
		if err != nil {
			return nil, formPartError(err, maxBody)
		}
		if int64(len(data)) > limit {
			return nil, overLimit(name)
		}
		raw[name] = data
	}
}

var bannerPartLimits = map[string]int64{
	"image":       MaxBannerImageBytes,
	"title":       maxBannerTextPartBytes,
	"action_link": maxBannerTextPartBytes,
	"is_active":   maxBannerTextPartBytes,
}

func bannerPartOverLimit(name string) *formError {
	switch name {
	case "image":
		return &formError{status: http.StatusRequestEntityTooLarge, msg: fmt.Sprintf("image must be at most 2 MiB (%d bytes)", MaxBannerImageBytes),
			warn: "Banner image too large", warnLimit: MaxBannerImageBytes}
	case "is_active":
		return &formError{status: http.StatusBadRequest, msg: `is_active must be "true" or "false"`}
	case "action_link":
		return &formError{status: http.StatusBadRequest, msg: fmt.Sprintf("action_link must be at most %d characters", MaxBannerActionLinkChars)}
	}
	return &formError{status: http.StatusBadRequest, msg: fmt.Sprintf("%s must be at most %d characters", name, MaxBannerTitleChars)}
}

// readBannerForm streams a multipart/form-data body of at most MaxBannerBodyBytes and
// validates its parts. The picture type is detected from its bytes; the file name and the
// part's declared Content-Type are ignored. Unknown parts are discarded.
func readBannerForm(w http.ResponseWriter, r *http.Request) (*bannerForm, *formError) {
	raw, ferr := readFormParts(w, r, MaxBannerBodyBytes, bannerPartLimits, bannerPartOverLimit)
	if ferr != nil {
		return nil, ferr
	}
	return parseBannerParts(raw)
}

func parseBannerParts(raw map[string][]byte) (*bannerForm, *formError) {
	form := &bannerForm{}
	bad := func(msg string) (*bannerForm, *formError) {
		return nil, &formError{status: http.StatusBadRequest, msg: msg}
	}
	if data, ok := raw["image"]; ok {
		if len(data) == 0 {
			return bad("image is empty")
		}
		ct := http.DetectContentType(data)
		if !bannerImageTypes[ct] {
			return bad("image must be a JPEG, PNG or WebP picture")
		}
		sum := sha256.Sum256(data)
		form.image = &bannerImage{contentType: ct, checksum: hex.EncodeToString(sum[:]), data: data}
	}
	text := func(name string, chars int) (*sql.NullString, string) {
		data, ok := raw[name]
		if !ok {
			return nil, ""
		}
		v := string(data)
		if !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
			return nil, name + " must be text"
		}
		v = strings.TrimSpace(v)
		if msg := tooLong(name, v, chars); msg != "" {
			return nil, msg
		}
		return &sql.NullString{String: v, Valid: v != ""}, ""
	}
	var msg string
	if form.title, msg = text("title", MaxBannerTitleChars); msg != "" {
		return bad(msg)
	}
	if form.actionLink, msg = text("action_link", MaxBannerActionLinkChars); msg != "" {
		return bad(msg)
	}
	if form.actionLink != nil && form.actionLink.Valid && !absoluteWebURL(form.actionLink.String) {
		return bad("action_link must be an absolute http or https URL")
	}
	if data, ok := raw["is_active"]; ok {
		switch string(data) {
		case "true":
			form.isActive = new(bool)
			*form.isActive = true
		case "false":
			form.isActive = new(bool)
		default:
			return bad(`is_active must be "true" or "false"`)
		}
	}
	return form, nil
}

func absoluteWebURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")
}

// extendUploadDeadlines gives an authenticated upload BannerUploadTimeout to arrive and be
// answered, instead of the server's default read and write timeouts.
func extendUploadDeadlines(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(BannerUploadTimeout)
	rc.SetReadDeadline(deadline)
	rc.SetWriteDeadline(deadline)
}

func parseBannerID(r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	return id, err == nil && id >= 1
}

type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const adminBannerSelect = `
	SELECT b.id, b.title, b.action_link, COALESCE(b.is_active, false), b.created_at AT TIME ZONE 'Asia/Baghdad',
	       b.image_url, bi.content_type, bi.size_bytes
	FROM banners b
	LEFT JOIN banner_images bi ON bi.banner_id = b.id`

func scanAdminBanner(scan func(dest ...any) error) (AdminBanner, error) {
	var b AdminBanner
	var title, link, contentType sql.NullString
	var created sql.NullTime
	var size sql.NullInt64
	var stored string
	if err := scan(&b.ID, &title, &link, &b.IsActive, &created, &stored, &contentType, &size); err != nil {
		return b, err
	}
	if title.Valid {
		b.Title = &title.String
	}
	if link.Valid {
		b.ActionLink = &link.String
	}
	if created.Valid {
		s := created.Time.In(tz.Baghdad).Format(time.RFC3339)
		b.CreatedAt = &s
	}
	if contentType.Valid {
		b.ImageContentType = &contentType.String
		b.ImageSizeBytes = &size.Int64
	}
	b.ImageURL = bannerImageURL(b.ID, contentType.Valid, stored)
	return b, nil
}

func readAdminBanner(ctx context.Context, q rowQueryer, id int) (AdminBanner, error) {
	return scanAdminBanner(q.QueryRowContext(ctx, adminBannerSelect+` WHERE b.id = $1`, id).Scan)
}

func saveBannerImage(ctx context.Context, tx *sql.Tx, id int, img *bannerImage) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO banner_images (banner_id, content_type, size_bytes, checksum, data)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (banner_id) DO UPDATE
		SET content_type = EXCLUDED.content_type, size_bytes = EXCLUDED.size_bytes,
		    checksum = EXCLUDED.checksum, data = EXCLUDED.data, created_at = CURRENT_TIMESTAMP`,
		id, img.contentType, len(img.data), img.checksum, img.data)
	return err
}

var errBannerNotFound = errors.New("banner not found")

func (app *AppEnv) AdminBannersHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, err := app.DB.QueryContext(r.Context(), adminBannerSelect+` ORDER BY b.created_at DESC NULLS LAST, b.id DESC`)
		if err != nil {
			respondInternalError(w, "Database error", "AdminBannersHandler: query failed", err)
			return
		}
		defer rows.Close()
		banners := []AdminBanner{}
		for rows.Next() {
			b, err := scanAdminBanner(rows.Scan)
			if err != nil {
				respondInternalError(w, "Database error", "AdminBannersHandler: scan failed", err)
				return
			}
			banners = append(banners, b)
		}
		if err := rows.Err(); err != nil {
			respondInternalError(w, "Database error", "AdminBannersHandler: rows iteration failed", err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "data": banners})

	case http.MethodPost:
		extendUploadDeadlines(w)
		form, ferr := readBannerForm(w, r)
		if ferr != nil {
			ferr.respond(w, r)
			return
		}
		if form.image == nil {
			respondError(w, http.StatusBadRequest, "image is required")
			return
		}
		isActive := form.isActive == nil || *form.isActive
		var title, link sql.NullString
		if form.title != nil {
			title = *form.title
		}
		if form.actionLink != nil {
			link = *form.actionLink
		}
		var banner AdminBanner
		err := func() error {
			tx, err := app.DB.BeginTx(r.Context(), nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			var id int
			if err := tx.QueryRowContext(r.Context(), `
				INSERT INTO banners (title, image_url, action_link, is_active)
				VALUES ($1, '', $2, $3)
				RETURNING id`, title, link, isActive).Scan(&id); err != nil {
				return err
			}
			if err := saveBannerImage(r.Context(), tx, id, form.image); err != nil {
				return err
			}
			if banner, err = readAdminBanner(r.Context(), tx, id); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err != nil {
			respondInternalError(w, "Failed to save banner", "AdminBannersHandler: create failed", err)
			return
		}
		slog.Info("Banner created", "banner_id", banner.ID, "content_type", form.image.contentType, "size_bytes", len(form.image.data))
		respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "data": banner})

	case http.MethodPut:
		id, ok := parseBannerID(r)
		if !ok {
			respondError(w, http.StatusBadRequest, "Invalid banner ID")
			return
		}
		extendUploadDeadlines(w)
		form, ferr := readBannerForm(w, r)
		if ferr != nil {
			ferr.respond(w, r)
			return
		}
		if form.empty() {
			respondError(w, http.StatusBadRequest, "send at least one of image, title, action_link or is_active")
			return
		}
		var title, link sql.NullString
		if form.title != nil {
			title = *form.title
		}
		if form.actionLink != nil {
			link = *form.actionLink
		}
		var banner AdminBanner
		err := func() error {
			tx, err := app.DB.BeginTx(r.Context(), nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			err = tx.QueryRowContext(r.Context(), `
				UPDATE banners
				SET title = CASE WHEN $2::boolean THEN $3::text ELSE title END,
				    action_link = CASE WHEN $4::boolean THEN $5::text ELSE action_link END,
				    is_active = COALESCE($6::boolean, is_active)
				WHERE id = $1
				RETURNING id`, id, form.title != nil, title, form.actionLink != nil, link, form.isActive).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return errBannerNotFound
			}
			if err != nil {
				return err
			}
			if form.image != nil {
				if err := saveBannerImage(r.Context(), tx, id, form.image); err != nil {
					return err
				}
			}
			if banner, err = readAdminBanner(r.Context(), tx, id); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if errors.Is(err, errBannerNotFound) {
			respondError(w, http.StatusNotFound, "Banner not found")
			return
		}
		if err != nil {
			respondInternalError(w, "Failed to save banner", "AdminBannersHandler: update failed", err, "banner_id", id)
			return
		}
		if form.image != nil {
			slog.Info("Banner updated", "banner_id", id, "content_type", form.image.contentType, "size_bytes", len(form.image.data))
		} else {
			slog.Info("Banner updated", "banner_id", id)
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "data": banner})

	case http.MethodDelete:
		id, ok := parseBannerID(r)
		if !ok {
			respondError(w, http.StatusBadRequest, "Invalid banner ID")
			return
		}
		res, err := app.DB.ExecContext(r.Context(), `DELETE FROM banners WHERE id = $1`, id)
		if err != nil {
			respondInternalError(w, "Failed to delete banner", "AdminBannersHandler: delete failed", err, "banner_id", id)
			return
		}
		n, err := res.RowsAffected()
		if err != nil {
			respondInternalError(w, "Failed to delete banner", "AdminBannersHandler: rows affected failed", err, "banner_id", id)
			return
		}
		if n == 0 {
			respondError(w, http.StatusNotFound, "Banner not found")
			return
		}
		slog.Info("Banner deleted", "banner_id", id)
		respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "message": "Banner deleted"})

	default:
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// AdminBannerImageHandler serves the uploaded picture of any banner, active or not, so the
// dashboard can preview it.
func (app *AppEnv) AdminBannerImageHandler(w http.ResponseWriter, r *http.Request) {
	app.serveBannerImage(w, r, false, AdminBannerImageCache, "AdminBannersHandler: image query failed")
}

// MobileBannerImageHandler serves the uploaded picture of an active banner without
// authentication: banners are shown to every parent.
func (app *AppEnv) MobileBannerImageHandler(w http.ResponseWriter, r *http.Request) {
	app.serveBannerImage(w, r, true, PublicBannerImageCache, "MobileBannerImageHandler: query failed")
}

// ifNoneMatch returns the picture checksums named in an If-None-Match header, comma-joined,
// and whether it is "*". Weak validators match too, as RFC 9110 requires for If-None-Match.
func ifNoneMatch(header string) (checksums string, wildcard bool) {
	var sums []string
	for _, tag := range strings.Split(header, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" {
			wildcard = true
			continue
		}
		tag = strings.TrimPrefix(tag, "W/")
		if len(tag) >= 2 && tag[0] == '"' && tag[len(tag)-1] == '"' && bannerChecksumRE.MatchString(tag[1:len(tag)-1]) {
			sums = append(sums, tag[1:len(tag)-1])
		}
	}
	return strings.Join(sums, ","), wildcard
}

func (app *AppEnv) serveBannerImage(w http.ResponseWriter, r *http.Request, activeOnly bool, cacheControl, op string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	id, ok := parseBannerID(r)
	if !ok {
		respondError(w, http.StatusBadRequest, "Invalid banner ID")
		return
	}
	sums, wildcard := ifNoneMatch(r.Header.Get("If-None-Match"))
	var contentType, checksum string
	var notModified bool
	var data []byte
	err := app.DB.QueryRowContext(r.Context(), `
		SELECT bi.content_type, bi.checksum, m.hit, CASE WHEN m.hit THEN NULL ELSE bi.data END
		FROM banner_images bi
		JOIN banners b ON b.id = bi.banner_id
		CROSS JOIN LATERAL (SELECT $2::boolean OR bi.checksum = ANY (string_to_array($3::text, ',')) AS hit) m
		WHERE bi.banner_id = $1 AND (NOT $4::boolean OR b.is_active = true)`,
		id, wildcard, sums, activeOnly).Scan(&contentType, &checksum, &notModified, &data)
	if errors.Is(err, sql.ErrNoRows) {
		respondError(w, http.StatusNotFound, "Banner image not found")
		return
	}
	if err != nil {
		respondInternalError(w, "Database error", op, err, "banner_id", id)
		return
	}
	h := w.Header()
	h.Set("ETag", `"`+checksum+`"`)
	h.Set("Cache-Control", cacheControl)
	h.Set("X-Content-Type-Options", "nosniff")
	if notModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}
