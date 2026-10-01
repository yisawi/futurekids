package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// dummyHash is compared against when an account does not exist, so an unknown account costs
// the same bcrypt work as a wrong secret and response time does not reveal which accounts exist.
var dummyHash = sync.OnceValue(func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("future-kids-no-such-account"), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return h
})

func burnBcrypt(secret string) {
	_ = bcrypt.CompareHashAndPassword(dummyHash(), []byte(secret))
}

// Admin login limits: failures per username from one client IP, and failures from one client
// IP across all usernames. Only failures count and both windows are fixed, so an attacker
// can never lock the admin out from an IP the attacker has not used.
const (
	AdminFailuresPerUserIP = 5
	AdminFailuresPerIP     = 20
	LoginWindow            = 15 * time.Minute
)

func limiterKey(prefix, s string) string {
	sum := sha256.Sum256([]byte(s))
	return prefix + hex.EncodeToString(sum[:12])
}

func logValue(s string) string {
	if len(s) > 64 {
		return s[:64] + "…"
	}
	return s
}

func respondTooManyAttempts(w http.ResponseWriter, retryAfter time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Max(1, math.Ceil(retryAfter.Seconds())))))
	respondError(w, http.StatusTooManyRequests, "Too many failed login attempts. Try again later.")
}

func logRateLimited(handler string, r *http.Request, attrs ...any) {
	slog.Warn(handler+": login rate-limited", append([]any{"path", r.URL.Path}, attrs...)...)
}
