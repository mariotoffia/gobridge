package ports

import (
	"context"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

// RouteLocator determines which cluster node should handle a given route.
// The runtime implements this interface; the HTTP transport adapter consumes it.
type RouteLocator interface {
	// Locate returns the peer that owns the given route.
	// When local is true, this instance should handle the route and peer is nil.
	// When local is false, peer contains the owning instance's endpoints.
	Locate(ctx context.Context, routeID string) (peer *persistence.PeerInfo, local bool, err error)
}

// MessageForwarder sends a message to another gobridge instance for processing.
// The HTTP transport adapter implements this using HTTP POST.
// The receiverID identifies the remote receiver's mounted path, which may differ
// from the route ID when a route references a receiver with a different name.
//
// Contract:
//
//   - Success boundary. Forward returns nil ONLY after the remote instance
//     has durably accepted the envelope into its own delivery pipeline (an
//     HTTP 2xx from the peer's ingress endpoint), where it is settled under
//     the peer's at-least-once semantics. A nil return lets the caller settle
//     the source, so an implementation MUST NOT report success on a mere
//     connection write or a pre-processing 2xx that the peer can lose on crash.
//
//   - At-least-once + idempotency. Forwarding is at-least-once: a timeout or
//     transient failure after the peer read the request re-posts the same
//     envelope. The implementation MUST propagate a stable idempotency key
//     (the envelope's own idempotency/dedup key, or a fallback derived from
//     the envelope ID) so the receiving side can drop replays. Cross-node
//     delivery is therefore effectively-once at best, never exactly-once.
//
//   - Fencing. Forward does NOT fence cluster ownership: the RouteLocator that
//     chose peer may be stale and name a former owner. Correctness rests on
//     the receiving instance authorizing the call (forward token / API key)
//     and on idempotent, at-least-once handoff — not on the forwarder
//     guaranteeing the peer still owns the route.
//
//   - Errors. On failure Forward returns a *shared.BridgeError (or an error
//     wrapping one, e.g. shared.ErrForwardFailed) so the caller can classify
//     transient (retry) vs permanent (dead-letter).
type MessageForwarder interface {
	Forward(ctx context.Context, peer *persistence.PeerInfo, receiverID string, env *messaging.Envelope) error
}

// EndpointResolver discovers this instance's externally-reachable address.
// Called once at startup. The listenAddr is the local HTTP listen address
// (e.g. ":9090") from the HTTP server configuration. Implementations probe
// the runtime environment to determine the externally-reachable DNS name or IP.
type EndpointResolver interface {
	Resolve(ctx context.Context, listenAddr string) (map[string]string, error)
}
