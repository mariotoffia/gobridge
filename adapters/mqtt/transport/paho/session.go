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
	starting  bool // guards against concurrent Start() calls (BUG-2 fix)

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
func NewSession(opts SessionOptions, mode connectivity.SessionMode, logger *slog.Logger, metrics ...ports.MetricsExporter) *Session {
	var m ports.MetricsExporter = &ports.NoopExporter{}
	if len(metrics) > 0 && metrics[0] != nil {
		m = metrics[0]
	}
	if opts.Clock == nil {
		opts.Clock = clock.System
	}
	return &Session{
		opts:       opts,
		mode:       mode,
		logger:     logger,
		metrics:    m,
		clk:        opts.Clock,
		events:     make(chan ports.SessionEvent, 16),
		router:     newRouter(logger, m),
		activeSubs: make(map[string]byte),
	}
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
