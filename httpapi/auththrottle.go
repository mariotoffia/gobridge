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
	// audited records whether the throttle-begin audit event has already been
	// emitted for the current window, so repeated rejections while throttled do
	// not flood the audit log (see shouldAuditThrottle).
	audited bool
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
		w.audited = false
	}
	w.failures++
}

// shouldAuditThrottle reports whether a throttling rejection for this client
// should emit an audit event, and marks the window as audited when it returns
// true. To keep the operator signal without letting a brute-forcer write audit
// records at request line-rate, it fires only ONCE per window — the moment
// throttling begins for the client. A fresh window (rolled over by
// recordFailure) re-arms the signal so a renewed burst is still observed. It
// must be called only after throttled() has returned true.
func (t *authThrottle) shouldAuditThrottle(client string) bool {
	if t == nil {
		return false
	}
	now := t.clk.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.clients[client]
	if w == nil || now.Sub(w.windowStart) >= t.window {
		// No active window: throttling is not actually in effect, so there is
		// nothing to audit (throttled() would also have returned false).
		return false
	}
	if w.audited {
		return false
	}
	w.audited = true
	return true
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

// retryAfterSeconds returns the Retry-After hint (in whole seconds) a throttled
// client should wait, derived from the configured failure window rather than a
// hardcoded constant so it tracks AuthFailureWindow. The window is rounded up so
// the hint never advises a retry before the window can actually elapse, and it
// is floored at 1 so a sub-second window still yields a valid header value.
func (t *authThrottle) retryAfterSeconds() int {
	window := defaultAuthFailureWindow
	if t != nil && t.window > 0 {
		window = t.window
	}
	secs := int((window + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return secs
}

// pruneLocked drops entries whose window has fully elapsed. Caller holds mu.
func (t *authThrottle) pruneLocked(now time.Time) {
	for k, w := range t.clients {
		if now.Sub(w.windowStart) >= t.window {
			delete(t.clients, k)
		}
	}
}
