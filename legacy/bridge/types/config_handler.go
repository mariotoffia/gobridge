package types

import "context"

// ConfigChangeHandler processes configuration changes.
// This interface allows the bridge to react to dynamic configuration updates
// from a ConfigSource.
//
// Implementations should handle changes atomically where possible and
// coordinate with LifecycleCoordinator for shared connections.
type ConfigChangeHandler interface {
	// HandleConnectionChange processes connection add/update/delete.
	// For add: Creates and starts a new connection.
	// For update: Updates connection settings (may trigger reconnect).
	// For delete: Drains and removes the connection.
	HandleConnectionChange(ctx context.Context, change ConfigChange) error

	// HandleSourceChange processes source add/update/delete.
	// Uses LifecycleCoordinator if the connection supports it for atomic changes.
	HandleSourceChange(ctx context.Context, change ConfigChange) error

	// HandleTargetChange processes target add/update/delete.
	// Uses LifecycleCoordinator if the connection supports it for atomic changes.
	HandleTargetChange(ctx context.Context, change ConfigChange) error

	// HandleBatchChanges processes multiple changes atomically when possible.
	// Groups changes by connection and uses LifecycleTransaction for atomic application.
	// Falls back to individual processing if atomic operations are not supported.
	HandleBatchChanges(ctx context.Context, changes []ConfigChange) error
}

// ConfigChangeRouter routes configuration changes to the appropriate handler.
// This is a convenience interface for implementations that want to handle
// all configuration types in a single place.
type ConfigChangeRouter interface {
	// Route dispatches a configuration change to the appropriate handler.
	// Returns an error if the change type is not recognized or handling fails.
	Route(ctx context.Context, change ConfigChange) error

	// RouteBatch dispatches multiple configuration changes.
	// May optimize by batching changes that can be applied atomically.
	RouteBatch(ctx context.Context, changes []ConfigChange) error
}
