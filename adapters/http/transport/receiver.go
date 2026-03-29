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

var _ ports.Receiver = (*Receiver)(nil)

type receiverConfig struct {
	id          string
	path        string
	maxBodySize int64
	apiKey      string
	locator     ports.RouteLocator
	forwarder   ports.MessageForwarder
	metrics     ports.MetricsExporter
	logger      *slog.Logger
}

// Receiver implements ports.Receiver for HTTP ingress.
type Receiver struct {
	cfg       receiverConfig
	mu        sync.Mutex
	emit      func(context.Context, ports.Delivery) error
	ready     chan struct{}
	readyOnce sync.Once
	routeID   string
}

func newReceiver(cfg receiverConfig) *Receiver {
	if cfg.maxBodySize == 0 {
		cfg.maxBodySize = 1 << 20 // 1 MiB
	}
	if cfg.metrics == nil {
		cfg.metrics = &ports.NoopExporter{}
	}
	return &Receiver{
		cfg:   cfg,
		ready: make(chan struct{}),
	}
}

// SetRouteID associates this receiver with a route for cluster-aware routing.
func (r *Receiver) SetRouteID(routeID string) {
	r.mu.Lock()
	r.routeID = routeID
	r.mu.Unlock()
}

// Run stores the emit callback and blocks until ctx is cancelled.
// Safe to call multiple times (idempotent ready signal).
func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	logging.DebugContext(r.cfg.logger, ctx, "http: receiver ready",
		"receiver_id", r.cfg.id,
		"path", r.cfg.path,
	)

	r.mu.Lock()
	r.emit = emit
	r.readyOnce.Do(func() { close(r.ready) })
	r.mu.Unlock()

	<-ctx.Done()
	return ctx.Err()
}

// ServeHTTP handles incoming HTTP POST requests and converts them to deliveries.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	ctx := req.Context()

	select {
	case <-r.ready:
	case <-ctx.Done():
		writeError(w, http.StatusServiceUnavailable, "receiver not ready")
		return
	}

	if r.cfg.apiKey != "" && !checkAPIKey(req, r.cfg.apiKey) {
		writeError(w, http.StatusUnauthorized, "invalid or missing API key")
		return
	}

	forwarded := req.Header.Get("X-Bridge-Forwarded") == "true"

	if logging.TraceEnabled(r.cfg.logger) {
		r.cfg.logger.Log(ctx, logging.LevelTrace, "http: ingress request",
			"path", req.URL.Path,
			"content_length", req.ContentLength,
			"forwarded", forwarded,
		)
	}

	req.Body = http.MaxBytesReader(w, req.Body, r.cfg.maxBodySize)
	var body ingressRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Subject == "" {
		writeError(w, http.StatusBadRequest, "subject is required")
		return
	}

	env, err := body.toEnvelope()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	env.Headers = domain.StripReservedHeaders(env.Headers)

	r.mu.Lock()
	routeID := r.routeID
	emit := r.emit
	r.mu.Unlock()

	if !forwarded && routeID != "" && r.cfg.locator != nil {
		node, local, err := r.cfg.locator.Locate(ctx, routeID)
		if err != nil {
			if r.cfg.logger != nil {
				r.cfg.logger.Error("route location failed", "route", routeID, "error", err)
			}
			writeError(w, http.StatusBadGateway, "route location failed")
			return
		}
		if !local && node != nil {
			if r.cfg.forwarder == nil {
				if r.cfg.logger != nil {
					r.cfg.logger.Error("remote route but no forwarder configured", "route", routeID, "peer", node.InstanceID)
				}
				writeError(w, http.StatusBadGateway, "no forwarder configured for remote route")
				return
			}
			r.cfg.metrics.Counter(domain.MetricClusterForwards, 1)
			fwdStart := time.Now()
			if err := r.cfg.forwarder.Forward(ctx, node, routeID, env); err != nil {
				r.cfg.metrics.Timer(domain.MetricHTTPForwardLatency, time.Since(fwdStart))
				if r.cfg.logger != nil {
					r.cfg.logger.Error("forward failed", "route", routeID, "peer", node.InstanceID, "error", err)
				}
				writeError(w, http.StatusBadGateway, "forward failed")
				return
			}
			r.cfg.metrics.Timer(domain.MetricHTTPForwardLatency, time.Since(fwdStart))
			writeJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
			return
		}
	}

	del := newHTTPDelivery(env)
	if err := emit(ctx, del); err != nil {
		if r.cfg.logger != nil {
			r.cfg.logger.Error("emit failed", "error", err)
		}
		writeError(w, http.StatusInternalServerError, "processing failed")
		return
	}

	select {
	case result := <-del.done:
		r.cfg.metrics.Timer(domain.MetricHTTPIngressLatency, time.Since(start))
		if result.err != nil {
			writeError(w, http.StatusInternalServerError, "processing failed")
		} else {
			writeJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
		}
	case <-ctx.Done():
		writeError(w, http.StatusGatewayTimeout, "request timeout")
	}
}

type ingressRequest struct {
	Subject   string          `json:"subject"`
	Payload   json.RawMessage `json:"payload"`
	Headers   map[string]any  `json:"headers,omitempty"`
	ID        string          `json:"id,omitempty"`
	ExpiresAt string          `json:"expires_at,omitempty"`
}

func (r *ingressRequest) toEnvelope() (*domain.Envelope, error) {
	env := &domain.Envelope{
		ID:        r.ID,
		Subject:   r.Subject,
		Payload:   []byte(r.Payload),
		Headers:   r.Headers,
		CreatedAt: time.Now(),
	}
	if r.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, r.ExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("invalid expires_at: must be RFC3339 format")
		}
		env.ExpiresAt = t
	}
	return env, nil
}
