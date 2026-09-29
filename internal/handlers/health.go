package handlers

import "net/http"

// HealthHandler reports that the server is up. It does not touch the database, so a
// database outage does not make the platform restart a healthy process.
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]string{"status": "success", "message": "Server is healthy and running!"})
}
