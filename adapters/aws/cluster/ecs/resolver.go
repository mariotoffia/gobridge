package ecs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
)

const (
	// defaultMaxAttempts is the number of times Resolve queries the ECS task
	// metadata endpoint before giving up. ECS task metadata is frequently not
	// populated in the first moments of a task's life (the agent wires up
	// networking asynchronously), so a single GET races task startup and
	// silently disables cluster forwarding for the whole process lifetime.
	// A small bounded retry with backoff closes that race without
	// blocking startup indefinitely.
	defaultMaxAttempts = 5

	// defaultRetryBackoff is the initial delay between metadata attempts. It
	// grows exponentially up to defaultMaxBackoff.
	defaultRetryBackoff = 500 * time.Millisecond

	// defaultMaxBackoff caps the per-attempt backoff so total resolution time
	// stays bounded (5 attempts: ~0.5+1+2+4 = ~7.5s worst case).
	defaultMaxBackoff = 4 * time.Second
)

// EcsEndpointResolver discovers this instance's reachable address by querying
// the ECS task metadata v4 endpoint. It prefers PrivateDNSName over IPv4Addresses.
//
// Requires the ECS_CONTAINER_METADATA_URI_V4 environment variable to be set
// (automatically provided by the ECS agent for Fargate and EC2 launch types).
//
// Because ECS task metadata is often not fully populated in the first seconds
// of a task's life, Resolve retries transient failures a bounded number of
// times with exponential backoff before returning an error.
type EcsEndpointResolver struct {
	client       *http.Client
	maxAttempts  int
	retryBackoff time.Duration
	maxBackoff   time.Duration
	clk          clock.Clock
	logger       *slog.Logger
}

// Option configures an EcsEndpointResolver.
type Option func(*EcsEndpointResolver)

// WithHTTPClient overrides the default HTTP client used to query the metadata endpoint.
func WithHTTPClient(c *http.Client) Option {
	return func(r *EcsEndpointResolver) { r.client = c }
}

// WithHTTPTimeout sets the timeout on the default HTTP client used
// to query the ECS metadata endpoint. Default 5s. Ignored when
// WithHTTPClient is also used (last option wins).
func WithHTTPTimeout(d time.Duration) Option {
	return func(r *EcsEndpointResolver) { r.client.Timeout = d }
}

// WithMaxAttempts overrides the number of metadata query attempts before
// Resolve gives up. Values < 1 are ignored.
func WithMaxAttempts(n int) Option {
	return func(r *EcsEndpointResolver) {
		if n >= 1 {
			r.maxAttempts = n
		}
	}
}

// WithRetryBackoff overrides the initial backoff between metadata attempts.
// Values <= 0 are ignored. The backoff grows exponentially, capped at the
// max backoff.
func WithRetryBackoff(d time.Duration) Option {
	return func(r *EcsEndpointResolver) {
		if d > 0 {
			r.retryBackoff = d
		}
	}
}

// WithMaxBackoff overrides the ceiling on the per-attempt backoff. Values <= 0
// are ignored.
func WithMaxBackoff(d time.Duration) Option {
	return func(r *EcsEndpointResolver) {
		if d > 0 {
			r.maxBackoff = d
		}
	}
}

// WithClock overrides the clock used to sleep between retries. Defaults to
// clock.System. Injecting a fake clock makes the backoff deterministic in tests.
func WithClock(c clock.Clock) Option {
	return func(r *EcsEndpointResolver) {
		if c != nil {
			r.clk = c
		}
	}
}

// WithLogger sets the structured logger used to warn about transient metadata
// resolution failures that are being retried.
func WithLogger(l *slog.Logger) Option {
	return func(r *EcsEndpointResolver) { r.logger = l }
}

// NewEcsEndpointResolver creates a resolver that queries the ECS task metadata
// v4 endpoint to discover the container's network address.
func NewEcsEndpointResolver(opts ...Option) *EcsEndpointResolver {
	r := &EcsEndpointResolver{
		client:       &http.Client{Timeout: 5 * time.Second},
		maxAttempts:  defaultMaxAttempts,
		retryBackoff: defaultRetryBackoff,
		maxBackoff:   defaultMaxBackoff,
		clk:          clock.System,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// errNoMetadataURI signals that the ECS metadata environment variable is not
// set. This is a permanent (non-retryable) misconfiguration.
var errNoMetadataURI = errors.New("ecs: ECS_CONTAINER_METADATA_URI_V4 not set")

// Resolve returns the externally-reachable HTTP endpoint for this ECS task.
//
// Transient failures (network errors, non-200 responses, or metadata that is
// not yet fully populated) are retried a bounded number of times with
// exponential backoff. A missing metadata environment variable is a permanent
// misconfiguration and fails immediately without retrying.
func (r *EcsEndpointResolver) Resolve(ctx context.Context, listenAddr string) (map[string]string, error) {
	metadataURI := os.Getenv("ECS_CONTAINER_METADATA_URI_V4")
	if metadataURI == "" {
		return nil, errNoMetadataURI
	}

	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		port = listenAddr
	}

	backoff := r.retryBackoff
	var lastErr error
	for attempt := 1; attempt <= r.maxAttempts; attempt++ {
		endpoints, ferr := r.fetchOnce(ctx, metadataURI, port)
		if ferr == nil {
			return endpoints, nil
		}
		lastErr = ferr

		// Never retry once the caller's context is done.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("ecs: metadata resolution cancelled after %d attempt(s): %w", attempt, ctx.Err())
		}
		if attempt == r.maxAttempts {
			break
		}

		if r.logger != nil {
			r.logger.WarnContext(ctx, "ecs: metadata resolution failed, retrying",
				"attempt", attempt,
				"max_attempts", r.maxAttempts,
				"backoff", backoff.String(),
				"error", ferr.Error(),
			)
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("ecs: metadata resolution cancelled after %d attempt(s): %w", attempt, ctx.Err())
		case <-r.clk.After(backoff):
		}

		if backoff *= 2; backoff > r.maxBackoff {
			backoff = r.maxBackoff
		}
	}

	return nil, fmt.Errorf("ecs: metadata resolution failed after %d attempt(s): %w", r.maxAttempts, lastErr)
}

// fetchOnce performs a single metadata query and address extraction. Every
// error it returns is treated as transient/retryable by Resolve.
func (r *EcsEndpointResolver) fetchOnce(ctx context.Context, metadataURI, port string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", metadataURI, nil)
	if err != nil {
		return nil, fmt.Errorf("ecs: create metadata request: %w", err)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ecs: query metadata endpoint: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ecs: metadata endpoint returned %d", resp.StatusCode)
	}

	var meta containerMetadata
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&meta); err != nil {
		return nil, fmt.Errorf("ecs: decode metadata: %w", err)
	}

	if len(meta.Networks) == 0 {
		return nil, fmt.Errorf("ecs: no networks in metadata response")
	}

	network := meta.Networks[0]
	host := network.PrivateDNSName
	if host == "" && len(network.IPv4Addresses) > 0 {
		host = network.IPv4Addresses[0]
	}
	if host == "" {
		return nil, fmt.Errorf("ecs: no address found in metadata")
	}

	return map[string]string{
		"http": fmt.Sprintf("http://%s:%s", host, port),
	}, nil
}

type containerMetadata struct {
	Networks []networkInfo `json:"Networks"`
}

type networkInfo struct {
	NetworkMode    string   `json:"NetworkMode"`
	IPv4Addresses  []string `json:"IPv4Addresses"`
	PrivateDNSName string   `json:"PrivateDNSName"`
}
