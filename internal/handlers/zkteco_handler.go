package handlers

import (
	"log/slog"
	"net/http"
)

// ADMSHandshakeHandler handles the ZKTeco ADMS device handshake.
//
// The device sends a GET /iclock/cdata?SN=<serial> request immediately on boot
// to discover the server and confirm it is reachable. The ADMS protocol
// requires an HTTP 200 plain-text "OK" response — any other response (JSON,
// HTML, a redirect, or a non-200 status) causes the device to mark the server
// as unreachable and stop pushing attendance logs.
//
// Route:   GET /iclock/cdata
// Auth:    NONE — must be entirely public; the device has no JWT capability.
// Body:    none
// Response: 200 text/plain "OK"
func ADMSHandshakeHandler(w http.ResponseWriter, r *http.Request) {
	// Reject anything that is not a GET so accidental POSTs surface clearly
	// in logs rather than silently returning OK.
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sn := r.URL.Query().Get("SN")
	slog.Info("ZKTeco ADMS handshake received", "device_sn", sn, "remote_addr", r.RemoteAddr)

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
