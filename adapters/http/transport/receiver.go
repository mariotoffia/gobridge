package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Receiver = (*Receiver)(nil)

const (
	// headerForwarded marks a request as already cluster-forwarded by a
	// peer bridge. It is advisory only: any client can set it, so it is
	// trusted ONLY together with a valid headerForwardToken — see
	// (*Receiver).forwardTrusted.
	headerForwarded = "X-Bridge-Forwarded"
	// headerForwardToken carries the shared internal forwarding secret
	// proving a request originates from a trusted peer bridge rather than
	// an external client. Without it a spoofed headerForwarded marker
	// cannot force local processing on a non-owner node.
	headerForwardToken = "X-Bridge-Forward-Token"

	// External-producer key headers. They let an untrusted HTTP producer
	// supply idempotency / dedup / ordering keys on the trusted side of
	// the ingress reserved-header strip (see externalKeysFromRequest).
	// The cluster forwarder re-emits these so the first-class keys
	// survive a bridge-to-bridge hop (see forwarder.go).
	headerIdempotencyKey  = "Idempotency-Key"
	headerDeduplicationID = "X-Dedup-Id"
	headerOrderingKey     = "X-Ordering-Key"
)

type receiverConfig struct {
	id           string
	path         string
	maxBodySize  int64
	apiKey       string
	forwardToken string
	locator      ports.RouteLocator
	forwarder    ports.MessageForwarder
	metrics      ports.MetricsExporter
	logger       *slog.Logger
	clock        clock.Clock
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
	if cfg.clock == nil {
		cfg.clock = clock.System
	}
	return &Receiver{
		cfg:   cfg,
		ready: make(chan struct{}),
	}
}

// Started returns a channel that is closed once Run has stored the
// emit callback and the receiver is ready to accept HTTP requests.
// It satisfies ports.ReceiverStartedSignaler.
func (r *Receiver) Started() <-chan struct{} { return r.ready }

// SetRouteID associates this receiver with a route for cluster-aware routing.
func (r *Receiver) SetRouteID(routeID string) {
	r.mu.Lock()
	r.routeID = routeID
	r.mu.Unlock()
}

// Run stores the emit callback and blocks until ctx is cancelled.
// Safe to call multiple times (idempotent ready signal).
func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	if logging.DebugEnabled(r.cfg.logger) {
		r.cfg.logger.Log(ctx, logging.LevelDebug, "http: receiver ready",
			"receiver_id", r.cfg.id,
			"path", r.cfg.path,
		)
	}

	r.mu.Lock()
	r.emit = emit
	r.readyOnce.Do(func() { close(r.ready) })
	r.mu.Unlock()

	<-ctx.Done()
	return ctx.Err()
}

// ServeHTTP handles incoming HTTP POST requests and converts them to deliveries.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := r.cfg.clock.Now()
	ctx := req.Context()

	select {
	case <-r.ready:
	case <-ctx.Done():
		writeError(w, http.StatusServiceUnavailable, "receiver not ready")
		return
	}

	if r.cfg.apiKey != "" && !checkAPIKey(req, r.cfg.apiKey) {
		writeUnauthorized(w, "invalid or missing API key")
		return
	}

	// Loop-prevention / "already forwarded" state is trusted ONLY from an
	// authenticated peer. A bare client-controlled X-Bridge-Forwarded
	// header can no longer force local processing on a non-owner node.
	forwardMarker := req.Header.Get(headerForwarded) == "true"
	forwarded := forwardMarker && r.forwardTrusted(req)
	if forwardMarker && !forwarded && logging.DebugEnabled(r.cfg.logger) {
		r.cfg.logger.Log(ctx, logging.LevelDebug,
			"http: ignoring untrusted X-Bridge-Forwarded marker", "path", req.URL.Path)
	}

	if logging.TraceEnabled(r.cfg.logger) {
		r.cfg.logger.Log(ctx, logging.LevelTrace, "http: ingress request",
			"path", req.URL.Path,
			"content_length", req.ContentLength,
			"forwarded", forwarded,
		)
	}

	req.Body = http.MaxBytesReader(w, req.Body, r.cfg.maxBodySize)

	ct := req.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}

	dec := json.NewDecoder(req.Body)
	var body ingressRequest
	if err := dec.Decode(&body); err != nil {
		if isMaxBytes(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds maximum size")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Reject any content after the single JSON value: a well-formed body
	// yields io.EOF on the next token. A second JSON value, trailing
	// garbage, or an oversize trailer is rejected rather than silently
	// ignored (413 when it tripped the body cap, 400 otherwise).
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if isMaxBytes(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds maximum size")
			return
		}
		writeError(w, http.StatusBadRequest, "unexpected trailing data after JSON body")
		return
	}

	if body.Subject == "" {
		writeError(w, http.StatusBadRequest, "subject is required")
		return
	}

	env, err := body.toEnvelope(r.cfg.clock, externalKeysFromRequest(req))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

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
			// Loop guard: a request that already carries an
			// X-Bridge-Forwarded marker was forwarded to us by a peer.
			// Trusted markers never reach here — they skip locating and are
			// processed locally to terminate the chain. An *untrusted* marker
			// on a route we do not own means a peer forwarded under a routing
			// disagreement (split-brain, membership churn, stale locator);
			// re-forwarding could bounce A->B->A until the client timeout
			// fires. Forwarding is single-hop by contract, so refuse rather
			// than re-forward. This stays loop-safe even when no forward
			// token is configured (the default), and remains spoof-safe: an
			// untrusted marker still cannot force local processing on a
			// non-owner — we 508, we do not handle it.
			if forwardMarker {
				r.cfg.metrics.Counter(MetricHTTPForwardLoopRefused, 1)
				if r.cfg.logger != nil {
					r.cfg.logger.Error("refusing to re-forward an already-forwarded request",
						"route", routeID, "peer", node.InstanceID)
				}
				writeError(w, http.StatusLoopDetected, "already forwarded; this node does not own the route")
				return
			}
			if r.cfg.forwarder == nil {
				if r.cfg.logger != nil {
					r.cfg.logger.Error("remote route but no forwarder configured", "route", routeID, "peer", node.InstanceID)
				}
				writeError(w, http.StatusBadGateway, "no forwarder configured for remote route")
				return
			}
			r.cfg.metrics.Counter(MetricClusterForwards, 1)
			fwdStart := r.cfg.clock.Now()
			if err := r.cfg.forwarder.Forward(ctx, node, r.cfg.id, env); err != nil {
				r.cfg.metrics.Timer(MetricHTTPForwardLatency, r.cfg.clock.Since(fwdStart))
				if r.cfg.logger != nil {
					r.cfg.logger.Error("forward failed", "route", routeID, "peer", node.InstanceID, "error", err)
				}
				writeError(w, http.StatusBadGateway, "forward failed")
				return
			}
			r.cfg.metrics.Timer(MetricHTTPForwardLatency, r.cfg.clock.Since(fwdStart))
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
		r.cfg.metrics.Timer(MetricHTTPIngressLatency, r.cfg.clock.Since(start))
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

// forwardTrusted reports whether the request may be treated as an
// already-forwarded, peer-originated message. A client-controlled
// X-Bridge-Forwarded header is NOT sufficient: the request must also
// present the configured internal forwarding token, constant-time
// compared. When no forward token is configured the receiver refuses to
// trust forwarded state at all, so a spoofed marker can never force
// local processing on a non-owner node. The same token must be wired
// into the peer HTTPForwarder by the composition root — see doc.go for
// the deferred peer-authentication contract.
func (r *Receiver) forwardTrusted(req *http.Request) bool {
	if r.cfg.forwardToken == "" {
		return false
	}
	return constantTimeSecretMatch(req.Header.Get(headerForwardToken), r.cfg.forwardToken)
}

// isMaxBytes reports whether err is (or wraps) a body-size-cap breach
// from http.MaxBytesReader, which the handler maps to 413.
func isMaxBytes(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// externalKeys carries the standard, NON-reserved request headers that
// let an external HTTP producer supply idempotency / dedup / ordering
// keys without touching the anti-spoofed x-bridge.* namespace. They are
// routed through EnvelopeInput's first-class fields so NewEnvelope
// stamps the reserved headers on the trusted side of the ingress strip.
type externalKeys struct {
	idempotencyKey  string
	deduplicationID string
	orderingKey     string
}

// externalKeysFromRequest reads the supported external-producer key
// headers. Idempotency-Key is the IETF-draft standard header; the dedup
// and ordering keys use the bridge's X- convention as no standard HTTP
// header exists for them.
func externalKeysFromRequest(req *http.Request) externalKeys {
	return externalKeys{
		idempotencyKey:  req.Header.Get(headerIdempotencyKey),
		deduplicationID: req.Header.Get(headerDeduplicationID),
		orderingKey:     req.Header.Get(headerOrderingKey),
	}
}

func (r *ingressRequest) toEnvelope(clk clock.Clock, keys externalKeys) (*messaging.Envelope, error) {
	if clk == nil {
		clk = clock.System
	}
	id := r.ID
	if id == "" {
		id = generateHTTPEnvelopeID(clk)
	}
	var expires time.Time
	if r.ExpiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, r.ExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("invalid expires_at: must be RFC3339 format")
		}
		expires = parsed
	}
	env, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID:              id,
		Subject:         r.Subject,
		Payload:         []byte(r.Payload),
		Headers:         r.Headers,
		CreatedAt:       clk.Now(),
		ExpiresAt:       expires,
		IdempotencyKey:  keys.idempotencyKey,
		DeduplicationID: keys.deduplicationID,
		OrderingKey:     keys.orderingKey,
	}, clk.Now())
	if err != nil {
		return nil, fmt.Errorf("envelope: %w", err)
	}
	return env, nil
}

var httpIDCounter atomic.Uint64

func generateHTTPEnvelopeID(clk clock.Clock) string {
	if clk == nil {
		clk = clock.System
	}
	n := httpIDCounter.Add(1)
	return fmt.Sprintf("http-%d-%d", clk.Now().UnixNano(), n)
}
