package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/bridge/logging"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Sender = (*SSESender)(nil)

const defaultMaxSSEClients = 10000

type sseSenderConfig struct {
	id                string
	path              string
	heartbeatInterval time.Duration
	maxClients        int
	apiKey            string
	locator           ports.RouteLocator
	metrics           ports.MetricsExporter
	logger            *slog.Logger
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
	if cfg.maxClients == 0 {
		cfg.maxClients = defaultMaxSSEClients
	}
	if cfg.metrics == nil {
		cfg.metrics = &ports.NoopExporter{}
	}
	return &SSESender{
		cfg:     cfg,
		clients: make(map[string]*sseClient),
	}
}

// SetRouteID associates this sender with a route for cross-cluster SSE redirect.
func (s *SSESender) SetRouteID(routeID string) {
	s.mu.Lock()
	s.routeID = routeID
	s.mu.Unlock()
}

// Send broadcasts an envelope to all connected SSE clients.
func (s *SSESender) Send(ctx context.Context, env *domain.Envelope) error {
	start := time.Now()

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	data, err := json.Marshal(sseEvent{
		ID:      env.ID,
		Subject: env.Subject,
		Payload: env.Payload,
		Headers: env.Headers,
	})
	if err != nil {
		return fmt.Errorf("sse: marshal event: %w", err)
	}

	eventBytes := formatSSE("message", env.ID, data)

	s.mu.RLock()
	clients := make([]*sseClient, 0, len(s.clients))
	for _, c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.RUnlock()

	if logging.TraceEnabled(s.cfg.logger) {
		s.cfg.logger.Log(ctx, logging.LevelTrace, "sse: broadcasting",
			"sender_id", s.cfg.id,
			"envelope_id", env.ID,
			"client_count", len(clients),
		)
	}

	for _, c := range clients {
		select {
		case <-ctx.Done():
			s.cfg.metrics.Timer(domain.MetricSSEBroadcastLatency, time.Since(start))
			return ctx.Err()
		case c.events <- eventBytes:
		default:
			if s.cfg.logger != nil {
				s.cfg.logger.Warn("sse: client buffer full, dropping event",
					"client", c.id, "event_id", env.ID)
			}
		}
	}

	s.cfg.metrics.Timer(domain.MetricSSEBroadcastLatency, time.Since(start))
	return nil
}

// ServeHTTP handles SSE client connections.
func (s *SSESender) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	if s.cfg.apiKey != "" && !checkAPIKey(r, s.cfg.apiKey) {
		writeError(w, http.StatusUnauthorized, "invalid or missing API key")
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
				logging.Debug(s.cfg.logger, "sse: redirecting to peer",
					"route_id", rid, "peer", node.InstanceID)
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
	s.cfg.metrics.Gauge(domain.MetricSSEClients, float64(count))
	logging.Debug(s.cfg.logger, "sse: client connected",
		"client_id", clientID, "total_clients", count)

	defer func() {
		s.mu.Lock()
		delete(s.clients, clientID)
		count := len(s.clients)
		s.mu.Unlock()
		s.cfg.metrics.Gauge(domain.MetricSSEClients, float64(count))
		logging.Debug(s.cfg.logger, "sse: client disconnected",
			"client_id", clientID, "total_clients", count)
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	heartbeat := time.NewTicker(s.cfg.heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case event := <-client.events:
			if _, err := w.Write(event); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprintf(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

type sseEvent struct {
	ID      string         `json:"id"`
	Subject string         `json:"subject"`
	Payload json.RawMessage `json:"payload"`
	Headers map[string]any `json:"headers,omitempty"`
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
