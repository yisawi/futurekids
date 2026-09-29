// Package background runs fire-and-forget work (push notifications, notification history,
// device last_sync updates) so that shutdown can wait for it before closing the database.
package background

import (
	"context"
	"log/slog"
	"sync"
)

// Group tracks goroutines started with Go. The zero value is ready to use. A nil *Group
// still runs work, untracked, so code that is not wired to a Group keeps working.
type Group struct {
	mu      sync.Mutex
	running int
	idle    chan struct{}
}

// Go runs fn in a new goroutine, recovering (and logging) a panic, and tracks it until it returns.
func (g *Group) Go(name string, fn func()) {
	if g != nil {
		g.mu.Lock()
		g.running++
		g.mu.Unlock()
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("background: recovered panic", "task", name, "panic", r)
			}
			if g != nil {
				g.mu.Lock()
				g.running--
				if g.running == 0 && g.idle != nil {
					close(g.idle)
					g.idle = nil
				}
				g.mu.Unlock()
			}
		}()
		fn()
	}()
}

// Running reports how many tracked goroutines have not returned yet.
func (g *Group) Running() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.running
}

// Wait blocks until every tracked goroutine has returned or ctx is done. On timeout it
// returns ctx.Err() and the number of goroutines still running.
func (g *Group) Wait(ctx context.Context) (pending int, err error) {
	if g == nil {
		return 0, nil
	}
	g.mu.Lock()
	if g.running == 0 {
		g.mu.Unlock()
		return 0, nil
	}
	if g.idle == nil {
		g.idle = make(chan struct{})
	}
	idle := g.idle
	g.mu.Unlock()

	select {
	case <-idle:
		return 0, nil
	case <-ctx.Done():
		return g.Running(), ctx.Err()
	}
}
