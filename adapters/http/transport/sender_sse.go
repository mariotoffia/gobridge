package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Sender = (*SSESender)(nil)
var _ ports.ContextCloser = (*SSESender)(nil)

const defaultMaxSSEClients = 10000

type sseSenderConfig struct {
	id                string
	path              string
	heartbeatInterval time.Duration
	writeTimeout      time.Duration
	maxClients        int
	// clientBufferSize is the depth of each connected client's event
	// queue (Config.ClientBufferSize). A full queue drops the event for
	// that client rather than blocking the fan-out. Zero defaults to
	// defaultSSEClientBuffer in newSSESender.
	clientBufferSize int
	// failOnZeroDelivery makes Send return a transient error when a
	// broadcast reached nobody (no subscribers, or every buffer full)
	// instead of the default at-most-once success. See
	// Config.FailOnZeroDelivery.
	failOnZeroDelivery bool
	apiKey             string
	// redirectEndpoint is the PeerInfo.Endpoints key consulted for
	// cross-cluster SSE client redirects. Empty (the default) disables
	// redirecting entirely: a request for a remote route is refused with
	// 503 instead of leaking an internal peer endpoint in a 307 Location.
	redirectEndpoint string
	locator          ports.RouteLocator
	metrics          ports.MetricsExporter
	logger           *slog.Logger
	clock            clock.Clock
}

type sseClient struct {
	id     string
	events chan []byte
}

// SSESender implements ports.Sender by broadcasting envelopes to connected SSE clients.
type SSESender struct {
	cfg       sseSenderConfig
	mu        sync.RWMutex
	clients   map[string]*sseClient
	routeID   string
	shutdown  chan struct{}
	closeOnce sync.Once
}

func newSSESender(cfg sseSenderConfig) *SSESender {
	if cfg.heartbeatInterval == 0 {
		cfg.heartbeatInterval = 30 * time.Second
	}
	if cfg.writeTimeout == 0 {
		cfg.writeTimeout = defaultSSEWriteTimeout
	}
	if cfg.maxClients == 0 {
		cfg.maxClients = defaultMaxSSEClients
	}
	if cfg.clientBufferSize <= 0 {
		cfg.clientBufferSize = defaultSSEClientBuffer
	}
	if cfg.metrics == nil {
		cfg.metrics = &ports.NoopExporter{}
	}
	if cfg.clock == nil {
		cfg.clock = clock.System
	}
	return &SSESender{
		cfg:      cfg,
		clients:  make(map[string]*sseClient),
		shutdown: make(chan struct{}),
	}
}

// Close drains the sender for graceful shutdown: it unblocks every
// connected client handler (each ServeHTTP loop selects on the shutdown
// channel and returns) and then waits — bounded by ctx — until all
// clients have deregistered. Without this, http.Server.Shutdown hangs
// forever on the long-lived SSE handlers. It satisfies
// ports.ContextCloser; the composition root must invoke it (directly or
// via Factory.Close) BEFORE http.Server.Shutdown. Safe to call multiple
// times.
func (s *SSESender) Close(ctx context.Context) error {
	s.closeOnce.Do(func() { close(s.shutdown) })
	for {
		if s.ClientCount() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.cfg.clock.After(10 * time.Millisecond):
		}
	}
}

// ClientCount returns the number of currently connected SSE clients.
func (s *SSESender) ClientCount() int {
	s.mu.RLock()
	n := len(s.clients)
	s.mu.RUnlock()
	return n
}

// WaitClientConnected blocks until at least n clients are connected or ctx expires.
func (s *SSESender) WaitClientConnected(ctx context.Context, n int) error {
	for {
		if s.ClientCount() >= n {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.cfg.clock.After(10 * time.Millisecond):
		}
	}
}

// SetRouteID associates this sender with a route for cross-cluster SSE redirect.
func (s *SSESender) SetRouteID(routeID string) {
	s.mu.Lock()
	s.routeID = routeID
	s.mu.Unlock()
}

// identity returns the configured logical identity used to validate
// ports.OutboundMessage.Address. When SetRouteID has supplied a
// routeID, that value wins; otherwise the sender spec ID (cfg.id) is
// used.
func (s *SSESender) identity() string {
	s.mu.RLock()
	rid := s.routeID
	s.mu.RUnlock()
	if rid != "" {
		return rid
	}
	return s.cfg.id
}

// tag returns the owning-sender dimension stamped on every metric this
// sender emits, so multiple SSE senders in one process keep distinct
// series (notably the SSEClients gauge) instead of clobbering a shared
// one — see metrics.go (tagKeySenderID).
func (s *SSESender) tag() shared.Tag {
	return shared.Tag{Key: tagKeySenderID, Value: s.cfg.id}
}

// Send broadcasts an envelope to all connected SSE clients.
//
// Address validation: an SSESender is bound at construction to a
// single logical identity (its sender spec ID, optionally overridden
// by SetRouteID for cluster-aware routing). When msg.Address is
// empty, the configured identity is used. A non-empty msg.Address
// must match the configured identity exactly; any other value is
// rejected with shared.ErrInvalidTopic without marshalling, fan-out
// to clients, or metric emission. Per-message dynamic SSE channel
// routing is explicitly out of scope (Non-Goal in
// ARCHITECTURE_PLAN.md). The logical Envelope.Subject flows through
// to the SSE event payload's "subject" field unchanged.
func (s *SSESender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	env := msg.Envelope
	if env == nil {
		return shared.ErrInvalidPayload.WithMessage("sse: nil envelope")
	}
	identity := s.identity()
	if msg.Address != "" && msg.Address != identity {
		return shared.ErrInvalidTopic.WithMessage(fmt.Sprintf(
			"sse: address %q does not match configured identity %q",
			msg.Address, identity))
	}
	start := s.cfg.clock.Now()

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	data, err := json.Marshal(sseEvent{
		ID:      env.ID(),
		Subject: env.Subject(),
		Payload: env.Payload(),
		// Egress to a possibly-external subscriber: strip INTERNAL-ONLY
		// reserved headers (route-id, route-override, source-id,
		// content-type) so bridge dispatch bookkeeping never leaks.
		// Bridge-to-bridge propagated and application headers pass
		// through — a sender cannot tell a peer bridge from an external
		// client, so this is the safe default.
		Headers: messaging.StripInternalOnlyHeaders(env.Headers()),
	})
	if err != nil {
		return fmt.Errorf("sse: marshal event: %w", err)
	}

	eventBytes := formatSSE("message", data)

	s.mu.RLock()
	clients := make([]*sseClient, 0, len(s.clients))
	for _, c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.RUnlock()

	if logging.TraceEnabled(s.cfg.logger) {
		s.cfg.logger.Log(ctx, logging.LevelTrace, "sse: broadcasting",
			"sender_id", s.cfg.id,
			"envelope_id", env.ID(),
			"client_count", len(clients),
		)
	}

	// SSE egress is AT-MOST-ONCE by default (see doc.go): Send reports
	// success even when the event reached nobody, and the route runner
	// then acks the source. Both zero-delivery cases — no subscribers at
	// all, and every subscriber's buffer full — are counted and logged at
	// ERROR level so the loss is loud, never silent. When
	// FailOnZeroDelivery is set the zero-delivery cases instead return a
	// TRANSIENT (Unavailable-class) error so the runner RETRIES (letting a
	// briefly-disconnected subscriber reconnect) and then DLQs per the
	// route's retry-exhaustion policy.
	if len(clients) == 0 {
		s.cfg.metrics.Counter(MetricSSENoSubscribers, 1, s.tag())
		if s.cfg.logger != nil {
			s.cfg.logger.Error("sse: no subscribers connected, event not delivered",
				"sender_id", s.cfg.id, "envelope_id", env.ID())
		}
		s.cfg.metrics.Timer(MetricSSEBroadcastLatency, s.cfg.clock.Since(start), s.tag())
		if s.cfg.failOnZeroDelivery {
			return shared.ErrUnavailable.WithMessage(fmt.Sprintf(
				"sse: zero delivery — no subscribers connected (sender %q, envelope %q)",
				s.cfg.id, env.ID()))
		}
		return nil
	}

	dropped := 0
	for _, c := range clients {
		select {
		case <-ctx.Done():
			s.cfg.metrics.Timer(MetricSSEBroadcastLatency, s.cfg.clock.Since(start), s.tag())
			return ctx.Err()
		case c.events <- eventBytes:
		default:
			dropped++
			s.cfg.metrics.Counter(MetricSSEDroppedEvents, 1, s.tag())
			if s.cfg.logger != nil {
				s.cfg.logger.Warn("sse: client buffer full, dropping event",
					"client", c.id, "event_id", env.ID())
			}
		}
	}
	allDropped := dropped == len(clients)
	if allDropped {
		s.cfg.metrics.Counter(MetricSSEAllDropped, 1, s.tag())
		if s.cfg.logger != nil {
			s.cfg.logger.Error("sse: all subscriber buffers full, event delivered to nobody",
				"sender_id", s.cfg.id, "envelope_id", env.ID(), "client_count", len(clients))
		}
	}

	s.cfg.metrics.Timer(MetricSSEBroadcastLatency, s.cfg.clock.Since(start), s.tag())
	if allDropped && s.cfg.failOnZeroDelivery {
		return shared.ErrUnavailable.WithMessage(fmt.Sprintf(
			"sse: zero delivery — all %d subscriber buffers full (sender %q, envelope %q)",
			len(clients), s.cfg.id, env.ID()))
	}
	return nil
}

// ServeHTTP handles SSE client connections.
func (s *SSESender) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	rc := http.NewResponseController(w)

	if s.cfg.apiKey != "" && !checkAPIKey(r, s.cfg.apiKey) {
		writeUnauthorized(w, "invalid or missing API key")
		return
	}

	s.mu.RLock()
	rid := s.routeID
	s.mu.RUnlock()

	if rid != "" && s.cfg.locator != nil {
		node, local, err := s.cfg.locator.Locate(r.Context(), rid)
		if err != nil && s.cfg.logger != nil {
			s.cfg.logger.Warn("sse: route location failed, serving locally", "route", rid, "error", err)
		}
		if err == nil && !local && node != nil {
			// Cross-cluster redirect is OPT-IN (Config.RedirectEndpoint
			// names the PeerInfo.Endpoints key to redirect to). The
			// default is to refuse with 503: redirecting to the same
			// endpoint the internal forwarder uses would leak the
			// internal peer address to a possibly-external SSE client
			// in the 307 Location header.
			if s.cfg.redirectEndpoint == "" {
				writeError(w, http.StatusServiceUnavailable,
					"route is owned by another node and no redirect endpoint is configured")
				return
			}
			endpoint, ok := node.Endpoints[s.cfg.redirectEndpoint]
			if ok {
				if logging.DebugEnabled(s.cfg.logger) {
					s.cfg.logger.Log(context.Background(), logging.LevelDebug, "sse: redirecting to peer",
						"route_id", rid, "peer", node.InstanceID, "endpoint_key", s.cfg.redirectEndpoint)
				}
				http.Redirect(w, r, endpoint+s.cfg.path, http.StatusTemporaryRedirect)
				return
			}
			if s.cfg.logger != nil {
				s.cfg.logger.Warn("sse: peer lacks configured redirect endpoint, refusing",
					"route_id", rid, "peer", node.InstanceID, "endpoint_key", s.cfg.redirectEndpoint)
			}
			writeError(w, http.StatusServiceUnavailable,
				"route is owned by another node")
			return
		}
	}

	select {
	case <-s.shutdown:
		writeError(w, http.StatusServiceUnavailable, "sender is shutting down")
		return
	default:
	}

	clientID := generateClientID()
	client := &sseClient{
		id:     clientID,
		events: make(chan []byte, s.cfg.clientBufferSize),
	}

	s.mu.Lock()
	if len(s.clients) >= s.cfg.maxClients {
		s.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "connection limit reached")
		return
	}
	s.clients[clientID] = client
	count := len(s.clients)
	s.mu.Unlock()
	s.cfg.metrics.Gauge(MetricSSEClients, float64(count), s.tag())
	if logging.DebugEnabled(s.cfg.logger) {
		s.cfg.logger.Log(context.Background(), logging.LevelDebug, "sse: client connected",
			"client_id", clientID, "total_clients", count)
	}

	defer func() {
		s.mu.Lock()
		delete(s.clients, clientID)
		count := len(s.clients)
		s.mu.Unlock()
		s.cfg.metrics.Gauge(MetricSSEClients, float64(count), s.tag())
		if logging.DebugEnabled(s.cfg.logger) {
			s.cfg.logger.Log(context.Background(), logging.LevelDebug, "sse: client disconnected",
				"client_id", clientID, "total_clients", count)
		}
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.armWriteDeadline(rc)
	// The per-write deadline is the slow-client eviction mechanism (H4).
	// If the ResponseWriter chain does not support it (e.g. a fronting
	// middleware that wraps without Unwrap), eviction is inert and a
	// stalled reader would pin this goroutine. Emit a metric so the gap is
	// countable/alertable even when no logger is configured, and warn once
	// so it is also visible in logs.
	if s.cfg.writeTimeout > 0 && !s.deadlineProbe(rc) {
		s.cfg.metrics.Counter(MetricSSEDeadlineUnsupported, 1, s.tag())
		if s.cfg.logger != nil {
			s.cfg.logger.Warn("sse: per-write deadline unsupported by ResponseWriter; slow-client eviction disabled",
				"client_id", clientID)
		}
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	heartbeat := s.cfg.clock.NewTicker(s.cfg.heartbeatInterval)
	defer heartbeat.Stop()

	// Cluster ownership is resolved at connect time (above), but a
	// rebalance AFTER connect can move the route to another node. Without
	// a re-check the client keeps a live, heartbeating — yet event-less —
	// stream forever. Re-poll the locator on a ticker so a client attached
	// to a now-non-owner node is disconnected and reconnects into the
	// connect-time redirect/refuse path. Only armed when this sender is
	// cluster-aware (bound route + locator); otherwise recheckC stays nil
	// and its select arm never fires. The heartbeat interval is a fine
	// cadence — a stale stream is corrected within one heartbeat.
	var recheckC <-chan time.Time
	if rid != "" && s.cfg.locator != nil {
		recheck := s.cfg.clock.NewTicker(s.cfg.heartbeatInterval)
		defer recheck.Stop()
		recheckC = recheck.C()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.shutdown:
			// Graceful drain (Close): unblock the handler so
			// http.Server.Shutdown can complete instead of hanging on
			// long-lived SSE streams.
			return
		case <-recheckC:
			// A transient locator error must NOT disconnect a healthy
			// stream — only a definitive move away from this node does.
			if s.ownershipMovedAway(ctx, rid) {
				if logging.DebugEnabled(s.cfg.logger) {
					s.cfg.logger.Log(context.Background(), logging.LevelDebug,
						"sse: route ownership moved to another node, closing stream so client reconnects",
						"route_id", rid, "client_id", clientID)
				}
				return
			}
		case event := <-client.events:
			s.armWriteDeadline(rc)
			if _, err := w.Write(event); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C():
			s.armWriteDeadline(rc)
			if _, err := fmt.Fprintf(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ownershipMovedAway reports whether the bound route is no longer owned
// by this node. It is the periodic re-check that closes an SSE stream a
// cluster rebalance has stranded on a non-owner (the connect-time locate
// only runs once). A locator error is treated as "still ours": a
// transient discovery blip must not disconnect a healthy subscriber, so
// it is logged and the stream is kept.
func (s *SSESender) ownershipMovedAway(ctx context.Context, rid string) bool {
	_, local, err := s.cfg.locator.Locate(ctx, rid)
	if err != nil {
		if s.cfg.logger != nil {
			s.cfg.logger.Warn("sse: ownership re-check failed, keeping stream open",
				"route_id", rid, "error", err)
		}
		return false
	}
	return !local
}

// armWriteDeadline re-arms the per-write deadline on the underlying
// connection before each SSE frame. Re-arming on every write overrides a
// fronting server's global WriteTimeout — which would otherwise kill a
// healthy long-lived stream — while still bounding any single frame so a
// stalled client is evicted (the next Write fails and the handler
// returns) instead of pinning this goroutine. Best-effort: a writer
// without deadline support (e.g. httptest.ResponseRecorder) returns
// http.ErrNotSupported, which is ignored. The deadline argument uses the
// WALL clock (time.Now), NOT the injected clock: SetWriteDeadline is an
// OS/kernel socket deadline, so an offset/scaled test clock would set a
// nonsensical kernel deadline. In unit tests the writer (httptest
// recorder / fake) returns http.ErrNotSupported, so the value is inert
// there anyway; wall-clock is correct for production.
func (s *SSESender) armWriteDeadline(rc *http.ResponseController) {
	if s.cfg.writeTimeout <= 0 {
		return
	}
	_ = rc.SetWriteDeadline(time.Now().Add(s.cfg.writeTimeout)) //nolint:forbidigo // OS kernel socket deadline needs the real wall clock, not the injectable clock (see godoc above)
}

// deadlineProbe reports whether the underlying ResponseWriter actually
// supports write deadlines. It is called once at stream start so an
// unsupported writer (which makes armWriteDeadline a silent no-op, leaving
// slow-client eviction inert) can be surfaced to operators. Like
// armWriteDeadline it uses the wall clock — the value is only probed for
// support, never asserted on.
func (s *SSESender) deadlineProbe(rc *http.ResponseController) bool {
	return rc.SetWriteDeadline(time.Now().Add(s.cfg.writeTimeout)) == nil //nolint:forbidigo // OS kernel socket deadline needs the real wall clock, not the injectable clock (see godoc above)
}

type sseEvent struct {
	ID      string          `json:"id"`
	Subject string          `json:"subject"`
	Payload json.RawMessage `json:"payload"`
	Headers map[string]any  `json:"headers,omitempty"`
}

// formatSSE renders one SSE frame. It deliberately emits NO "id:" field:
// an id would make EventSource clients persist a Last-Event-ID and send
// it on reconnect, implying resumability this sender does not provide
// (there is no backlog or replay window — events broadcast while a
// client is disconnected are lost). Omitting the id keeps the wire
// contract honestly at-most-once; the envelope ID remains available in
// the JSON payload's "id" field. See doc.go "SSE delivery semantics".
func formatSSE(event string, data []byte) []byte {
	event = sanitizeSSEField(event)
	size := len("event: ") + len(event) + 1 +
		len("data: ") + len(data) + 2
	buf := make([]byte, 0, size)
	buf = append(buf, "event: "...)
	buf = append(buf, event...)
	buf = append(buf, '\n')
	buf = append(buf, "data: "...)
	buf = append(buf, data...)
	buf = append(buf, '\n', '\n')
	return buf
}
