package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"
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
	cfg        ForwarderConfig
	client     *http.Client
	pathPrefix string
	clusterKey string
	logger     *slog.Logger
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
		pathPrefix: pathPrefix,
	}
	if len(clusterKey) > 0 {
		f.clusterKey = clusterKey[0]
	}
	return f
}

// SetLogger configures the forwarder's logger for trace/debug output.
func (f *HTTPForwarder) SetLogger(l *slog.Logger) { f.logger = l }

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

// Forward sends an envelope to a remote instance's receiver endpoint.
func (f *HTTPForwarder) Forward(
	ctx context.Context, target *domain.PeerInfo, receiverID string, env *domain.Envelope,
) error {
	httpEndpoint, ok := target.Endpoints["http"]
	if !ok {
		return shared.ErrForwardFailed.WithMessage("target has no HTTP endpoint")
	}

	if logging.TraceEnabled(f.logger) {
		f.logger.Log(ctx, logging.LevelTrace, "http: forwarding",
			"target_instance", target.InstanceID,
			"receiver_id", receiverID,
			"endpoint", httpEndpoint,
		)
	}

	ir := ingressRequest{
		ID:      env.ID,
		Subject: env.Subject,
		Payload: env.Payload,
		Headers: env.Headers,
	}
	if !env.ExpiresAt.IsZero() {
		ir.ExpiresAt = env.ExpiresAt.Format(time.RFC3339)
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
	req.Header.Set("X-Bridge-Forwarded", "true")
	if f.clusterKey != "" {
		req.Header.Set("X-API-Key", f.clusterKey)
	}

	var lastErr error
	for attempt := 0; attempt <= f.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := f.retryDelay(attempt)
			select {
			case <-ctx.Done():
				return shared.ErrForwardFailed.Wrap(ctx.Err())
			case <-f.cfg.Clock.After(delay):
			}
			req = req.Clone(ctx)
			req.Body = io.NopCloser(bytes.NewReader(body))
		}

		var resp *http.Response
		resp, lastErr = f.client.Do(req)
		if lastErr != nil {
			continue
		}

		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()

		if resp.StatusCode >= 500 {
			if logging.DebugEnabled(f.logger) {
				f.logger.Log(ctx, logging.LevelDebug, "http: forward failed (server error)",
					"target_instance", target.InstanceID,
					"receiver_id", receiverID,
					"status_code", resp.StatusCode,
					"attempt", attempt+1,
				)
			}
			lastErr = shared.ErrUnavailable.WithMessage(
				fmt.Sprintf("remote returned %d", resp.StatusCode),
			)
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

	return shared.ErrForwardFailed.Wrap(lastErr)
}
