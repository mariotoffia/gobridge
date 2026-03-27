package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

type sseSenderConfig struct {
	id                string
	path              string
	heartbeatInterval time.Duration
	locator           ports.RouteLocator
	metrics           ports.MetricsExporter
	logger            *slog.Logger
}

type sseClient struct {
	id     string
	events chan []byte
	done   chan struct{}
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
	s.routeID = routeID
}

// Send broadcasts an envelope to all connected SSE clients.
func (s *SSESender) Send(_ context.Context, env *domain.Envelope) error {
	start := time.Now()

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

	for _, c := range clients {
		select {
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

	if s.routeID != "" && s.cfg.locator != nil {
		node, local, err := s.cfg.locator.Locate(r.Context(), s.routeID)
		if err == nil && !local && node != nil {
			httpEndpoint, ok := node.Endpoints["http"]
			if ok {
				http.Redirect(w, r, httpEndpoint+s.cfg.path, http.StatusTemporaryRedirect)
				return
			}
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	clientID := generateClientID()
	client := &sseClient{
		id:     clientID,
		events: make(chan []byte, 256),
		done:   make(chan struct{}),
	}

	s.mu.Lock()
	s.clients[clientID] = client
	count := len(s.clients)
	s.mu.Unlock()
	s.cfg.metrics.Gauge(domain.MetricSSEClients, float64(count))

	defer func() {
		s.mu.Lock()
		delete(s.clients, clientID)
		count := len(s.clients)
		s.mu.Unlock()
		s.cfg.metrics.Gauge(domain.MetricSSEClients, float64(count))
		close(client.done)
	}()

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
	var buf []byte
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
