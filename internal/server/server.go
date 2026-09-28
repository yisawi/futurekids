// Package server builds the HTTP server with connection-level limits.
package server

import (
	"net/http"
	"time"
)

// Timeouts bound how long a client may take to send a request and receive a response.
type Timeouts struct {
	ReadHeader time.Duration
	Read       time.Duration
	Write      time.Duration
	Idle       time.Duration
}

// DefaultTimeouts: ReadHeader stops slow-header (slowloris) clients early; Read covers the
// largest ADMS batch on a slow link; Write bounds the slowest handler (Excel export);
// Idle frees keep-alive connections that are no longer used.
var DefaultTimeouts = Timeouts{
	ReadHeader: 10 * time.Second,
	Read:       30 * time.Second,
	Write:      30 * time.Second,
	Idle:       60 * time.Second,
}

// MaxHeaderBytes caps request headers; the largest legitimate header is a JWT of a few hundred bytes.
const MaxHeaderBytes = 64 << 10

// New returns an http.Server for addr and h with the given timeouts and header limit.
func New(addr string, h http.Handler, t Timeouts) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: t.ReadHeader,
		ReadTimeout:       t.Read,
		WriteTimeout:      t.Write,
		IdleTimeout:       t.Idle,
		MaxHeaderBytes:    MaxHeaderBytes,
	}
}
