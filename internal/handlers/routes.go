package handlers

import (
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"

	"future_kids/internal/clientip"
)

// RouteErrors answers the requests mux has no handler for with the JSON error envelope: 404
// for an unknown path, and 405 with an Allow header for a known path called with the wrong
// method. The mux decides this before any handler or middleware runs, so a wrong method is
// reported before authentication. /health and /iclock/* keep the mux's plain-text answers,
// since attendance devices must see exactly what they always have (RULES.md §5).
func RouteErrors(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if pattern != "" || plainRouteErrors(r.URL.Path) {
			mux.ServeHTTP(w, r)
			return
		}
		rec := &routeErrorRecorder{header: http.Header{}}
		h.ServeHTTP(rec, r)
		switch rec.status {
		case http.StatusNotFound:
			respondError(w, http.StatusNotFound, "Not found")
		case http.StatusMethodNotAllowed:
			w.Header().Set("Allow", rec.header.Get("Allow"))
			respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		default:
			for k, v := range rec.header {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.status)
			w.Write([]byte(rec.body.String()))
		}
	})
}

func plainRouteErrors(path string) bool {
	return path == "/health" || path == "/iclock" || strings.HasPrefix(path, "/iclock/")
}

type routeErrorRecorder struct {
	header http.Header
	status int
	body   strings.Builder
}

func (r *routeErrorRecorder) Header() http.Header { return r.header }

func (r *routeErrorRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *routeErrorRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}

// ObservedClientRequests is how many requests after startup LogObservedClients records.
const ObservedClientRequests = 20

// LogObservedClients logs, for the first ObservedClientRequests requests after startup, the
// connection's remote address, the X-Real-IP and X-Forwarded-For headers it carried, and the
// client IP the resolver derives from them, so TRUSTED_PROXY_CIDRS can be set from what the
// deployment's proxy really sends. Responses are not affected.
func LogObservedClients(next http.Handler, resolver clientip.Resolver) http.Handler {
	var seen atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := seen.Add(1); n <= ObservedClientRequests {
			ip, identified := resolver.Resolve(r)
			slog.Info("Observed client address",
				"request", n,
				"path", r.URL.Path,
				"remote_addr", r.RemoteAddr,
				"x_real_ip", logValue(r.Header.Get("X-Real-IP")),
				"x_forwarded_for", logValue(strings.Join(r.Header.Values("X-Forwarded-For"), ", ")),
				"client_ip", ip,
				"client_identified", identified,
			)
		}
		next.ServeHTTP(w, r)
	})
}
