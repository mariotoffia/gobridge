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

	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.MessageForwarder = (*HTTPForwarder)(nil)

// HTTPForwarder implements ports.MessageForwarder using HTTP POST
// to forward messages to other gobridge instances in the cluster.
type HTTPForwarder struct {
	client     *http.Client
	pathPrefix string
	clusterKey string
	logger     *slog.Logger
}

// NewHTTPForwarder creates a forwarder that sends messages to remote instances.
// The optional clusterKey is sent as X-API-Key on forwarded requests to
// authenticate with the receiving peer's API key check.
func NewHTTPForwarder(pathPrefix string, timeout time.Duration, clusterKey ...string) *HTTPForwarder {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	f := &HTTPForwarder{
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 32,
				MaxConnsPerHost:     64,
				IdleConnTimeout:     90 * time.Second,
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

// Forward sends an envelope to a remote instance's receiver endpoint.
func (f *HTTPForwarder) Forward(
	ctx context.Context, target *domain.PeerInfo, receiverID string, env *domain.Envelope,
) error {
	httpEndpoint, ok := target.Endpoints["http"]
	if !ok {
		return domain.ErrForwardFailed.WithMessage("target has no HTTP endpoint")
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
		return domain.ErrForwardFailed.Wrap(err)
	}

	url := httpEndpoint + f.pathPrefix + "/receivers/" + receiverID + "/messages"

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return domain.ErrForwardFailed.Wrap(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Bridge-Forwarded", "true")
	if f.clusterKey != "" {
		req.Header.Set("X-API-Key", f.clusterKey)
	}

	const maxRetries = 2
	retryDelays := [...]time.Duration{100 * time.Millisecond, 200 * time.Millisecond}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return domain.ErrForwardFailed.Wrap(ctx.Err())
			case <-time.After(retryDelays[attempt-1]):
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
			lastErr = domain.ErrUnavailable.WithMessage(
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
			return domain.NewBridgeError(
				domain.ErrCodeForwardFailed, domain.ErrorPermanent,
				fmt.Sprintf("remote returned %d", resp.StatusCode),
			)
		}
		return nil
	}

	return domain.ErrForwardFailed.Wrap(lastErr)
}
