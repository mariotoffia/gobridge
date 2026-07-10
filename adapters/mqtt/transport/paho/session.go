package paho

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"sync/atomic"

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
	cm       pahoConnection
	cmCancel context.CancelFunc // cancels the CM's background context on Close
	events   chan ports.SessionEvent
	// eventsClosed guards the single close of s.events. TWO paths close
	// it — Close (terminal shutdown) and Reload's Start-failure signal
	// (F-1: closing events routes the dead session into the runtime
	// manager's events-channel-close restart path). Both honor this flag
	// under s.mu so a double-close cannot panic, and pushEvent checks it
	// so no send can race the close. Start clears it (and re-materialises
	// s.events) when the supervisor re-Starts a Reload-failed session.
	eventsClosed bool
	closed       bool
	connected    bool // true when autopaho reports connection is up
	starting     bool // true while a Start() attempt is in flight
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

	// plan is the last DESIRED session plan (the target the most recent
	// Reconcile was asked to reach). It is stashed BEFORE the broker ops run
	// so it is set even when Reconcile-before-Start returns an error (the
	// stash OnConnectionUp replays), and it drives topicCovered. It is NOT the
	// applied state — see appliedPlan.
	plan *connectivity.SessionPlan

	// appliedPlan is the last plan whose broker Subscribe/Unsubscribe ops
	// ACTUALLY SUCCEEDED — the "last successfully reconciled" state, distinct
	// from the desired plan above (blocking-#2). A FAILED reconcile (e.g. an
	// Unsubscribe that times out) must NOT be committed as applied history:
	// the empty-plan no-op short-circuit and the reconnect-window teardown
	// both key off appliedPlan, so keeping it at the last SUCCESSFUL value is
	// what makes the NEXT Reconcile RETRY the failed op instead of no-oping.
	// nil until the first reconcile succeeds. Guarded by mu.
	appliedPlan *connectivity.SessionPlan

	// sharedSubWarned latches the one-time advisory that shared
	// subscriptions ($share) are configured on a stable/shared-ClientID
	// mode — the client_id-collision footgun for scale-out (HIGH-3). Guarded
	// by mu; deduplicated so the warning fires once per session lifetime, not
	// on every reconcile.
	sharedSubWarned bool

	// outboxWarned latches the one-time advisory that outbound QoS 1/2 packet
	// state is IN-MEMORY (autopaho), so durable egress needs the bridge's
	// shared_outbox / idempotent replay, not MQTT session durability alone
	// (HIGH-5). Guarded by mu; deduplicated so it fires once per session even
	// when many QoS 1/2 senders bind to it.
	outboxWarned bool

	// activeSubs tracks topics for which SUBSCRIBE has been issued.
	activeSubs map[string]byte // topic -> qos

	// liveCreds is the most recently applied credential material. It is
	// consulted by the ConnectPacketBuilder on every (re)connect so that
	// rotated credentials take effect on the NEXT CONNECT packet
	// without rebuilding the ConnectionManager. Protected by mu.
	liveCreds mqttCredentials

	// connectOverride, when non-nil, replaces the real autopaho dial in
	// Start (build ClientConfig → NewConnection → AwaitConnection) with a
	// test double. It returns the connection seam plus the cancel func
	// Close/Reload use to tear down the CM's background context. Kept
	// SDK-free (pahoConnection, not *autopaho.ConnectionManager) so this
	// file need not import the vendor SDK. Production leaves it nil; the
	// real dial lives in acl_session.go.
	connectOverride func(ctx context.Context) (pahoConnection, context.CancelFunc, error)

	// authFailureCB is the reactive-recovery hook (HIGH-3). The
	// CredentialRefresher injects a URI-bound callback via
	// SetAuthFailureCallback; reportAuthFailure invokes it when a live CONNECT
	// is rejected with an authorization failure (CONNACK 0x86/0x87 →
	// shared.ErrNotAuthorized), forcing an immediate re-resolve rather than
	// stalling on revoked credentials until the next poll. atomic.Pointer gives
	// safe publication across the builder goroutine (setter) and autopaho's
	// OnConnectError callback goroutine (load).
	authFailureCB atomic.Pointer[func(error)]
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

// sessionEventsBuffer is the capacity of the session lifecycle-event
// channel. It is a named constant so the Reload-failure re-Start
// (acl_session.go) can re-materialise a channel with the SAME capacity
// it was constructed with (F-1).
const sessionEventsBuffer = 16

// NewSession creates an MQTT Session from the given options.
// metrics may be nil; a no-op exporter is used in that case.
//
// For Persistent/Exclusive modes a zero SessionExpiryInterval is
// coerced to DefaultPersistentSessionExpiry (with a warning): expiry 0
// means the broker discards session state the moment the network drops,
// which silently voids the offline-retention contract those modes exist
// to provide.
//
// A zero ReceiveMaximum is coerced to DefaultReceiveMaximum. 0 is not a
// legal MQTT v5 Receive Maximum (protocol error), so it can only mean
// "unset"; the effective default bounds worst-case buffered memory (see
// DefaultReceiveMaximum and the ReceiveMaximum doc comment).
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
	if opts.ReceiveMaximum == 0 {
		// ponytail: BEHAVIOR CHANGE — the effective Receive Maximum default
		// was lowered from the MQTT protocol maximum (65535) to
		// DefaultReceiveMaximum (1024) to bound worst-case buffered memory
		// (receive_maximum × max_payload; paho buffers up to Receive Maximum
		// full publishes, and the startup pending buffer is sized to it too).
		// 0 is not a legal MQTT v5 Receive Maximum, so coercing it is
		// correct. Operators who want the old ceiling set receive_maximum
		// explicitly.
		opts.ReceiveMaximum = DefaultReceiveMaximum
		if logger != nil {
			logger.Warn("mqtt: receive_maximum unset; defaulting to bound worst-case buffered "+
				"memory (receive_maximum × max_payload) at the cost of QoS 1/2 in-flight "+
				"parallelism — set receive_maximum explicitly (e.g. 65535) to restore the prior ceiling",
				"receive_maximum", opts.ReceiveMaximum,
			)
		}
	}
	s := &Session{
		opts:       opts,
		mode:       mode,
		logger:     logger,
		metrics:    m,
		clk:        opts.Clock,
		events:     make(chan ports.SessionEvent, sessionEventsBuffer),
		activeSubs: make(map[string]byte),
	}
	// The router shares the session's (possibly fake) clock so the startup
	// grace window is deterministic under test, and calls back through
	// s.unsubscribeOrphan to converge broker state for orphan topics. The
	// covered-topic predicate lets the router distinguish a real live-route
	// loss (a still-desired subscription whose handler registers late) from
	// benign orphan cleanup when it acks-and-drops past the grace window.
	r := newRouter(logger, m,
		withRouterClock(opts.Clock),
		withUnmatchedGrace(opts.UnmatchedGrace),
		withUnsubscribe(s.unsubscribeOrphan),
		withCovered(s.topicCovered),
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

// warnOutboxDurabilityOnce logs, at most once per session, the HIGH-5 egress
// durability boundary for QoS 1/2 senders: autopaho keeps outbound packet state
// IN MEMORY (see the store note in acl_session.go), so publishes in flight at
// process death are LOST and QoS 2 is not exactly-once across a restart. Durable
// at-least-once egress is the bridge's shared_outbox / idempotent-replay
// responsibility, not the MQTT session's — surface that so QoS 1/2 is not
// mistaken for durable egress on its own. Deduplicated so binding many senders
// to one session does not spam the advisory; QoS 0 senders never call it.
func (s *Session) warnOutboxDurabilityOnce(logger *slog.Logger, senderID string, qos byte) {
	if logger == nil {
		return
	}
	s.mu.Lock()
	first := !s.outboxWarned
	s.outboxWarned = true
	s.mu.Unlock()
	if !first {
		return
	}
	logger.Warn("mqtt: outbound QoS 1/2 packet state is IN-MEMORY (autopaho) — publishes in flight at process "+
		"death are LOST and QoS 2 is not exactly-once across a restart; durable egress requires the bridge's "+
		"shared_outbox / idempotent replay, not MQTT session durability alone",
		"session_id", s.opts.ClientID,
		"sender_id", senderID,
		"qos", qos,
	)
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
