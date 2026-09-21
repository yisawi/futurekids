package handlers

import (
	"encoding/json"
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
