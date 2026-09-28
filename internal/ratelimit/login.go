// Package ratelimit provides an in-memory, per-key limiter for failed login attempts.
package ratelimit

import (
	"sync"
	"time"
)

// Outcome is how a reserved login attempt ended.
type Outcome int

const (
	// Released means the attempt ended without a verdict (e.g. a server error); it is not counted.
	Released Outcome = iota
	// Failed means the credentials were wrong; it counts toward the limit.
	Failed
	// Succeeded means the credentials were correct; the key's counter is cleared.
	Succeeded
)

// LoginLimiter allows at most maxFailures failed attempts per key within a fixed
// window that starts at the key's first attempt. Once the limit is reached, every
// attempt for that key is refused until the window ends, even with correct credentials.
//
// Allow reserves a slot before the credentials are checked, so concurrent requests
// cannot exceed the limit. Every successful Allow must be paired with exactly one Finish.
// Expired entries are swept during normal calls, at most once per window.
type LoginLimiter struct {
	// Now returns the current time. Tests may replace it before first use.
	Now func() time.Time

	maxFailures int
	window      time.Duration

	mu        sync.Mutex
	entries   map[string]*entry
	lastSweep time.Time
}

type entry struct {
	start    time.Time
	failures int
	inFlight int
}

// NewLoginLimiter returns a limiter allowing maxFailures failed attempts per key per window.
func NewLoginLimiter(maxFailures int, window time.Duration) *LoginLimiter {
	return &LoginLimiter{
		Now:         time.Now,
		maxFailures: maxFailures,
		window:      window,
		entries:     make(map[string]*entry),
	}
}

// Allow reserves an attempt for key. When the key is locked it returns false and
// how long until the window ends.
func (l *LoginLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.Now()
	l.sweep(now)

	e, ok := l.entries[key]
	if !ok {
		e = &entry{start: now}
		l.entries[key] = e
	} else if !now.Before(e.start.Add(l.window)) {
		e.start, e.failures = now, 0
	}

	if e.failures+e.inFlight >= l.maxFailures {
		return false, e.start.Add(l.window).Sub(now)
	}
	e.inFlight++
	return true, 0
}

// Finish records the outcome of an attempt previously reserved with Allow.
func (l *LoginLimiter) Finish(key string, outcome Outcome) {
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[key]
	if !ok {
		return
	}
	if e.inFlight > 0 {
		e.inFlight--
	}
	switch outcome {
	case Failed:
		e.failures++
	case Succeeded:
		e.failures = 0
		if e.inFlight == 0 {
			delete(l.entries, key)
		}
	}
}

// Len reports how many keys are currently tracked.
func (l *LoginLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// sweep removes expired, idle entries. The caller must hold l.mu.
func (l *LoginLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < l.window {
		return
	}
	l.lastSweep = now
	for key, e := range l.entries {
		if e.inFlight == 0 && !now.Before(e.start.Add(l.window)) {
			delete(l.entries, key)
		}
	}
}
