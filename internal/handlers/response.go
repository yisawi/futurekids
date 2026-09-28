package handlers

import (
	"encoding/json"
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
