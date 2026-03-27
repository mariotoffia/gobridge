package transport

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

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
	cfg     receiverConfig
	mu      sync.Mutex
	emit    func(context.Context, ports.Delivery) error
	ready   chan struct{}
	routeID string
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
	r.routeID = routeID
}

// Run stores the emit callback and blocks until ctx is cancelled.
func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	r.mu.Lock()
	r.emit = emit
	close(r.ready)
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

	req.Body = http.MaxBytesReader(w, req.Body, r.cfg.maxBodySize)
	var body ingressRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	env := body.toEnvelope()

	if !forwarded && r.routeID != "" && r.cfg.locator != nil {
		node, local, err := r.cfg.locator.Locate(ctx, r.routeID)
		if err != nil {
			writeError(w, http.StatusBadGateway, "route location failed: "+err.Error())
			return
		}
		if !local && node != nil && r.cfg.forwarder != nil {
			r.cfg.metrics.Counter(domain.MetricClusterForwards, 1)
			fwdStart := time.Now()
			if err := r.cfg.forwarder.Forward(ctx, node, r.routeID, env); err != nil {
				r.cfg.metrics.Timer(domain.MetricHTTPForwardLatency, time.Since(fwdStart))
				writeError(w, http.StatusBadGateway, "forward failed: "+err.Error())
				return
			}
			r.cfg.metrics.Timer(domain.MetricHTTPForwardLatency, time.Since(fwdStart))
			writeJSON(w, http.StatusOK, map[string]string{
				"status":       "accepted",
				"forwarded_to": node.InstanceID,
			})
			return
		}
	}

	del := newHTTPDelivery(env)
	if err := r.emit(ctx, del); err != nil {
		writeError(w, http.StatusInternalServerError, "emit failed: "+err.Error())
		return
	}

	select {
	case result := <-del.done:
		r.cfg.metrics.Timer(domain.MetricHTTPIngressLatency, time.Since(start))
		if result.err != nil {
			writeError(w, http.StatusInternalServerError, result.err.Error())
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

func (r *ingressRequest) toEnvelope() *domain.Envelope {
	env := &domain.Envelope{
		ID:        r.ID,
		Subject:   r.Subject,
		Payload:   []byte(r.Payload),
		Headers:   r.Headers,
		CreatedAt: time.Now(),
	}
	if r.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, r.ExpiresAt); err == nil {
			env.ExpiresAt = t
		}
	}
	return env
}
