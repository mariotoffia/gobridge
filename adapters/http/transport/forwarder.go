package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

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

// Forward sends an envelope to a remote instance for the given route.
func (f *HTTPForwarder) Forward(
	ctx context.Context, target *domain.PeerInfo, routeID string, env *domain.Envelope,
) error {
	httpEndpoint, ok := target.Endpoints["http"]
	if !ok {
		return domain.ErrForwardFailed.WithMessage("target has no HTTP endpoint")
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

	url := httpEndpoint + f.pathPrefix + "/receivers/" + routeID + "/messages"

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return domain.ErrForwardFailed.Wrap(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Bridge-Forwarded", "true")
	if f.clusterKey != "" {
		req.Header.Set("X-API-Key", f.clusterKey)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return domain.ErrForwardFailed.Wrap(err)
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) //nolint:errcheck
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 400 {
		return domain.ErrForwardFailed.WithMessage(
			fmt.Sprintf("remote returned %d", resp.StatusCode),
		)
	}
	return nil
}
