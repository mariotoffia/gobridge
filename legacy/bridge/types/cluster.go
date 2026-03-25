package types

import "context"

// BridgeIdentity uniquely identifies a bridge instance within a cluster.
type BridgeIdentity struct {
	// BridgeID is the unique identifier of this bridge within the cluster.
	// This could be a Fargate task ID, hostname, UUID, etc.
	BridgeID string `json:"bridgeId"`
	// ClusterID is the identifier of the cluster this bridge belongs to.
	// If empty, defaults to "default".
	ClusterID string `json:"clusterId,omitempty"`
}

// GetClusterID returns the cluster ID, defaulting to "default" if not set.
func (b BridgeIdentity) GetClusterID() string {
	if b.ClusterID == "" {
		return "default"
	}
	return b.ClusterID
}

// ClusterConfigurator decorates a ConfigSource with cluster-awareness.
//
// All cluster internals are hidden within the implementation:
//   - Leader election
//   - Membership negotiation
//   - Shared subscriptions coordination
//   - Drain coordination between nodes
//
// The bridge code doesn't need to know about clustering details - it just
// uses ClusterConfigurator as a ConfigSource and receives an adjusted
// configuration view based on cluster state.
//
// Example usage:
//
//	// Without clustering - use ConfigSource directly
//	bridge := NewBridge(configSource)
//
//	// With clustering - ClusterConfigurator wraps ConfigSource
//	clusterConfig := NewClusterConfigurator(configSource, identity, opts)
//	bridge := NewBridge(clusterConfig)  // Same interface!
type ClusterConfigurator interface {
	// Embed ConfigSource - the cluster configurator presents a filtered/adjusted
	// configuration view to the bridge based on cluster state.
	ConfigSource

	// GetIdentity returns this bridge's unique identity within the cluster.
	GetIdentity() BridgeIdentity

	// RequestDrain signals intent to drain this bridge instance.
	// The implementation coordinates with other cluster members to:
	//   - Stop accepting new work
	//   - Transfer in-flight work if possible
	//   - Wait for current work to complete
	// Returns when draining is complete or context is cancelled.
	RequestDrain(ctx context.Context) error

	// IsDraining returns true if this bridge is currently draining.
	IsDraining() bool

	// IsLeader returns true if this bridge is the current leader.
	// Leadership is used for cluster-wide decisions but the details
	// are hidden within the implementation.
	IsLeader() bool

	// WaitForReady blocks until the cluster configurator is ready.
	// This includes joining the cluster, discovering peers, and
	// establishing the initial configuration view.
	WaitForReady(ctx context.Context) error
}

// ClusterConfiguratorOption is a functional option for configuring ClusterConfigurator.
type ClusterConfiguratorOption func(*ClusterConfiguratorOptions)

// ClusterConfiguratorOptions holds configuration for ClusterConfigurator.
type ClusterConfiguratorOptions struct {
	// HeartbeatInterval is how often to send heartbeats to other nodes.
	HeartbeatInterval string `json:"heartbeatInterval,omitempty"` // e.g., "5s"
	// HeartbeatTimeout is how long before a node is considered dead.
	HeartbeatTimeout string `json:"heartbeatTimeout,omitempty"` // e.g., "30s"
	// DrainTimeout is the maximum time to wait for draining.
	DrainTimeout string `json:"drainTimeout,omitempty"` // e.g., "5m"
	// SharedSubscriptionStrategy defines how to handle shared subscriptions.
	// Options depend on the implementation (e.g., "round-robin", "sticky").
	SharedSubscriptionStrategy string `json:"sharedSubscriptionStrategy,omitempty"`
}

// ClusterState represents the current state of the cluster as seen by this bridge.
// This is exposed for monitoring/debugging purposes.
type ClusterState struct {
	// Members is the number of active cluster members.
	Members int `json:"members"`
	// LeaderID is the ID of the current leader.
	LeaderID string `json:"leaderId,omitempty"`
	// IsReady indicates if the cluster is ready for operation.
	IsReady bool `json:"isReady"`
	// DrainingMembers is the number of members currently draining.
	DrainingMembers int `json:"drainingMembers"`
}

// ClusterStateProvider is an optional interface for ClusterConfigurator implementations
// that expose cluster state for monitoring.
type ClusterStateProvider interface {
	// GetClusterState returns the current cluster state.
	GetClusterState() ClusterState
}

