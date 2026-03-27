package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

// HTTPForwarder implements ports.MessageForwarder using HTTP POST
// to forward messages to other gobridge instances in the cluster.
type HTTPForwarder struct {
	client     *http.Client
	pathPrefix string
}

// NewHTTPForwarder creates a forwarder that sends messages to remote instances.
func NewHTTPForwarder(pathPrefix string, timeout time.Duration) *HTTPForwarder {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &HTTPForwarder{
		client:     &http.Client{Timeout: timeout},
		pathPrefix: pathPrefix,
	}
}

// Forward sends an envelope to a remote instance for the given route.
func (f *HTTPForwarder) Forward(
	ctx context.Context, target *domain.PeerInfo, routeID string, env *domain.Envelope,
) error {
	httpEndpoint, ok := target.Endpoints["http"]
	if !ok {
		return domain.ErrForwardFailed.WithMessage("target has no HTTP endpoint")
	}

	body, err := json.Marshal(ingressRequest{
		ID:      env.ID,
		Subject: env.Subject,
		Payload: env.Payload,
		Headers: env.Headers,
	})
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

	resp, err := f.client.Do(req)
	if err != nil {
		return domain.ErrForwardFailed.Wrap(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return domain.ErrForwardFailed.WithMessage(
			fmt.Sprintf("remote returned %d", resp.StatusCode),
		)
	}
	return nil
}
