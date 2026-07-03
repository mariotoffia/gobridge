package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.MessageForwarder = (*HTTPForwarder)(nil)

// ForwarderConfig holds tuning knobs for HTTPForwarder.
type ForwarderConfig struct {
	Timeout             time.Duration
	IdleConnTimeout     time.Duration
	MaxRetries          int
	RetryInitialDelay   time.Duration
	RetryMaxDelay       time.Duration
	MaxIdleConnsPerHost int
	MaxConnsPerHost     int
	Clock               clock.Clock
	// ForwardToken is the shared internal secret sent as
	// X-Bridge-Forward-Token so the receiving peer can distinguish a
	// trusted bridge-to-bridge hop from a spoofed client request. It MUST
	// match the receiver-side forward token (Factory.WithForwardToken).
	// Empty disables the token (the receiver then ignores the forwarded
	// marker entirely — see (*Receiver).forwardTrusted).
	ForwardToken string
	// ReceiverAPIKeys maps a receiver ID to the API key presented as
	// X-API-Key when forwarding to that receiver on a peer. It exists
	// because receiver API keys are configured PER RECEIVER: a single
	// shared cluster key would 401 (permanent → DLQ) against any
	// receiver protected by a different key. A receiver ID absent from
	// the map falls back to the shared cluster key passed to the
	// constructor.
	ReceiverAPIKeys map[string]string
	// Breaker, when non-nil, gates every forward attempt through the
	// supplied circuit breaker so a dead peer fails fast (breaker-open
	// rejection carrying ErrUnavailable + RetryAfter) instead of costing
	// each inbound request the full retry × timeout budget. The
	// implementation is supplied by the composition root — this adapter
	// consumes only the ports.CircuitBreaker contract (same pattern as
	// the MQTT CircuitBreakerSender).
	Breaker ports.CircuitBreaker
	// Metrics receives forwarder-side emissions (currently
	// MetricHTTPForwardBreakerOpen). Nil falls back to the no-op
	// exporter.
	Metrics ports.MetricsExporter
}

// DefaultForwarderConfig returns production-safe defaults.
func DefaultForwarderConfig() ForwarderConfig {
	return ForwarderConfig{
		Timeout:             30 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxRetries:          2,
		RetryInitialDelay:   100 * time.Millisecond,
		RetryMaxDelay:       200 * time.Millisecond,
		MaxIdleConnsPerHost: 32,
		MaxConnsPerHost:     64,
	}
}

// HTTPForwarder implements ports.MessageForwarder using HTTP POST
// to forward messages to other gobridge instances in the cluster.
type HTTPForwarder struct {
	cfg          ForwarderConfig
	client       *http.Client
	pathPrefix   string
	clusterKey   string
	forwardToken string
	logger       *slog.Logger
}

// NewHTTPForwarder creates a forwarder that sends messages to remote instances.
// The optional clusterKey is sent as X-API-Key on forwarded requests to
// authenticate with the receiving peer's API key check.
func NewHTTPForwarder(pathPrefix string, timeout time.Duration, clusterKey ...string) *HTTPForwarder {
	cfg := DefaultForwarderConfig()
	if timeout > 0 {
		cfg.Timeout = timeout
	}
	return NewHTTPForwarderWithConfig(pathPrefix, cfg, clusterKey...)
}

// NewHTTPForwarderWithConfig creates a forwarder with explicit configuration.
// The optional clusterKey is sent as X-API-Key on forwarded requests.
func NewHTTPForwarderWithConfig(pathPrefix string, cfg ForwarderConfig, clusterKey ...string) *HTTPForwarder {
	defaults := DefaultForwarderConfig()
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaults.Timeout
	}
	if cfg.IdleConnTimeout <= 0 {
		cfg.IdleConnTimeout = defaults.IdleConnTimeout
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = defaults.MaxRetries
	}
	if cfg.RetryInitialDelay <= 0 {
		cfg.RetryInitialDelay = defaults.RetryInitialDelay
	}
	if cfg.RetryMaxDelay <= 0 {
		cfg.RetryMaxDelay = defaults.RetryMaxDelay
	}
	if cfg.MaxIdleConnsPerHost <= 0 {
		cfg.MaxIdleConnsPerHost = defaults.MaxIdleConnsPerHost
	}
	if cfg.MaxConnsPerHost <= 0 {
		cfg.MaxConnsPerHost = defaults.MaxConnsPerHost
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System
	}
	if cfg.Metrics == nil {
		cfg.Metrics = &ports.NoopExporter{}
	}
	f := &HTTPForwarder{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: cfg.MaxIdleConnsPerHost,
				MaxConnsPerHost:     cfg.MaxConnsPerHost,
				IdleConnTimeout:     cfg.IdleConnTimeout,
			},
		},
		pathPrefix:   pathPrefix,
		forwardToken: cfg.ForwardToken,
	}
	if len(clusterKey) > 0 {
		f.clusterKey = clusterKey[0]
	}
	return f
}

// SetLogger configures the forwarder's logger for trace/debug output.
func (f *HTTPForwarder) SetLogger(l *slog.Logger) { f.logger = l }

// setForwardedKeyHeader copies a reserved envelope header onto the
// trusted external-producer HTTP header consumed by the receiver's
// ingress path, when present and non-empty. No-op for absent keys so a
// forwarded request carries only the keys the original envelope held.
func setForwardedKeyHeader(req *http.Request, httpHeader string, env *messaging.Envelope, envHeader string) {
	if v, ok := env.Headers().GetString(envHeader); ok && v != "" {
		req.Header.Set(httpHeader, v)
	}
}

// retryDelay returns a linearly interpolated delay between
// RetryInitialDelay and RetryMaxDelay for the given 1-based attempt.
func (f *HTTPForwarder) retryDelay(attempt int) time.Duration {
	if f.cfg.MaxRetries <= 1 || attempt <= 1 {
		return f.cfg.RetryInitialDelay
	}
	frac := float64(attempt-1) / float64(f.cfg.MaxRetries-1)
	d := f.cfg.RetryInitialDelay + time.Duration(frac*float64(f.cfg.RetryMaxDelay-f.cfg.RetryInitialDelay))
	if d > f.cfg.RetryMaxDelay {
		d = f.cfg.RetryMaxDelay
	}
	return d
}

// maxRetryAfter caps how long a peer-supplied Retry-After header may
// stall a forward retry. The clamp bounds both a hostile/buggy peer
// (Retry-After: 86400) and HTTP-date arithmetic gone wrong; anything
// above the cap is clamped, not rejected, so the throttle hint is still
// honoured in bounded form.
const maxRetryAfter = 30 * time.Second

// parseRetryAfter parses an RFC 9110 Retry-After value — either a
// non-negative integer of seconds or an HTTP-date — and clamps the
// result to [0, maxRetryAfter]. Returns (0, false) for absent or
// malformed values, so callers fall back to their own retry delay.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
		if secs < 0 {
			return 0, false
		}
		return min(time.Duration(secs)*time.Second, maxRetryAfter), true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	d := when.Sub(now)
	if d < 0 {
		d = 0
	}
	return min(d, maxRetryAfter), true
}

// receiverKey returns the X-API-Key value for a given target receiver:
// the per-receiver key when configured, else the shared cluster key.
func (f *HTTPForwarder) receiverKey(receiverID string) string {
	if key, ok := f.cfg.ReceiverAPIKeys[receiverID]; ok && key != "" {
		return key
	}
	return f.clusterKey
}

// Forward sends an envelope to a remote instance's receiver endpoint.
//
// Delivery contract: AT-LEAST-ONCE. A timeout after the peer received
// the body is retried as a fresh POST, so the peer can observe the same
// envelope more than once. Every forward carries an Idempotency-Key
// derived from the preserved envelope ID (the envelope's own key when
// set, else the envelope ID), which the receiving side's bounded
// idempotency window uses to absorb such duplicates — see doc.go.
func (f *HTTPForwarder) Forward(
	ctx context.Context, target *persistence.PeerInfo, receiverID string, env *messaging.Envelope,
) error {
	httpEndpoint, ok := target.Endpoints["http"]
	if !ok {
		return shared.ErrForwardFailed.WithMessage("target has no HTTP endpoint")
	}

	if f.cfg.Breaker != nil {
		if err := f.cfg.Breaker.BeforeRequest(); err != nil {
			f.cfg.Metrics.Counter(MetricHTTPForwardBreakerOpen, 1)
			if logging.DebugEnabled(f.logger) {
				f.logger.Log(ctx, logging.LevelDebug, "http: forward rejected by open circuit breaker",
					"target_instance", target.InstanceID,
					"receiver_id", receiverID,
				)
			}
			return err
		}
	}
	err := f.forward(ctx, target, httpEndpoint, receiverID, env)
	if f.cfg.Breaker != nil {
		f.cfg.Breaker.AfterRequest(err)
	}
	return err
}

func (f *HTTPForwarder) forward(
	ctx context.Context, target *persistence.PeerInfo, httpEndpoint, receiverID string, env *messaging.Envelope,
) error {
	if logging.TraceEnabled(f.logger) {
		f.logger.Log(ctx, logging.LevelTrace, "http: forwarding",
			"target_instance", target.InstanceID,
			"receiver_id", receiverID,
			"endpoint", httpEndpoint,
		)
	}

	ir := ingressRequest{
		ID:      env.ID(),
		Subject: env.Subject(),
		Payload: env.Payload(),
		Headers: env.Headers(),
	}
	if !env.ExpiresAt().IsZero() {
		ir.ExpiresAt = env.ExpiresAt().Format(time.RFC3339)
	}
	body, err := json.Marshal(ir)
	if err != nil {
		return shared.ErrForwardFailed.Wrap(err)
	}

	url := httpEndpoint + f.pathPrefix + "/receivers/" + receiverID + "/messages"

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return shared.ErrForwardFailed.Wrap(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerForwarded, "true")
	if f.forwardToken != "" {
		req.Header.Set(headerForwardToken, f.forwardToken)
	}
	if key := f.receiverKey(receiverID); key != "" {
		req.Header.Set("X-API-Key", key)
	}
	// Re-emit the first-class idempotency / dedup / ordering keys as the
	// trusted external-producer HTTP headers so they survive the
	// receiver's reserved-header strip and are re-stamped on the far
	// side. The remaining bridge-to-bridge headers (tenant, correlation,
	// causation, trace, forwarded-from/hop) require the authenticated
	// envelope-propagation contract that is deferred — see doc.go.
	setForwardedKeyHeader(req, headerIdempotencyKey, env, messaging.HeaderIdempotencyKey)
	setForwardedKeyHeader(req, headerDeduplicationID, env, messaging.HeaderDeduplicationID)
	setForwardedKeyHeader(req, headerOrderingKey, env, messaging.HeaderOrderingKey)
	// Forward retries are at-least-once: a timeout after the peer read
	// the body re-posts the same envelope. When the envelope carries no
	// idempotency key of its own, derive one from the preserved envelope
	// ID so the receiving side's idempotency window can drop the replay.
	if req.Header.Get(headerIdempotencyKey) == "" {
		req.Header.Set(headerIdempotencyKey, env.ID())
	}

	var lastErr error
	var retryAfterHint time.Duration
	for attempt := 0; attempt <= f.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := f.retryDelay(attempt)
			// Honour the peer's (clamped) throttle hint when it exceeds
			// our own backoff.
			if retryAfterHint > delay {
				delay = retryAfterHint
			}
			select {
			case <-ctx.Done():
				return shared.ErrForwardFailed.Wrap(ctx.Err())
			case <-f.cfg.Clock.After(delay):
			}
			req = req.Clone(ctx)
			req.Body = io.NopCloser(bytes.NewReader(body))
		}
		retryAfterHint = 0

		var resp *http.Response
		resp, lastErr = f.client.Do(req)
		if lastErr != nil {
			continue
		}

		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()

		if transientStatus(resp.StatusCode) {
			if logging.DebugEnabled(f.logger) {
				f.logger.Log(ctx, logging.LevelDebug, "http: forward failed (transient)",
					"target_instance", target.InstanceID,
					"receiver_id", receiverID,
					"status_code", resp.StatusCode,
					"attempt", attempt+1,
				)
			}
			if hint, ok := parseRetryAfter(resp.Header.Get("Retry-After"), f.cfg.Clock.Now()); ok {
				retryAfterHint = hint
			}
			lastErr = transientStatusError(resp.StatusCode, retryAfterHint)
			continue
		}
		if resp.StatusCode >= 400 {
			if logging.DebugEnabled(f.logger) {
				f.logger.Log(ctx, logging.LevelDebug, "http: forward failed (client error)",
					"target_instance", target.InstanceID,
					"receiver_id", receiverID,
					"status_code", resp.StatusCode,
				)
			}
			return shared.NewBridgeError(
				shared.ErrCodeForwardFailed, shared.ErrorPermanent,
				fmt.Sprintf("remote returned %d", resp.StatusCode),
			)
		}
		return nil
	}

	wrapped := shared.ErrForwardFailed.Wrap(lastErr)
	if hint := shared.GetRetryAfter(lastErr); hint > 0 {
		// Surface the throttle hint on the outermost error: GetRetryAfter
		// stops at the first BridgeError in the chain.
		wrapped = wrapped.WithRetryAfter(hint)
	}
	return wrapped
}

// transientStatus reports whether an HTTP status must be retried rather
// than dead-lettered: every 5xx, plus 429 Too Many Requests (throttled
// peer — RFC 6585) and 408 Request Timeout. Classifying 429/408 as
// permanent would dead-letter messages a briefly throttled peer would
// have accepted moments later.
func transientStatus(code int) bool {
	return code >= 500 || code == http.StatusTooManyRequests || code == http.StatusRequestTimeout
}

// transientStatusError maps a transient HTTP status onto the domain
// error catalog: 429 → ErrThrottled, 408 → ErrTimeout, 5xx →
// ErrUnavailable. The clamped Retry-After hint (when > 0) rides along so
// upstream retry policies respect the peer's cool-down.
func transientStatusError(code int, retryAfter time.Duration) error {
	msg := fmt.Sprintf("remote returned %d", code)
	var be *shared.BridgeError
	switch code {
	case http.StatusTooManyRequests:
		be = shared.ErrThrottled.WithMessage(msg)
	case http.StatusRequestTimeout:
		be = shared.ErrTimeout.WithMessage(msg)
	default:
		be = shared.ErrUnavailable.WithMessage(msg)
	}
	if retryAfter > 0 {
		be = be.WithRetryAfter(retryAfter)
	}
	return be
}
