package warnings

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"future_kids/internal/testdb"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

type f1Part struct {
	name, filename, contentType string
	data                        []byte
}

func f1Image(data []byte) f1Part { return f1Part{"image", "banner.jpg", "image/jpeg", data} }

func f1Text(name, value string) f1Part { return f1Part{name: name, data: []byte(value)} }

func f1Form(parts ...f1Part) (string, []byte) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		if p.filename != "" {
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, p.name, p.filename))
		} else {
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"`, p.name))
		}
		if p.contentType != "" {
			h.Set("Content-Type", p.contentType)
		}
		w, _ := mw.CreatePart(h)
		w.Write(p.data)
	}
	mw.Close()
	return mw.FormDataContentType(), buf.Bytes()
}

func f1Do(t *testing.T, srv *g3Server, method, path string, header map[string]string, contentType string, body []byte) a14Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.url(path), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return a14Response{resp.StatusCode, resp.Header, raw, time.Since(start)}
}

func f1Picture(seed int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 16, 9))
	for x := 0; x < 16; x++ {
		for y := 0; y < 9; y++ {
			img.Set(x, y, color.RGBA{uint8(seed * 37), uint8(x * 16), uint8(y * 28), 255})
		}
	}
	return img
}

func f1JPEG(t *testing.T, seed int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, f1Picture(seed), &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func f1PNG(t *testing.T, seed int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, f1Picture(seed)); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func f1GIF(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := gif.Encode(&buf, f1Picture(1), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// f1WebP is a RIFF/WEBP container with a VP8L chunk of filler bytes: enough for content
// sniffing, which is all the server checks.
func f1WebP(seed int) []byte {
	payload := bytes.Repeat([]byte{byte(seed), 0x2f, 0x00}, 20)
	chunk := append([]byte("VP8L"), binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))...)
	chunk = append(chunk, payload...)
	body := append([]byte("WEBP"), chunk...)
	return append(append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...), body...)
}

// f1Padded is a JPEG followed by filler, exactly n bytes long.
func f1Padded(t *testing.T, n int) []byte {
	t.Helper()
	data := f1JPEG(t, 9)
	return append(data, bytes.Repeat([]byte{0}, n-len(data))...)
}

func f1Sum(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// f1Snapshot lists every banner with its picture metadata.
func f1Snapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`
		SELECT b.id, COALESCE(b.title, '<NULL>'), COALESCE(b.action_link, '<NULL>'), COALESCE(b.is_active::text, '<NULL>'), b.image_url,
		       COALESCE(bi.content_type, '-'), COALESCE(bi.size_bytes, 0), COALESCE(bi.checksum, '-')
		FROM banners b LEFT JOIN banner_images bi ON bi.banner_id = b.id ORDER BY b.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, size int
		var title, link, active, stored, ct, sum string
		if err := rows.Scan(&id, &title, &link, &active, &stored, &ct, &size, &sum); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%d|%s|%s|%s|%s|%s|%d|%s\n", id, title, link, active, stored, ct, size, sum)
	}
	var images int
	db.QueryRow(`SELECT COUNT(*) FROM banner_images`).Scan(&images)
	fmt.Fprintf(&b, "images=%d\n", images)
	return b.String()
}

func f1Data(t *testing.T, r a14Response) map[string]any {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("got %d %s, want 200", r.status, r.body)
	}
	data, ok := r.json(t)["data"].(map[string]any)
	if !ok {
		t.Fatalf("no data object in %s", r.body)
	}
	return data
}

func f1ID(t *testing.T, r a14Response) int {
	t.Helper()
	return int(f1Data(t, r)["id"].(float64))
}

func f1Fail(t *testing.T, r a14Response, status int, message string) {
	t.Helper()
	if r.status != status {
		t.Errorf("got %d %s, want %d %q", r.status, r.body, status, message)
		return
	}
	if m := r.json(t); m["status"] != "error" || m["message"] != message {
		t.Errorf("body %s, want {status: error, message: %q}", r.body, message)
	}
}

const f1PictureMessage = "image must be a JPEG, PNG or WebP picture"

// TestF1Banners verifies Task F1: the admin dashboard lists, uploads, edits and deletes
// banners whose pictures are stored in banner_images, pictures are recognised by their bytes,
// served unchanged with caching headers, publicly only while active, and every failed write
// leaves the stored banners exactly as they were.
func TestF1Banners(t *testing.T) {
	srv, db := a14Server(t, "f1")
	admin := a14AdminToken(t, srv)
	bearer := a14Bearer(admin)
	created, updated, deleted := 0, 0, 0
	send := func(method, query string, parts ...f1Part) a14Response {
		t.Helper()
		ct, body := f1Form(parts...)
		r := f1Do(t, srv, method, "/api/admin/banners"+query, bearer, ct, body)
		if r.status == http.StatusOK {
			if method == "POST" {
				created++
			} else {
				updated++
			}
		}
		return r
	}
	create := func(parts ...f1Part) a14Response { t.Helper(); return send("POST", "", parts...) }
	update := func(id int, parts ...f1Part) a14Response {
		t.Helper()
		return send("PUT", fmt.Sprintf("?id=%d", id), parts...)
	}
	adminImage := func(id int, header map[string]string) a14Response {
		t.Helper()
		h := map[string]string{"Authorization": "Bearer " + admin}
		for k, v := range header {
			h[k] = v
		}
		return f1Do(t, srv, "GET", fmt.Sprintf("/api/admin/banners/image?id=%d", id), h, "", nil)
	}
	publicImage := func(id int, header map[string]string) a14Response {
		t.Helper()
		return f1Do(t, srv, "GET", fmt.Sprintf("/api/mobile/banners/image?id=%d", id), header, "", nil)
	}
	checksum := func(id int) string {
		t.Helper()
		var sum string
		db.QueryRow(`SELECT checksum FROM banner_images WHERE banner_id = $1`, id).Scan(&sum)
		return sum
	}

	t.Run("JPEG, PNG and WebP are served byte for byte with caching headers", func(t *testing.T) {
		for _, tc := range []struct {
			name, contentType string
			data              []byte
		}{
			{"JPEG", "image/jpeg", f1JPEG(t, 1)},
			{"PNG", "image/png", f1PNG(t, 2)},
			{"WebP", "image/webp", f1WebP(3)},
		} {
			r := create(f1Image(tc.data), f1Text("title", tc.name+" banner"))
			d := f1Data(t, r)
			id := int(d["id"].(float64))
			etag := `"` + f1Sum(tc.data) + `"`
			if d["image_content_type"] != tc.contentType || d["image_size_bytes"] != float64(len(tc.data)) || d["is_active"] != true ||
				d["title"] != tc.name+" banner" || d["action_link"] != nil || d["image_url"] != fmt.Sprintf("/api/mobile/banners/image?id=%d", id) {
				t.Errorf("%s: created banner %v", tc.name, d)
			}
			if ts, _ := d["created_at"].(string); !strings.HasSuffix(ts, "+03:00") {
				t.Errorf("%s: created_at %v, want RFC 3339 in +03:00", tc.name, d["created_at"])
			} else if _, err := time.Parse(time.RFC3339, ts); err != nil {
				t.Errorf("%s: created_at %q: %v", tc.name, ts, err)
			}
			for _, route := range []struct {
				name  string
				get   func(int, map[string]string) a14Response
				cache string
			}{{"admin", adminImage, "private, no-cache"}, {"public", publicImage, "public, max-age=300"}} {
				r := route.get(id, nil)
				if r.status != 200 || !bytes.Equal(r.body, tc.data) {
					t.Errorf("%s %s: %d, %d bytes, want 200 and the %d uploaded bytes", tc.name, route.name, r.status, len(r.body), len(tc.data))
				}
				for k, want := range map[string]string{"Content-Type": tc.contentType, "X-Content-Type-Options": "nosniff", "ETag": etag, "Cache-Control": route.cache, "Content-Length": fmt.Sprint(len(tc.data))} {
					if got := r.header.Get(k); got != want {
						t.Errorf("%s %s: %s %q, want %q", tc.name, route.name, k, got, want)
					}
				}
				for _, inm := range []string{etag, "W/" + etag, `"` + strings.Repeat("0", 64) + `", ` + etag, "*"} {
					r := route.get(id, map[string]string{"If-None-Match": inm})
					if r.status != 304 || len(r.body) != 0 || r.header.Get("ETag") != etag || r.header.Get("Cache-Control") != route.cache {
						t.Errorf("%s %s If-None-Match %s: %d %d bytes ETag=%q, want 304 with the ETag and no body", tc.name, route.name, inm, r.status, len(r.body), r.header.Get("ETag"))
					}
				}
				if r := route.get(id, map[string]string{"If-None-Match": `"` + strings.Repeat("a", 64) + `"`}); r.status != 200 || !bytes.Equal(r.body, tc.data) {
					t.Errorf("%s %s with another ETag: %d", tc.name, route.name, r.status)
				}
			}
			if r := f1Do(t, srv, "HEAD", fmt.Sprintf("/api/mobile/banners/image?id=%d", id), nil, "", nil); r.status != 200 || len(r.body) != 0 || r.header.Get("ETag") != etag {
				t.Errorf("%s HEAD: %d %d bytes", tc.name, r.status, len(r.body))
			}
		}
	})

	t.Run("pictures are recognised by their bytes, not their names", func(t *testing.T) {
		jpg := f1JPEG(t, 4)
		r := create(f1Part{"image", "notes.txt", "text/plain", jpg})
		if d := f1Data(t, r); d["image_content_type"] != "image/jpeg" {
			t.Errorf("JPEG declared as text/plain stored as %v", d["image_content_type"])
		}
		before := f1Snapshot(t, db)
		svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"><script>alert(1)</script></svg>`)
		bmp := append([]byte("BM"), bytes.Repeat([]byte{0}, 60)...)
		for _, tc := range []struct {
			name string
			part f1Part
			msg  string
		}{
			{"text declared as JPEG", f1Part{"image", "photo.jpg", "image/jpeg", []byte("this is not a picture at all")}, f1PictureMessage},
			{"SVG", f1Part{"image", "banner.svg", "image/svg+xml", svg}, f1PictureMessage},
			{"SVG named .png", f1Part{"image", "banner.png", "image/png", svg}, f1PictureMessage},
			{"GIF", f1Part{"image", "banner.gif", "image/gif", f1GIF(t)}, f1PictureMessage},
			{"BMP", f1Part{"image", "banner.bmp", "image/bmp", bmp}, f1PictureMessage},
			{"HTML", f1Part{"image", "banner.jpg", "image/jpeg", []byte("<html><body>hi</body></html>")}, f1PictureMessage},
			{"empty file", f1Part{"image", "banner.jpg", "image/jpeg", nil}, "image is empty"},
		} {
			f1Fail(t, create(tc.part, f1Text("title", "Rejected")), 400, tc.msg)
		}
		f1Fail(t, create(f1Text("title", "No picture")), 400, "image is required")
		f1Fail(t, create(f1Image(jpg), f1Image(jpg)), 400, "image must be sent once")
		f1Fail(t, create(f1Image(jpg), f1Text("title", "a"), f1Text("title", "b")), 400, "title must be sent once")
		f1Fail(t, f1Do(t, srv, "POST", "/api/admin/banners", bearer, "application/json", []byte(`{"title":"json"}`)), 400, "Request must be multipart/form-data")
		f1Fail(t, f1Do(t, srv, "POST", "/api/admin/banners", bearer, "", nil), 400, "Request must be multipart/form-data")
		f1Fail(t, f1Do(t, srv, "POST", "/api/admin/banners", bearer, "multipart/form-data; boundary=f1", []byte("--f1\r\nnot a part header\r\n")), 400, "Invalid multipart body")
		if after := f1Snapshot(t, db); after != before {
			t.Errorf("rejected uploads changed the table\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	t.Run("2 MiB is the largest picture and other routes keep 64 KiB", func(t *testing.T) {
		exact := f1Padded(t, 2<<20)
		id := f1ID(t, create(f1Image(exact)))
		if r := adminImage(id, nil); r.status != 200 || len(r.body) != 2<<20 || f1Sum(r.body) != f1Sum(exact) {
			t.Errorf("2 MiB picture served as %d, %d bytes", r.status, len(r.body))
		}
		before := f1Snapshot(t, db)
		f1Fail(t, create(f1Image(f1Padded(t, 2<<20+1))), 413, "image must be at most 2 MiB (2097152 bytes)")
		f1Fail(t, update(id, f1Image(f1Padded(t, 2<<20+1))), 413, "image must be at most 2 MiB (2097152 bytes)")
		f1Fail(t, create(f1Image(f1JPEG(t, 5)), f1Part{name: "padding", data: bytes.Repeat([]byte("p"), 2<<20+64<<10)}), 413, "Request body too large")
		f1Fail(t, create(f1Part{name: "padding", data: bytes.Repeat([]byte("p"), 3<<20)}, f1Image(f1JPEG(t, 5))), 413, "Request body too large")
		if after := f1Snapshot(t, db); after != before {
			t.Errorf("413s changed the table")
		}
		f1Fail(t, f1Do(t, srv, "PUT", "/api/admin/settings", bearer, "application/json", []byte(`{"key":"k","value":"`+strings.Repeat("v", 100<<10)+`"}`)), 413, "Request body too large")
	})

	t.Run("title, action_link and is_active", func(t *testing.T) {
		jpg := f1JPEG(t, 6)
		stored := func(id int) (title, link sql.NullString, active bool) {
			t.Helper()
			if err := db.QueryRow(`SELECT title, action_link, is_active FROM banners WHERE id = $1`, id).Scan(&title, &link, &active); err != nil {
				t.Fatal(err)
			}
			return
		}
		long := strings.Repeat("ع", 255)
		if id := f1ID(t, create(f1Image(jpg), f1Text("title", long))); true {
			if title, _, _ := stored(id); title.String != long {
				t.Errorf("255-character title stored as %d characters", len([]rune(title.String)))
			}
		}
		f1Fail(t, create(f1Image(jpg), f1Text("title", long+"ع")), 400, "title must be at most 255 characters")
		d := f1Data(t, create(f1Image(jpg), f1Text("title", "   "), f1Text("action_link", " \t ")))
		if title, link, active := stored(int(d["id"].(float64))); title.Valid || link.Valid || !active || d["title"] != nil || d["action_link"] != nil {
			t.Errorf("blank title and link: stored %v %v active=%v, response %v %v", title, link, active, d["title"], d["action_link"])
		}
		if title, _, _ := stored(f1ID(t, create(f1Image(jpg), f1Text("title", "  Open day  ")))); title.String != "Open day" {
			t.Errorf("title stored as %q, want it trimmed", title.String)
		}
		f1Fail(t, create(f1Image(jpg), f1Text("title", "bad \xff byte")), 400, "title must be text")
		f1Fail(t, create(f1Image(jpg), f1Text("title", "nul\x00byte")), 400, "title must be text")

		link2048 := "https://example.com/" + strings.Repeat("a", 2048-len("https://example.com/"))
		if _, link, _ := stored(f1ID(t, create(f1Image(jpg), f1Text("action_link", link2048)))); link.String != link2048 {
			t.Errorf("2048-character action_link not stored")
		}
		f1Fail(t, create(f1Image(jpg), f1Text("action_link", link2048+"a")), 400, "action_link must be at most 2048 characters")
		for _, ok := range []string{"https://example.com/news?id=1", "HTTP://EXAMPLE.COM", "  https://example.com/x  "} {
			if r := create(f1Image(jpg), f1Text("action_link", ok)); r.status != 200 {
				t.Errorf("action_link %q: %d %s", ok, r.status, r.body)
			}
		}
		for _, bad := range []string{"javascript:alert(1)", "JavaScript:alert(1)", "data:text/html,hi", "intent://scan#Intent;end", "/relative/path", "example.com/page", "ftp://example.com/file", "https://", "http:///nohost", "mailto:office@example.com", "https:example.com"} {
			f1Fail(t, create(f1Image(jpg), f1Text("action_link", bad)), 400, "action_link must be an absolute http or https URL")
		}
		if _, _, active := stored(f1ID(t, create(f1Image(jpg), f1Text("is_active", "false")))); active {
			t.Errorf(`is_active "false" stored as true`)
		}
		for _, bad := range []string{"TRUE", "True", "1", "0", "yes", "", " true", "false "} {
			f1Fail(t, create(f1Image(jpg), f1Text("is_active", bad)), 400, `is_active must be "true" or "false"`)
		}
	})

	t.Run("PUT changes only the parts it receives", func(t *testing.T) {
		first := f1JPEG(t, 7)
		id := f1ID(t, create(f1Image(first), f1Text("title", "First"), f1Text("action_link", "https://example.com/first")))
		sum := checksum(id)
		d := f1Data(t, update(id, f1Text("title", "Second")))
		if d["title"] != "Second" || d["action_link"] != "https://example.com/first" || d["is_active"] != true || checksum(id) != sum {
			t.Errorf("title-only update: %v, checksum changed %v", d, checksum(id) != sum)
		}
		d = f1Data(t, update(id, f1Text("is_active", "false")))
		if d["is_active"] != false || d["title"] != "Second" || checksum(id) != sum {
			t.Errorf("is_active-only update: %v", d)
		}
		oldTag := `"` + sum + `"`
		second := f1PNG(t, 8)
		d = f1Data(t, update(id, f1Image(second)))
		if d["image_content_type"] != "image/png" || d["is_active"] != false || d["title"] != "Second" || checksum(id) != f1Sum(second) {
			t.Errorf("image update: %v", d)
		}
		if r := adminImage(id, map[string]string{"If-None-Match": oldTag}); r.status != 200 || !bytes.Equal(r.body, second) || r.header.Get("ETag") != `"`+f1Sum(second)+`"` {
			t.Errorf("after replacing the picture: %d, ETag %q", r.status, r.header.Get("ETag"))
		}
		if d = f1Data(t, update(id, f1Text("title", " "), f1Text("action_link", ""))); d["title"] != nil || d["action_link"] != nil {
			t.Errorf("blank title and link should clear them: %v", d)
		}
		before := f1Snapshot(t, db)
		f1Fail(t, update(id), 400, "send at least one of image, title, action_link or is_active")
		f1Fail(t, update(id, f1Text("unknown", "x")), 400, "send at least one of image, title, action_link or is_active")
		f1Fail(t, update(999999, f1Text("title", "Ghost")), 404, "Banner not found")
		f1Fail(t, send("PUT", "?id=abc", f1Text("title", "x")), 400, "Invalid banner ID")
		f1Fail(t, send("PUT", "", f1Text("title", "x")), 400, "Invalid banner ID")
		f1Fail(t, update(id, f1Image(f1JPEG(t, 1)), f1Text("is_active", "maybe")), 400, `is_active must be "true" or "false"`)
		if after := f1Snapshot(t, db); after != before {
			t.Errorf("rejected updates changed the table")
		}

		var legacy int
		db.QueryRow(`INSERT INTO banners (title, image_url) VALUES ('Legacy', 'https://example.com/banners/legacy-put.jpg') RETURNING id`).Scan(&legacy)
		d = f1Data(t, update(legacy, f1Image(first)))
		if d["image_url"] != fmt.Sprintf("/api/mobile/banners/image?id=%d", legacy) || d["image_content_type"] != "image/jpeg" {
			t.Errorf("picture added to a legacy banner: %v", d)
		}
	})

	t.Run("DELETE removes the banner and its picture", func(t *testing.T) {
		id := f1ID(t, create(f1Image(f1JPEG(t, 10))))
		r := f1Do(t, srv, "DELETE", fmt.Sprintf("/api/admin/banners?id=%d", id), bearer, "", nil)
		if r.status != 200 || r.json(t)["message"] != "Banner deleted" {
			t.Fatalf("delete: %d %s", r.status, r.body)
		}
		deleted++
		var rows, images int
		db.QueryRow(`SELECT (SELECT COUNT(*) FROM banners WHERE id = $1), (SELECT COUNT(*) FROM banner_images WHERE banner_id = $1)`, id).Scan(&rows, &images)
		if rows != 0 || images != 0 {
			t.Errorf("after delete: %d banner rows, %d picture rows", rows, images)
		}
		f1Fail(t, adminImage(id, nil), 404, "Banner image not found")
		f1Fail(t, f1Do(t, srv, "DELETE", fmt.Sprintf("/api/admin/banners?id=%d", id), bearer, "", nil), 404, "Banner not found")
		f1Fail(t, f1Do(t, srv, "DELETE", "/api/admin/banners?id=0", bearer, "", nil), 400, "Invalid banner ID")
	})

	t.Run("the admin list shows every banner, newest first, without picture bytes", func(t *testing.T) {
		var legacy int
		db.QueryRow(`INSERT INTO banners (title, image_url, action_link, is_active, created_at) VALUES (NULL, 'https://example.com/banners/old.jpg', NULL, NULL, '2020-01-01 08:00') RETURNING id`).Scan(&legacy)
		r := f1Do(t, srv, "GET", "/api/admin/banners", bearer, "", nil)
		if r.status != 200 {
			t.Fatalf("list: %d %s", r.status, r.body)
		}
		var resp struct {
			Data []map[string]any `json:"data"`
		}
		json.Unmarshal(r.body, &resp)
		var total int
		db.QueryRow(`SELECT COUNT(*) FROM banners`).Scan(&total)
		if len(resp.Data) != total {
			t.Fatalf("list has %d banners, the table %d", len(resp.Data), total)
		}
		for i, b := range resp.Data {
			var keys []string
			for k := range b {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if strings.Join(keys, ",") != "action_link,created_at,id,image_content_type,image_size_bytes,image_url,is_active,title" {
				t.Errorf("banner %v has keys %v", b["id"], keys)
			}
			if i > 0 && b["created_at"].(string) > resp.Data[i-1]["created_at"].(string) {
				t.Errorf("banner %v is newer than the one before it", b["id"])
			}
		}
		last := resp.Data[len(resp.Data)-1]
		if int(last["id"].(float64)) != legacy || last["image_url"] != "https://example.com/banners/old.jpg" || last["is_active"] != false ||
			last["image_content_type"] != nil || last["image_size_bytes"] != nil || last["title"] != nil {
			t.Errorf("legacy banner listed as %v", last)
		}
		if len(r.body) > 64<<10 {
			t.Errorf("list is %d bytes; it must not carry picture bytes", len(r.body))
		}
	})

	t.Run("the parent app sees active banners and their public pictures", func(t *testing.T) {
		const parentPhone, parentPin = "+9647000001201", "604158"
		if r := a14Do(t, srv, "POST", "/api/admin/students", bearer, map[string]any{"name": "Sara Example", "parent_name": "Omar Example", "parent_phone": parentPhone, "parent_pin": parentPin}); r.status != 200 {
			t.Fatalf("create student: %d %s", r.status, r.body)
		}
		login := a14Do(t, srv, "POST", "/api/mobile/login", nil, map[string]string{"phone": parentPhone, "pin": parentPin})
		if login.status != 200 {
			t.Fatalf("parent login: %d %s", login.status, login.body)
		}
		parent := a14Bearer(fmt.Sprint(login.json(t)["data"].(map[string]any)["token"]))
		var legacy int
		db.QueryRow(`INSERT INTO banners (title, image_url, action_link, created_at) VALUES (NULL, 'https://example.com/banners/legacy.jpg', NULL, '2021-01-01 08:00') RETURNING id`).Scan(&legacy)
		pic := f1JPEG(t, 11)
		id := f1ID(t, create(f1Image(pic), f1Text("title", "Open day"), f1Text("action_link", "https://example.com/open-day")))
		list := func() map[int]map[string]any {
			t.Helper()
			r := a14Do(t, srv, "GET", "/api/mobile/banners", parent, nil)
			if r.status != 200 {
				t.Fatalf("parent banners: %d %s", r.status, r.body)
			}
			var resp struct {
				Data []map[string]any `json:"data"`
			}
			json.Unmarshal(r.body, &resp)
			out := map[int]map[string]any{}
			for _, b := range resp.Data {
				var keys []string
				for k, v := range b {
					keys = append(keys, k)
					if _, ok := v.(string); !ok && k != "id" {
						t.Errorf("parent banner %v: %s is %T, want a string", b["id"], k, v)
					}
				}
				sort.Strings(keys)
				if strings.Join(keys, ",") != "action_link,id,image_url,title" {
					t.Errorf("parent banner keys %v, want the unchanged four", keys)
				}
				out[int(b["id"].(float64))] = b
			}
			return out
		}
		got := list()
		up, ok := got[id]
		if !ok || up["image_url"] != fmt.Sprintf("/api/mobile/banners/image?id=%d", id) || up["title"] != "Open day" || up["action_link"] != "https://example.com/open-day" {
			t.Fatalf("uploaded banner in the parent list: %v", up)
		}
		if lb := got[legacy]; lb == nil || lb["image_url"] != "https://example.com/banners/legacy.jpg" || lb["title"] != "" || lb["action_link"] != "" {
			t.Errorf("legacy banner in the parent list: %v", lb)
		}
		if r := f1Do(t, srv, "GET", up["image_url"].(string), nil, "", nil); r.status != 200 || !bytes.Equal(r.body, pic) {
			t.Errorf("image_url without a token: %d", r.status)
		}

		update(id, f1Text("is_active", "false"))
		if _, still := list()[id]; still {
			t.Errorf("deactivated banner is still in the parent list")
		}
		f1Fail(t, publicImage(id, nil), 404, "Banner image not found")
		f1Fail(t, publicImage(id, parent), 404, "Banner image not found")
		if r := adminImage(id, nil); r.status != 200 || !bytes.Equal(r.body, pic) {
			t.Errorf("admin preview of an inactive banner: %d", r.status)
		}
		f1Fail(t, publicImage(legacy, nil), 404, "Banner image not found")
		f1Fail(t, publicImage(999999, nil), 404, "Banner image not found")
		f1Fail(t, f1Do(t, srv, "GET", "/api/mobile/banners/image?id=x", nil, "", nil), 400, "Invalid banner ID")
		f1Fail(t, f1Do(t, srv, "GET", "/api/mobile/banners/image", nil, "", nil), 400, "Invalid banner ID")
		update(id, f1Text("is_active", "true"))
		if r := publicImage(id, nil); r.status != 200 {
			t.Errorf("reactivated banner picture: %d", r.status)
		}
	})

	t.Run("failed writes leave banners and pictures exactly as they were", func(t *testing.T) {
		id := f1ID(t, create(f1Image(f1JPEG(t, 12)), f1Text("title", "Kept")))
		before := f1Snapshot(t, db)
		f1Fail(t, create(f1Image(f1JPEG(t, 13)), f1Text("title", "Valid"), f1Text("is_active", "nope")), 400, `is_active must be "true" or "false"`)
		for _, q := range []string{
			`ALTER TABLE banners ADD CONSTRAINT f1_refuse_poison CHECK (title <> 'Poison')`,
			`ALTER TABLE banner_images ADD CONSTRAINT f1_refuse_size CHECK (size_bytes <> 4242)`,
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		defer db.Exec(`ALTER TABLE banners DROP CONSTRAINT f1_refuse_poison; ALTER TABLE banner_images DROP CONSTRAINT f1_refuse_size`)
		f1Fail(t, create(f1Image(f1JPEG(t, 14)), f1Text("title", "Poison")), 500, "Failed to save banner")
		f1Fail(t, create(f1Image(f1Padded(t, 4242)), f1Text("title", "Picture refused")), 500, "Failed to save banner")
		f1Fail(t, update(id, f1Image(f1PNG(t, 15)), f1Text("title", "Poison")), 500, "Failed to save banner")
		f1Fail(t, update(id, f1Image(f1Padded(t, 4242)), f1Text("title", "Renamed")), 500, "Failed to save banner")
		if after := f1Snapshot(t, db); after != before {
			t.Errorf("failed writes changed the tables\nbefore:\n%s\nafter:\n%s", before, after)
		}
		for _, op := range []string{"AdminBannersHandler: create failed", "AdminBannersHandler: update failed"} {
			if srv.out.index(op) < 0 {
				t.Errorf("no %q log", op)
			}
		}
	})

	t.Run("logs name the banner and the picture, never the form values", func(t *testing.T) {
		create(f1Image(f1JPEG(t, 16)), f1Text("title", "f1-secret-title-marker"), f1Text("action_link", "https://example.com/f1-secret-link-marker"))
		out := srv.out.String()
		for _, secret := range []string{"f1-secret-title-marker", "f1-secret-link-marker", "Open day", "Poison"} {
			if strings.Contains(out, secret) {
				t.Errorf("server log contains %q", secret)
			}
		}
		counts := map[string]int{}
		for _, raw := range strings.Split(out, "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(raw), &m) != nil {
				continue
			}
			msg, _ := m["msg"].(string)
			if msg != "Banner created" && msg != "Banner updated" && msg != "Banner deleted" {
				continue
			}
			counts[msg]++
			var keys []string
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			got := strings.Join(keys, ",")
			withImage, idOnly := "banner_id,content_type,level,msg,size_bytes,time", "banner_id,level,msg,time"
			if m["level"] != "INFO" || (got != withImage && got != idOnly) || (msg == "Banner created" && got != withImage) || (msg == "Banner deleted" && got != idOnly) {
				t.Errorf("log line %v", m)
			}
		}
		if counts["Banner created"] != created || counts["Banner updated"] != updated || counts["Banner deleted"] != deleted {
			t.Errorf("log lines %v for %d creates, %d updates, %d deletes", counts, created, updated, deleted)
		}
	})

	t.Run("authentication on every admin route", func(t *testing.T) {
		id := f1ID(t, create(f1Image(f1JPEG(t, 17))))
		later, earlier := time.Now().Add(time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
		expired := e1Sign(t, jwt.MapClaims{"username": "admin", "role": "admin", "sv": 0, "exp": earlier})
		parentTok := e1Sign(t, jwt.MapClaims{"parent_id": 1, "phone": "+9647000001201", "role": "parent", "sv": 0, "exp": later})
		ct, body := f1Form(f1Image(f1JPEG(t, 18)), f1Text("title", "Denied"))
		routes := []struct {
			method, path string
		}{
			{"GET", "/api/admin/banners"},
			{"POST", "/api/admin/banners"},
			{"PUT", fmt.Sprintf("/api/admin/banners?id=%d", id)},
			{"DELETE", fmt.Sprintf("/api/admin/banners?id=%d", id)},
			{"GET", fmt.Sprintf("/api/admin/banners/image?id=%d", id)},
		}
		before := f1Snapshot(t, db)
		call := func(method, path string, header map[string]string) a14Response {
			if method == "POST" || method == "PUT" {
				return f1Do(t, srv, method, path, header, ct, body)
			}
			return f1Do(t, srv, method, path, header, "", nil)
		}
		for _, rt := range routes {
			for _, tc := range []struct {
				name   string
				header map[string]string
				status int
				msg    string
			}{
				{"no token", nil, 401, "Unauthorized"},
				{"malformed token", a14Bearer("not-a-jwt"), 401, "Unauthorized"},
				{"wrong scheme", map[string]string{"Authorization": "Basic " + admin}, 401, "Unauthorized"},
				{"expired token", a14Bearer(expired), 401, "Unauthorized"},
				{"parent token", a14Bearer(parentTok), 403, "Forbidden"},
			} {
				r := call(rt.method, rt.path, tc.header)
				if r.status != tc.status || r.json(t)["message"] != tc.msg {
					t.Errorf("%s %s with %s: %d %s, want %d %s", rt.method, rt.path, tc.name, r.status, r.body, tc.status, tc.msg)
				}
			}
		}
		script, err := os.ReadFile(filepath.Join("..", "..", "db", "scripts", "rotate_admin_password.sql"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(strings.ReplaceAll(string(script), "NEW_PASSWORD_HERE", "f1-rotated-password")); err != nil {
			t.Fatalf("rotation script: %v", err)
		}
		for _, rt := range routes {
			if r := call(rt.method, rt.path, bearer); r.status != 401 {
				t.Errorf("%s %s with a token from before the rotation: %d %s, want 401", rt.method, rt.path, r.status, r.body)
			}
		}
		if after := f1Snapshot(t, db); after != before {
			t.Errorf("rejected requests changed the tables")
		}
		if r := publicImage(id, nil); r.status != 200 {
			t.Errorf("public picture needs no token: %d", r.status)
		}
		login := a14AdminLogin(t, srv, "198.51.100.4", "admin", "f1-rotated-password")
		if login.status != 200 {
			t.Fatalf("login after rotation: %d %s", login.status, login.body)
		}
		fresh := a14Bearer(fmt.Sprint(login.json(t)["data"].(map[string]any)["token"]))
		if r := f1Do(t, srv, "GET", "/api/admin/banners", fresh, "", nil); r.status != 200 {
			t.Errorf("new token after rotation: %d", r.status)
		}
	})

	t.Run("wrong method", func(t *testing.T) {
		for path, allow := range map[string]string{"/api/admin/banners/image?id=1": "GET, HEAD", "/api/mobile/banners/image?id=1": "GET, HEAD", "/api/admin/banners": "DELETE, GET, HEAD, POST, PUT"} {
			r := f1Do(t, srv, "PATCH", path, nil, "", nil)
			if r.status != 405 || r.header.Get("Allow") != allow {
				t.Errorf("PATCH %s: %d Allow=%q, want 405 Allow=%q", path, r.status, r.header.Get("Allow"), allow)
			}
		}
	})
}

// TestF1BeforePhoneNormalisation runs the new code on a database at 000024 without 000025,
// the state Staging is in between the steps of the README deploy notes.
func TestF1BeforePhoneNormalisation(t *testing.T) {
	db, dsn := setupThrowawayDB(t, "f1v24")
	if _, err := db.Exec(a14Migration(t, "000025_normalize_phone_numbers.down.sql")); err != nil {
		t.Fatal(err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(a14AdminPassword), bcrypt.DefaultCost)
	if _, err := db.Exec(`UPDATE admins SET password_hash = $1 WHERE username = 'admin'`, string(hash)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (1201, 'Omar Example', '+9647000001201', 'x')`); err != nil {
		t.Fatal(err)
	}
	srv := g3Start(t, dsn, g3FreePort(t), true, "TRUSTED_PROXY_CIDRS=127.0.0.1/32", "ABSENCE_CRON_SCHEDULE=0 0 1 1 *")
	admin := a14Bearer(a14AdminToken(t, srv))
	pic := f1PNG(t, 20)
	ct, body := f1Form(f1Image(pic), f1Text("title", "Before 000025"))
	r := f1Do(t, srv, "POST", "/api/admin/banners", admin, ct, body)
	id := f1ID(t, r)
	parent := a14Bearer(e1Sign(t, jwt.MapClaims{"parent_id": 1201, "phone": "+9647000001201", "role": "parent", "sv": 0, "exp": time.Now().Add(time.Hour).Unix()}))
	list := a14Do(t, srv, "GET", "/api/mobile/banners", parent, nil)
	if list.status != 200 || !strings.Contains(string(list.body), fmt.Sprintf(`"image_url":"/api/mobile/banners/image?id=%d"`, id)) {
		t.Errorf("parent list on 000024: %d %s", list.status, list.body)
	}
	if r := f1Do(t, srv, "GET", fmt.Sprintf("/api/mobile/banners/image?id=%d", id), nil, "", nil); r.status != 200 || !bytes.Equal(r.body, pic) {
		t.Errorf("public picture on 000024: %d", r.status)
	}
}

// TestF1BannerImagesMigration verifies migration 000024: it applies alone on a database at
// 000023, rolls back by deactivating banners that lose their picture, applies again, is reached
// by golang-migrate from versions 20 to 23, and phone normalisation (now 000025) still applies
// after it, collision abort included.
func TestF1BannerImagesMigration(t *testing.T) {
	migrate, err := exec.LookPath("migrate")
	if err != nil {
		t.Skip("golang-migrate CLI not installed")
	}
	fresh := func(t *testing.T, label string) (*sql.DB, func(args ...string) (string, error)) {
		t.Helper()
		cli, cliDSN := setupThrowawayDB(t, label)
		for _, tbl := range []string{"banner_images", "device_tokens", "settings", "notifications", "weekly_schedules", "student_leaves", "banners", "admins", "attendance_logs", "devices", "students", "parents"} {
			if _, err := cli.Exec("DROP TABLE IF EXISTS " + tbl + " CASCADE"); err != nil {
				t.Fatal(err)
			}
		}
		cli.Exec(`DROP FUNCTION IF EXISTS get_student_status(INT, DATE)`)
		cli.Exec(`DROP FUNCTION IF EXISTS notifications_fill_parent_id()`)
		u, _ := url.Parse(cliDSN)
		q := u.Query()
		q.Set("sslmode", "disable")
		u.RawQuery = q.Encode()
		return cli, func(args ...string) (string, error) {
			out, err := exec.Command(migrate, append([]string{"-path", testdb.MigrationsDir(), "-database", u.String()}, args...)...).CombinedOutput()
			return strings.TrimSpace(string(out)), err
		}
	}
	state := func(db *sql.DB) (table, phoneColumns bool) {
		db.QueryRow(`SELECT to_regclass('banner_images') IS NOT NULL, EXISTS (SELECT 1 FROM information_schema.columns WHERE column_name = 'phone_number_original')`).Scan(&table, &phoneColumns)
		return
	}

	t.Run("000023 to 000024 adds only banner_images; down and up again", func(t *testing.T) {
		db, run := fresh(t, "f1m23")
		if out, err := run("goto", "23"); err != nil {
			t.Fatalf("goto 23: %v\n%s", err, out)
		}
		if table, _ := state(db); table {
			t.Fatalf("banner_images exists at 23")
		}
		if out, err := run("goto", "24"); err != nil {
			t.Fatalf("goto 24: %v\n%s", err, out)
		}
		if v, _ := run("version"); v != "24" {
			t.Errorf("version %q, want 24", v)
		}
		if table, phone := state(db); !table || phone {
			t.Fatalf("at 24: banner_images %v, phone columns %v; want true, false", table, phone)
		}
		if _, err := db.Exec(`INSERT INTO banners (id, title, image_url) VALUES (1, 'Uploaded', ''), (2, 'Legacy', 'https://example.com/legacy.jpg');
			INSERT INTO banner_images (banner_id, content_type, size_bytes, checksum, data) VALUES (1, 'image/png', 3, repeat('a', 64), '\x010203')`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO banner_images (banner_id, content_type, size_bytes, checksum, data) VALUES (2, 'image/gif', 3, repeat('b', 64), '\x010203')`); err == nil {
			t.Errorf("banner_images accepted image/gif")
		}
		if out, err := run("down", "1"); err != nil {
			t.Fatalf("down 1: %v\n%s", err, out)
		}
		var uploadedActive, legacyActive bool
		db.QueryRow(`SELECT (SELECT is_active FROM banners WHERE id = 1), (SELECT is_active FROM banners WHERE id = 2)`).Scan(&uploadedActive, &legacyActive)
		if table, _ := state(db); table || uploadedActive || !legacyActive {
			t.Errorf("after down: banner_images %v, uploaded banner active %v, legacy active %v; want false, false, true", table, uploadedActive, legacyActive)
		}
		if out, err := run("up", "1"); err != nil {
			t.Fatalf("up 1: %v\n%s", err, out)
		}
		if table, phone := state(db); !table || phone {
			t.Errorf("after up again: banner_images %v, phone columns %v", table, phone)
		}
	})

	for _, from := range []string{"20", "21", "22", "23"} {
		t.Run("golang-migrate from version "+from+" through 24 to 25", func(t *testing.T) {
			db, run := fresh(t, "f1v"+from)
			if out, err := run("goto", from); err != nil {
				t.Fatalf("goto %s: %v\n%s", from, err, out)
			}
			if out, err := run("goto", "24"); err != nil {
				t.Fatalf("goto 24: %v\n%s", err, out)
			}
			if out, err := run("up"); err != nil {
				t.Fatalf("up: %v\n%s", err, out)
			}
			if v, _ := run("version"); v != "25" {
				t.Errorf("version %q, want 25", v)
			}
			if table, phone := state(db); !table || !phone {
				t.Errorf("at 25: banner_images %v, phone columns %v", table, phone)
			}
		})
	}

	t.Run("000025 still aborts on collisions after 000024", func(t *testing.T) {
		db, run := fresh(t, "f1coll")
		if out, err := run("goto", "24"); err != nil {
			t.Fatalf("goto 24: %v\n%s", err, out)
		}
		if _, err := db.Exec(`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (971, 'A', '07000000971', 'x'), (972, 'B', '+9647000000971', 'x')`); err != nil {
			t.Fatal(err)
		}
		out, err := run("up")
		if err == nil || !strings.Contains(out, "[971, 972]") {
			t.Fatalf("000025 should abort listing [971, 972]: %v\n%s", err, out)
		}
		if table, phone := state(db); !table || phone {
			t.Errorf("after the aborted 000025: banner_images %v, phone columns %v; want true, false", table, phone)
		}
	})
}
