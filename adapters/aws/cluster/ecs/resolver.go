package ecs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// EcsEndpointResolver discovers this instance's reachable address by querying
// the ECS task metadata v4 endpoint. It prefers PrivateDNSName over IPv4Addresses.
//
// Requires the ECS_CONTAINER_METADATA_URI_V4 environment variable to be set
// (automatically provided by the ECS agent for Fargate and EC2 launch types).
type EcsEndpointResolver struct {
	client *http.Client
}

// Option configures an EcsEndpointResolver.
type Option func(*EcsEndpointResolver)

// WithHTTPClient overrides the default HTTP client used to query the metadata endpoint.
func WithHTTPClient(c *http.Client) Option {
	return func(r *EcsEndpointResolver) { r.client = c }
}

// NewEcsEndpointResolver creates a resolver that queries the ECS task metadata
// v4 endpoint to discover the container's network address.
func NewEcsEndpointResolver(opts ...Option) *EcsEndpointResolver {
	r := &EcsEndpointResolver{
		client: &http.Client{Timeout: 5 * time.Second},
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Resolve returns the externally-reachable HTTP endpoint for this ECS task.
func (r *EcsEndpointResolver) Resolve(ctx context.Context, listenAddr string) (map[string]string, error) {
	metadataURI := os.Getenv("ECS_CONTAINER_METADATA_URI_V4")
	if metadataURI == "" {
		return nil, fmt.Errorf("ecs: ECS_CONTAINER_METADATA_URI_V4 not set")
	}

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

	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		port = listenAddr
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
