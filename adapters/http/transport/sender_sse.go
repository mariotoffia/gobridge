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

const defaultMaxSSEClients = 10000

type sseSenderConfig struct {
	id                string
	path              string
	heartbeatInterval time.Duration
	writeTimeout      time.Duration
	maxClients        int
	apiKey            string
	locator           ports.RouteLocator
	metrics           ports.MetricsExporter
	logger            *slog.Logger
	clock             clock.Clock
}

type sseClient struct {
	id     string
	events chan []byte
}

// SSESender implements ports.Sender by broadcasting envelopes to connected SSE clients.
type SSESender struct {
	cfg     sseSenderConfig
	mu      sync.RWMutex
	clients map[string]*sseClient
	routeID string
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
	if cfg.metrics == nil {
		cfg.metrics = &ports.NoopExporter{}
	}
	if cfg.clock == nil {
		cfg.clock = clock.System
	}
	return &SSESender{
		cfg:     cfg,
		clients: make(map[string]*sseClient),
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

	eventBytes := formatSSE("message", env.ID(), data)

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

	for _, c := range clients {
		select {
		case <-ctx.Done():
			s.cfg.metrics.Timer(MetricSSEBroadcastLatency, s.cfg.clock.Since(start))
			return ctx.Err()
		case c.events <- eventBytes:
		default:
			s.cfg.metrics.Counter(MetricSSEDroppedEvents, 1)
			if s.cfg.logger != nil {
				s.cfg.logger.Warn("sse: client buffer full, dropping event",
					"client", c.id, "event_id", env.ID())
			}
		}
	}

	s.cfg.metrics.Timer(MetricSSEBroadcastLatency, s.cfg.clock.Since(start))
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
			httpEndpoint, ok := node.Endpoints["http"]
			if ok {
				if logging.DebugEnabled(s.cfg.logger) {
					s.cfg.logger.Log(context.Background(), logging.LevelDebug, "sse: redirecting to peer",
						"route_id", rid, "peer", node.InstanceID)
				}
				http.Redirect(w, r, httpEndpoint+s.cfg.path, http.StatusTemporaryRedirect)
				return
			}
		}
	}

	clientID := generateClientID()
	client := &sseClient{
		id:     clientID,
		events: make(chan []byte, 256),
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
	s.cfg.metrics.Gauge(MetricSSEClients, float64(count))
	if logging.DebugEnabled(s.cfg.logger) {
		s.cfg.logger.Log(context.Background(), logging.LevelDebug, "sse: client connected",
			"client_id", clientID, "total_clients", count)
	}

	defer func() {
		s.mu.Lock()
		delete(s.clients, clientID)
		count := len(s.clients)
		s.mu.Unlock()
		s.cfg.metrics.Gauge(MetricSSEClients, float64(count))
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
	if s.cfg.writeTimeout > 0 && !s.deadlineProbe(rc) && s.cfg.logger != nil {
		// The per-write deadline is the slow-client eviction mechanism
		// (H4). If the ResponseWriter chain does not support it (e.g. a
		// fronting middleware that wraps without Unwrap), eviction is inert
		// and a stalled reader would pin this goroutine. Warn once so the
		// gap is visible rather than silent.
		s.cfg.logger.Warn("sse: per-write deadline unsupported by ResponseWriter; slow-client eviction disabled",
			"client_id", clientID)
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	heartbeat := s.cfg.clock.NewTicker(s.cfg.heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
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

// armWriteDeadline re-arms the per-write deadline on the underlying
// connection before each SSE frame. Re-arming on every write overrides a
// fronting server's global WriteTimeout — which would otherwise kill a
// healthy long-lived stream — while still bounding any single frame so a
// stalled client is evicted (the next Write fails and the handler
// returns) instead of pinning this goroutine. Best-effort: a writer
// without deadline support (e.g. httptest.ResponseRecorder) returns
// http.ErrNotSupported, which is ignored. The deadline is sourced from
// the injected clock so tests stay deterministic.
func (s *SSESender) armWriteDeadline(rc *http.ResponseController) {
	if s.cfg.writeTimeout <= 0 {
		return
	}
	_ = rc.SetWriteDeadline(s.cfg.clock.Now().Add(s.cfg.writeTimeout))
}

// deadlineProbe reports whether the underlying ResponseWriter actually
// supports write deadlines. It is called once at stream start so an
// unsupported writer (which makes armWriteDeadline a silent no-op, leaving
// slow-client eviction inert) can be surfaced to operators.
func (s *SSESender) deadlineProbe(rc *http.ResponseController) bool {
	return rc.SetWriteDeadline(s.cfg.clock.Now().Add(s.cfg.writeTimeout)) == nil
}

type sseEvent struct {
	ID      string          `json:"id"`
	Subject string          `json:"subject"`
	Payload json.RawMessage `json:"payload"`
	Headers map[string]any  `json:"headers,omitempty"`
}

func formatSSE(event, id string, data []byte) []byte {
	id = sanitizeSSEField(id)
	event = sanitizeSSEField(event)
	size := len("id: ") + len(id) + 1 +
		len("event: ") + len(event) + 1 +
		len("data: ") + len(data) + 2
	buf := make([]byte, 0, size)
	buf = append(buf, "id: "...)
	buf = append(buf, id...)
	buf = append(buf, '\n')
	buf = append(buf, "event: "...)
	buf = append(buf, event...)
	buf = append(buf, '\n')
	buf = append(buf, "data: "...)
	buf = append(buf, data...)
	buf = append(buf, '\n', '\n')
	return buf
}
