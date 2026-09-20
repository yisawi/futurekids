package handlers

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// ADMSCdataHandler handles the ZKTeco ADMS /iclock/cdata endpoint.
//
// The device uses this single endpoint for two distinct interactions:
//
//  1. GET  — Device registration / heartbeat on boot.
//     The device sends GET /iclock/cdata?SN=<serial> to discover the server.
//     The ADMS protocol requires HTTP 200 plain-text "OK" — anything else
//     (JSON, HTML, redirect, or non-200) causes the device to mark the server
//     unreachable and stop pushing logs.
//
//  2. POST — Data push (attendance logs, user records, etc.).
//     After registration the device POSTs tab-delimited records to the same
//     path with query params:
//     ?SN=<serial>&table=ATTLOG   → attendance records
//     ?SN=<serial>&table=USER     → employee records
//     ?SN=<serial>&table=OPERLOG  → operator-audit records
//     The body is newline-separated ADMS text lines.
//
// Route:    GET|POST /iclock/cdata
// Auth:     NONE — must be entirely public; device firmware cannot carry JWTs.
// Response: 200 text/plain "OK" for every valid request.
func ADMSCdataHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sn := q.Get("SN")
	table := q.Get("table")
	cmd := q.Get("c")

	// ── GET: registration / heartbeat ────────────────────────────────────────
	if r.Method == http.MethodGet {
		slog.Info("ZKTeco ADMS handshake/heartbeat",
			"method", r.Method,
			"device_sn", sn,
			"table", table,
			"command", cmd,
			"remote_addr", r.RemoteAddr,
		)
		writeADMSOK(w)
		return
	}

	// ── POST: device is pushing data ─────────────────────────────────────────
	if r.Method == http.MethodPost {
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			slog.Error("ZKTeco ADMS: failed to read POST body",
				"device_sn", sn,
				"error", err,
			)
			writeADMSOK(w) // still respond OK so the device does not stall
			return
		}

		body := strings.TrimSpace(string(rawBody))
		lines := strings.Split(body, "\n")

		slog.Info("ZKTeco ADMS data push received",
			"method", r.Method,
			"device_sn", sn,
			"table", table,
			"command", cmd,
			"remote_addr", r.RemoteAddr,
			"line_count", len(lines),
		)

		// Log each record individually so they appear as distinct log entries
		// and are easy to grep/filter by table type.
		for i, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			slog.Info("ZKTeco ADMS record",
				"device_sn", sn,
				"table", table,
				"index", i,
				"record", line,
			)
		}

		writeADMSOK(w)
		return
	}

	// ── Any other method is unexpected ───────────────────────────────────────
	slog.Warn("ZKTeco ADMS: unexpected HTTP method",
		"method", r.Method,
		"device_sn", sn,
		"remote_addr", r.RemoteAddr,
	)
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
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
