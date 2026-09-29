package handlers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// respondJSON writes a JSON-encoded payload with the given HTTP status code.
// It sets Content-Type to application/json before writing.
func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// respondError writes a standard {"status":"error","message":"..."} JSON body.
func respondError(w http.ResponseWriter, status int, message string) {
	respondJSON(w, status, map[string]string{
		"status":  "error",
		"message": message,
	})
}

// respondInternalError logs the failed operation with its error and context attributes,
// then writes a 500 with clientMsg. Internal details stay in the log, never in the response.
func respondInternalError(w http.ResponseWriter, clientMsg, op string, err error, attrs ...any) {
	slog.Error(op, append(attrs, "error", err)...)
	respondError(w, http.StatusInternalServerError, clientMsg)
}

// Request body limits. JSON payloads here are a few hundred bytes. ADMS batches are about
// 60 bytes per punch, so 10 MB (~150k punches) is far beyond any real device buffer.
const (
	MaxJSONBodyBytes int64 = 64 << 10
	MaxADMSBodyBytes int64 = 10 << 20
)

// decodeJSONBody decodes a JSON body of at most MaxJSONBodyBytes into dst. It writes 413
// (logged as WARN) for an oversized body or 400 with badRequestMsg for malformed JSON,
// and reports whether decoding succeeded.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any, badRequestMsg string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, MaxJSONBodyBytes)
	err := json.NewDecoder(r.Body).Decode(dst)
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		return true
	case errors.As(err, &tooLarge):
		slog.Warn("Request body too large", "path", r.URL.Path, "remote_addr", r.RemoteAddr, "limit_bytes", tooLarge.Limit)
		respondError(w, http.StatusRequestEntityTooLarge, "Request body too large")
	default:
		respondError(w, http.StatusBadRequest, badRequestMsg)
	}
	return false
}

// respondRetry logs a transient failure and writes 503 with Retry-After, telling a client
// (such as an attendance device) to resend the same request later.
func respondRetry(w http.ResponseWriter, op string, err error, attrs ...any) {
	slog.Error(op, append(attrs, "error", err)...)
	w.Header().Set("Retry-After", "30")
	respondError(w, http.StatusServiceUnavailable, "Service temporarily unavailable, retry later")
}
