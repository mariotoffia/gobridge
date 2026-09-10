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
	// acceptZeroDeliveryLoss OPTS OUT of the safe default: when true a
	// broadcast that reached nobody (no subscribers, or every buffer full)
	// is reported as an at-most-once SUCCESS and the route runner acks the
	// source. When false (the DEFAULT) zero delivery returns a TRANSIENT
	// (Unavailable-class) error so the source is retried/DLQ'd instead of
	// losing the event. Set from Config.AtMostOnceAcceptLoss.
	acceptZeroDeliveryLoss bool
	apiKey                 string
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
	// done is closed by this client's own handler when it stops reading
	// events (any exit path), BEFORE it deregisters under the lock. A
	// fan-out that observes done closed must NOT count an enqueue to this
	// client as delivery: the buffered channel would accept the write but
	// nobody will ever read it (issue 1). A nil done — hand-built test
	// clients — is never ready, so such a client is treated as live.
	done chan struct{}
}

// SSESender implements ports.Sender by broadcasting envelopes to connected SSE clients.
type SSESender struct {
	cfg     sseSenderConfig
	mu      sync.RWMutex
	clients map[string]*sseClient
	routeID string
	// closing is set true under mu by Close BEFORE it closes shutdown.
	// Send reads it under the read lock held across its fan-out, so the
	// ack decision is atomic with respect to Close: observing closing
	// false there orders the enqueues before Close, guaranteeing the
	// handlers drain them on shutdown rather than abandoning an already
	// acked event (issues 2 & 3).
	closing   bool
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
	s.closeOnce.Do(func() {
		// Mark closing UNDER THE LOCK before closing shutdown so a
		// concurrent Send either observes closing (and refuses to ack) or
		// completes its fan-out first — its enqueues then happen-before
		// this close and are drained by the handlers on shutdown (issue 2).
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		close(s.shutdown)
	})
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

// isClosing reports whether Close has begun (s.closing set under mu). Read
// under the lock so it is coherent with Close, which sets s.closing under
// the write lock before closing s.shutdown. A Send that observes this must
// fail CLOSED with a transient error instead of acking a broadcast whose
// subscribers Close is tearing down (issue 2). This is a cheap early-out;
// the race-free decision is the s.closing read under the fan-out lock in
// Send.
func (s *SSESender) isClosing() bool {
	s.mu.RLock()
	closing := s.closing
	s.mu.RUnlock()
	return closing
}

// shuttingDownErr is the TRANSIENT (Unavailable-class) error Send returns
// when a broadcast races Close. Transient so the route runner retries
// after the listener rebinds (or a durable source redelivers) rather than
// dropping the event during a config reload / shutdown window.
func (s *SSESender) shuttingDownErr(env *messaging.Envelope) error {
	return shared.ErrUnavailable.WithMessage(fmt.Sprintf(
		"sse: sender is shutting down, event not delivered (sender %q, envelope %q)",
		s.cfg.id, env.ID()))
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
// routing is deliberately out of scope: an SSE sender IS one channel,
// so a per-message channel would mean a per-message connection.
// The logical Envelope.Subject flows through
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

	// A broadcast that races Close (config reload / process shutdown) must
	// never be acked. Cheap early-out — checked BEFORE ctx so a
	// shutdown+cancel race surfaces as a retryable Unavailable rather than
	// Canceled. The AUTHORITATIVE, race-free decision is the s.closing read
	// under the fan-out lock below (issue 2).
	if s.isClosing() {
		return s.shuttingDownErr(env)
	}

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

	// Fan out under the READ LOCK, iterating the LIVE client map rather
	// than a released snapshot. Two correctness reasons:
	//   - issue 1: a snapshot lets a client deregister after the copy is
	//     taken yet still receive a phantom enqueue nobody reads, which the
	//     old code miscounted as delivery. Iterating the live map — and
	//     skipping any client whose handler has closed done — counts only
	//     enqueues to a still-reading subscriber.
	//   - issue 2: Close sets s.closing under the WRITE lock before it
	//     closes s.shutdown, so reading closing==false while holding this
	//     read lock orders every enqueue below BEFORE Close; those events
	//     are therefore drained by the handlers on shutdown (issue 3),
	//     never silently abandoned.
	// Every enqueue is non-blocking (select default), so holding the read
	// lock across the loop cannot stall.
	s.mu.RLock()
	if s.closing {
		s.mu.RUnlock()
		return s.shuttingDownErr(env)
	}
	total := len(s.clients)
	delivered := 0
	var droppedIDs []string
	for _, c := range s.clients {
		// A handler closes done when it stops reading; enqueuing to such a
		// client is lost, so it is not a live delivery target (issue 1). A
		// nil done (hand-built test client) is never ready → treated live.
		select {
		case <-c.done:
			continue
		default:
		}
		select {
		case c.events <- eventBytes:
			delivered++
		default:
			droppedIDs = append(droppedIDs, c.id)
		}
	}
	s.mu.RUnlock()

	for _, id := range droppedIDs {
		s.cfg.metrics.Counter(MetricSSEDroppedEvents, 1, s.tag())
		if s.cfg.logger != nil {
			s.cfg.logger.Warn("sse: client buffer full, dropping event",
				"client", id, "event_id", env.ID())
		}
	}

	if logging.TraceEnabled(s.cfg.logger) {
		s.cfg.logger.Log(ctx, logging.LevelTrace, "sse: broadcasting",
			"sender_id", s.cfg.id,
			"envelope_id", env.ID(),
			"client_count", total,
			"delivered", delivered,
		)
	}

	s.cfg.metrics.Timer(MetricSSEBroadcastLatency, s.cfg.clock.Since(start), s.tag())

	// SSE egress is SAFE-BY-DEFAULT: a broadcast that reached NO live
	// subscriber — none connected, or every one already gone/full —
	// returns a TRANSIENT (Unavailable-class) error so the route runner
	// does NOT ack the source. A durable source RETRIES (letting a
	// briefly-disconnected subscriber reconnect) then DLQs per policy; an
	// HTTP-ingress source surfaces HTTP 500 to the producer. Both cases are
	// counted and logged at ERROR level so the loss is loud. Only when the
	// operator EXPLICITLY opts into loss (Config.AtMostOnceAcceptLoss) does
	// Send report success and let the source be acked though the event
	// reached nobody.
	if total == 0 {
		s.cfg.metrics.Counter(MetricSSENoSubscribers, 1, s.tag())
		if s.cfg.logger != nil {
			s.cfg.logger.Error("sse: no subscribers connected, event not delivered",
				"sender_id", s.cfg.id, "envelope_id", env.ID())
		}
		if !s.cfg.acceptZeroDeliveryLoss {
			return shared.ErrUnavailable.WithMessage(fmt.Sprintf(
				"sse: zero delivery — no subscribers connected (sender %q, envelope %q)",
				s.cfg.id, env.ID()))
		}
		return nil
	}
	if delivered == 0 {
		// Every connected subscriber was gone or buffer-full: the event
		// reached nobody live. Keep the historical "all subscriber buffers
		// full" wording (the dominant cause) so operator log filters match.
		s.cfg.metrics.Counter(MetricSSEAllDropped, 1, s.tag())
		if s.cfg.logger != nil {
			s.cfg.logger.Error("sse: all subscriber buffers full, event delivered to nobody",
				"sender_id", s.cfg.id, "envelope_id", env.ID(), "client_count", total)
		}
		if !s.cfg.acceptZeroDeliveryLoss {
			return shared.ErrUnavailable.WithMessage(fmt.Sprintf(
				"sse: zero delivery — %d subscriber(s) gone or full (sender %q, envelope %q)",
				total, s.cfg.id, env.ID()))
		}
		return nil
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
		done:   make(chan struct{}),
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
		// Signal the fan-out FIRST — before deregistering — that this
		// handler has stopped reading, so a concurrent Send holding the
		// read lock skips our now-unread buffer instead of counting an
		// enqueue to it as a live delivery (issue 1).
		close(client.done)
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
	// The per-write deadline is the slow-client eviction mechanism.
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
		// Priority shutdown check. With several channels ready at once Go's
		// select picks uniformly at random, so a buffered event and a closed
		// shutdown could be served in either order. Check shutdown FIRST and
		// DRAIN already-enqueued (already-acked) events before returning, so
		// Close never abandons an event Send already reported as delivered
		// (issue 3).
		select {
		case <-s.shutdown:
			s.drainOnShutdown(w, rc, flusher, client)
			return
		default:
		}
		select {
		case <-ctx.Done():
			return
		case <-s.shutdown:
			// Graceful drain (Close): flush already-acked buffered events,
			// then unblock so http.Server.Shutdown can complete instead of
			// hanging on long-lived SSE streams.
			s.drainOnShutdown(w, rc, flusher, client)
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

// drainOnShutdown flushes events already enqueued to this client — events
// Send already reported as delivered — before the handler returns on
// Close, so a graceful shutdown does not silently drop an acked event
// (issue 3). It is a SINGLE non-blocking pass: Send refuses to enqueue
// once s.closing is set (Close sets it under the write lock before closing
// s.shutdown), so no new events arrive once shutdown is observed and the
// buffer is quiescent. The per-write deadline still bounds a stalled
// socket so a wedged reader cannot pin shutdown.
//
// ponytail: a client wedged mid-Write when Close fires cannot be drained —
// SSE is at-most-once with no per-subscriber durable queue, so an event
// already handed to a stuck socket is lost. Lifting this ceiling means
// durable per-client queues, which are explicitly out of scope.
func (s *SSESender) drainOnShutdown(w http.ResponseWriter, rc *http.ResponseController, flusher http.Flusher, client *sseClient) {
	for {
		select {
		case event := <-client.events:
			s.armWriteDeadline(rc)
			if _, err := w.Write(event); err != nil {
				return
			}
			flusher.Flush()
		default:
			return
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
//
// The data value is framed PER SSE RULES: a "data:" field is a single
// physical line, so any line terminator inside data is emitted as a new
// "data:" line (the client rejoins segments with "\n"). A single "data:"
// prefix in front of multi-line bytes would make EventSource keep only
// the first physical line and mis-parse the rest — silent data loss at
// the SSE boundary. json.Marshal already compacts the RawMessage
// payload (stripping structural newlines), but the formatter must not
// depend on the shape produced by its only caller.
func formatSSE(event string, data []byte) []byte {
	event = sanitizeSSEField(event)
	// Preallocate for the common single-line case; multi-line data grows
	// the slice by one "data: " prefix per extra segment.
	buf := make([]byte, 0, len("event: ")+len(event)+1+len("data: ")+len(data)+2)
	buf = append(buf, "event: "...)
	buf = append(buf, event...)
	buf = append(buf, '\n')
	buf = appendSSEData(buf, data)
	buf = append(buf, '\n', '\n')
	return buf
}

// appendSSEData appends data as one or more "data:" lines. Every SSE line
// terminator (LF, CR, or CRLF) starts a fresh "data:" line so the frame
// stays a valid single-event body no matter what bytes data carries.
func appendSSEData(buf, data []byte) []byte {
	buf = append(buf, "data: "...)
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '\n':
			buf = append(buf, "\ndata: "...)
		case '\r':
			buf = append(buf, "\ndata: "...)
			// Collapse CRLF into a single break rather than emitting an
			// empty extra data line.
			if i+1 < len(data) && data[i+1] == '\n' {
				i++
			}
		default:
			buf = append(buf, data[i])
		}
	}
	return buf
}
