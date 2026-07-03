package httpapi

import (
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
)

const (
	// defaultAuthFailureLimit is the number of failed authentication attempts a
	// single client may make within a window before being throttled.
	defaultAuthFailureLimit = 5
	// defaultAuthFailureWindow is the fixed window over which failed attempts
	// are counted.
	defaultAuthFailureWindow = time.Minute
	// authThrottleMaxClients bounds the tracked-client map so a spray of
	// distinct source IPs cannot grow it without limit; stale entries are
	// pruned first, and past the cap new clients are simply not tracked
	// (fail-open on memory pressure rather than unbounded growth).
	authThrottleMaxClients = 4096
)

// authThrottle is a small, clock-driven, fixed-window rate limiter for failed
// authentication attempts, keyed per client. It exists to make credential
// brute-forcing against the admin/monitor API both visible (paired with an
// audit event by the caller) and expensive. It is intentionally minimal: a
// fixed window, not a token bucket, is sufficient to blunt online guessing.
type authThrottle struct {
	clk    clock.Clock
	limit  int
	window time.Duration

	mu      sync.Mutex
	clients map[string]*authWindow
}

type authWindow struct {
	windowStart time.Time
	failures    int
}

func newAuthThrottle(clk clock.Clock, limit int, window time.Duration) *authThrottle {
	if clk == nil {
		clk = clock.System
	}
	if limit <= 0 {
		limit = defaultAuthFailureLimit
	}
	if window <= 0 {
		window = defaultAuthFailureWindow
	}
	return &authThrottle{
		clk:     clk,
		limit:   limit,
		window:  window,
		clients: make(map[string]*authWindow),
	}
}

// throttled reports whether the client has exceeded the failure limit within
// the current window. It does not mutate the failure count.
func (t *authThrottle) throttled(client string) bool {
	if t == nil {
		return false
	}
	now := t.clk.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.clients[client]
	if w == nil {
		return false
	}
	if now.Sub(w.windowStart) >= t.window {
		return false // window elapsed; a fresh attempt is allowed
	}
	return w.failures >= t.limit
}

// recordFailure increments the client's failure counter, opening a new window
// when the previous one has elapsed.
func (t *authThrottle) recordFailure(client string) {
	if t == nil {
		return
	}
	now := t.clk.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.clients[client]
	if w == nil {
		if len(t.clients) >= authThrottleMaxClients {
			t.pruneLocked(now)
		}
		if len(t.clients) >= authThrottleMaxClients {
			return // fail-open: cannot track more clients
		}
		w = &authWindow{windowStart: now}
		t.clients[client] = w
	}
	if now.Sub(w.windowStart) >= t.window {
		w.windowStart = now
		w.failures = 0
	}
	w.failures++
}

// recordSuccess clears any failure state for the client on a successful auth.
func (t *authThrottle) recordSuccess(client string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.clients, client)
	t.mu.Unlock()
}

// pruneLocked drops entries whose window has fully elapsed. Caller holds mu.
func (t *authThrottle) pruneLocked(now time.Time) {
	for k, w := range t.clients {
		if now.Sub(w.windowStart) >= t.window {
			delete(t.clients, k)
		}
	}
}
