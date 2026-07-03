package paho

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sync"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// Session implements ports.Session for MQTT, owning the broker connection,
// ClientID identity, and subscription reconciliation.
type Session struct {
	opts    SessionOptions
	mode    connectivity.SessionMode
	logger  *slog.Logger
	metrics ports.MetricsExporter
	clk     clock.Clock

	mu sync.Mutex
	// cm is the live connection seam. Defined as the unexported
	// pahoConnection interface (see acl_client.go) so this file does
	// not import the vendor SDK. Tests assign a *pahoConn wrapping a
	// sentinel autopaho.ConnectionManager when they only need a
	// non-nil presence.
	cm        pahoConnection
	cmCancel  context.CancelFunc // cancels the CM's background context on Close
	events    chan ports.SessionEvent
	closed    bool
	connected bool // true when autopaho reports connection is up
	starting  bool // true while a Start() attempt is in flight
	// startDone is closed when the in-flight Start attempt finishes
	// (success or failure) so concurrent Start callers can wait for the
	// outcome instead of returning a false success (finding: concurrent
	// Start returned nil while the winner was still connecting).
	// Replaced with a fresh channel each time a Start attempt begins.
	startDone chan struct{}

	// takeoverStreak counts consecutive session-takeover disconnects
	// (0x8E) without an intervening stable connection; connUpAt is when
	// the current connection came up. Both guarded by mu; used to damp
	// ClientID-collision takeover storms.
	takeoverStreak int
	connUpAt       int64 // unix nanos of last OnConnectionUp; 0 when down

	// reconcileMu serializes concurrent Reconcile calls so their
	// subscribe/unsubscribe operations cannot interleave and corrupt
	// activeSubs (e.g. the SessionManager-driven reconcile on
	// SessionConnected racing a rotation- or caller-triggered Reconcile).
	// Per finding C7 the SessionManager is the single owner of
	// reconciliation; OnConnectionUp (handleConnectionUp) does NOT
	// reconcile inline — it only resets activeSubs and signals the
	// manager — so it is intentionally not a holder of this mutex (see
	// handleConnectionUp for why it must not block on it).
	reconcileMu sync.Mutex

	// router receives all incoming publishes; Receivers register handlers.
	router *router

	// plan is the last reconciled session plan, re-applied on reconnect.
	plan *connectivity.SessionPlan

	// activeSubs tracks topics for which SUBSCRIBE has been issued.
	activeSubs map[string]byte // topic -> qos

	// liveCreds is the most recently applied credential material. It is
	// consulted by the ConnectPacketBuilder on every (re)connect so that
	// rotated credentials take effect on the NEXT CONNECT packet
	// without rebuilding the ConnectionManager. Protected by mu.
	liveCreds mqttCredentials
}

// mqttCredentials is the mutable subset of SessionOptions that can be
// rotated at runtime. TLS material is intentionally out of scope for
// now — rotating a TLS certificate requires a new TLS handshake which
// autopaho does not support on an existing CM.
type mqttCredentials struct {
	Username string
	Password string
}

var _ ports.Session = (*Session)(nil)

// NewSession creates an MQTT Session from the given options.
// metrics may be nil; a no-op exporter is used in that case.
//
// For Persistent/Exclusive modes a zero SessionExpiryInterval is
// coerced to DefaultPersistentSessionExpiry (with a warning): expiry 0
// means the broker discards session state the moment the network drops,
// which silently voids the offline-retention contract those modes exist
// to provide.
func NewSession(opts SessionOptions, mode connectivity.SessionMode, logger *slog.Logger, metrics ...ports.MetricsExporter) *Session {
	var m ports.MetricsExporter = &ports.NoopExporter{}
	if len(metrics) > 0 && metrics[0] != nil {
		m = metrics[0]
	}
	if opts.Clock == nil {
		opts.Clock = clock.System
	}
	if mode != connectivity.SessionEphemeral && opts.SessionExpiryInterval == 0 {
		opts.SessionExpiryInterval = DefaultPersistentSessionExpiry
		if logger != nil {
			logger.Warn("mqtt: session_expiry_interval 0 gives zero offline retention; defaulting",
				"mode", string(mode),
				"session_expiry_interval", opts.SessionExpiryInterval,
			)
		}
	}
	s := &Session{
		opts:       opts,
		mode:       mode,
		logger:     logger,
		metrics:    m,
		clk:        opts.Clock,
		events:     make(chan ports.SessionEvent, 16),
		activeSubs: make(map[string]byte),
	}
	// The router shares the session's (possibly fake) clock so the startup
	// grace window is deterministic under test, and calls back through
	// s.unsubscribeOrphan to converge broker state for orphan topics.
	r := newRouter(logger, m,
		withRouterClock(opts.Clock),
		withUnmatchedGrace(opts.UnmatchedGrace),
		withUnsubscribe(s.unsubscribeOrphan),
	)
	if opts.ReceiveMaximum > 0 {
		// Bound the pre-registration pending buffer by the same window
		// that bounds the broker's un-acked QoS 1/2 in-flight publishes.
		r.setPendingLimit(int(opts.ReceiveMaximum))
	}
	s.router = r
	return s
}

// ConnectionManager returns the underlying autopaho.ConnectionManager.
// Receiver and Sender use this to issue subscribe/publish calls.
func (s *Session) clock() clock.Clock {
	if s.clk != nil {
		return s.clk
	}
	return clock.System
}

// Router returns the message router for registering Receiver handlers.
func (s *Session) Router() *router {
	return s.router
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func parseURLs(raw []string) ([]*url.URL, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("at least one broker URL is required")
	}
	out := make([]*url.URL, len(raw))
	for i, s := range raw {
		u, err := url.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("invalid broker URL %q: %w", s, err)
		}
		out[i] = u
	}
	return out, nil
}
