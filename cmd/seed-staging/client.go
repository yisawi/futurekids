package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

type client struct {
	base  string
	token string
	http  *http.Client
}

// apiError is a response the seeder did not expect. It never carries the password or a token.
type apiError struct {
	method, path string
	status       int
	message      string
	retryAfter   string
}

func (e *apiError) Error() string {
	if e.status == http.StatusTooManyRequests {
		return fmt.Sprintf("%s %s: 429 Too Many Requests; Retry-After: %s seconds", e.method, e.path, e.retryAfter)
	}
	return fmt.Sprintf("%s %s: HTTP %d %s", e.method, e.path, e.status, e.message)
}

func newClient(base string) *client {
	return &client{base: base, http: &http.Client{Timeout: 2 * time.Minute}}
}

func (c *client) send(method, path, contentType string, body []byte, auth bool) (int, []byte, http.Header, error) {
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if auth && c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s: reading response: %w", method, path, err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return resp.StatusCode, raw, resp.Header, &apiError{method: method, path: path, status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After")}
	}
	return resp.StatusCode, raw, resp.Header, nil
}

func errorFrom(method, path string, status int, raw []byte) *apiError {
	var env struct {
		Message string `json:"message"`
	}
	json.Unmarshal(raw, &env)
	if env.Message == "" {
		env.Message = strings.TrimSpace(string(raw))
		if len(env.Message) > 120 {
			env.Message = env.Message[:120]
		}
	}
	return &apiError{method: method, path: path, status: status, message: env.Message}
}

// call sends a JSON request with the admin token and decodes a 200 response into out. Any other
// status is returned as an *apiError.
func (c *client) call(method, path string, in, out any) error {
	var body []byte
	ct := ""
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
		ct = "application/json"
	}
	status, raw, _, err := c.send(method, path, ct, body, true)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return errorFrom(method, path, status, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: unexpected response: %v", method, path, err)
		}
	}
	return nil
}

func (c *client) login(username, password string) error {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	status, raw, _, err := c.send("POST", "/api/admin/login", "application/json", body, false)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return errorFrom("POST", "/api/admin/login", status, raw)
	}
	var resp struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &resp) != nil || resp.Data.Token == "" {
		return fmt.Errorf("POST /api/admin/login: no token in the response")
	}
	c.token = resp.Data.Token
	return nil
}

type formPart struct {
	name, filename, contentType string
	data                        []byte
}

func (c *client) multipart(method, path string, parts []formPart, out any) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		disposition := fmt.Sprintf(`form-data; name="%s"`, p.name)
		if p.filename != "" {
			disposition += fmt.Sprintf(`; filename="%s"`, p.filename)
		}
		h.Set("Content-Disposition", disposition)
		if p.contentType != "" {
			h.Set("Content-Type", p.contentType)
		}
		w, err := mw.CreatePart(h)
		if err != nil {
			return err
		}
		w.Write(p.data)
	}
	mw.Close()
	status, raw, _, err := c.send(method, path, mw.FormDataContentType(), buf.Bytes(), true)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return errorFrom(method, path, status, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// adms sends one ATTLOG batch as the device would: no token, plain text, answered "OK".
func (c *client) adms(serial, body string) error {
	path := "/iclock/cdata?SN=" + serial + "&table=ATTLOG"
	status, raw, _, err := c.send("POST", path, "text/plain", []byte(body), false)
	if err != nil {
		return err
	}
	if status != http.StatusOK || strings.TrimSpace(string(raw)) != "OK" {
		return &apiError{method: "POST", path: path, status: status, message: strings.TrimSpace(string(raw))}
	}
	return nil
}
