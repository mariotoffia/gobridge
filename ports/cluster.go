package ports

import (
	"context"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// RouteLocator determines which cluster node should handle a given route.
// The runtime implements this interface; the HTTP transport adapter consumes it.
type RouteLocator interface {
	// Locate returns the peer that owns the given route.
	// When local is true, this instance should handle the route and peer is nil.
	// When local is false, peer contains the owning instance's endpoints.
	Locate(ctx context.Context, routeID string) (peer *domain.PeerInfo, local bool, err error)
}

// MessageForwarder sends a message to another gobridge instance for processing.
// The HTTP transport adapter implements this using HTTP POST.
// The receiverID identifies the remote receiver's mounted path, which may differ
// from the route ID when a route references a receiver with a different name.
type MessageForwarder interface {
	Forward(ctx context.Context, peer *domain.PeerInfo, receiverID string, env *messaging.Envelope) error
}

// EndpointResolver discovers this instance's externally-reachable address.
// Called once at startup. The listenAddr is the local HTTP listen address
// (e.g. ":9090") from the HTTP server configuration. Implementations probe
// the runtime environment to determine the externally-reachable DNS name or IP.
type EndpointResolver interface {
	Resolve(ctx context.Context, listenAddr string) (map[string]string, error)
}
