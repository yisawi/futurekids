package handlers

import (
	"log/slog"
	"net/http"
)

// ADMSCdataHandler answers GET /iclock/cdata, the ZKTeco ADMS registration/heartbeat call
// a device makes on boot to discover the server. Attendance data arrives as POST
// /iclock/cdata, which is routed to AppEnv.ADMSHandler.
//
// The ADMS protocol requires HTTP 200 plain-text "OK" — anything else (JSON, HTML,
// redirect, or non-200) makes the device mark the server unreachable (RULES.md §5).
//
// Route:    GET /iclock/cdata
// Auth:     NONE — must be entirely public; device firmware cannot carry JWTs.
// Response: 200 text/plain "OK".
func ADMSCdataHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	slog.Info("ZKTeco ADMS handshake/heartbeat",
		"method", r.Method,
		"device_sn", q.Get("SN"),
		"table", q.Get("table"),
		"command", q.Get("c"),
		"remote_addr", r.RemoteAddr,
	)
	writeADMSOK(w)
}

// ADMSGetRequestHandler handles GET /iclock/getrequest.
//
// After registration the device polls this endpoint continuously to receive
// server-to-device commands (e.g. enroll user, delete user, get user list).
// An empty "OK" reply tells the device there are no pending commands.
//
// Route:    GET /iclock/getrequest
// Auth:     NONE — public, same reason as /iclock/cdata.
// Response: 200 text/plain "OK" (no pending command) or a raw ADMS command string.
func ADMSGetRequestHandler(w http.ResponseWriter, r *http.Request) {
	sn := r.URL.Query().Get("SN")

	slog.Info("ZKTeco ADMS command poll",
		"method", r.Method,
		"device_sn", sn,
		"remote_addr", r.RemoteAddr,
	)

	// TODO: query a commands table in the DB and return a pending command for
	// this device if one exists. For now, reply "OK" (no command).
	writeADMSOK(w)
}

// writeADMSOK writes the mandatory ADMS success response.
// The protocol requires exactly: HTTP 200, Content-Type text/plain, body "OK".
// Any deviation causes devices to consider the server unreachable.
func writeADMSOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// writeADMSRetry answers 503 so the device keeps the batch and resends it later. It is used
// only for transient server-side failures (database unavailable), never for bad data, which
// is always ACKed with writeADMSOK (RULES.md §5).
func writeADMSRetry(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Retry-After", "30")
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte("RETRY"))
}
