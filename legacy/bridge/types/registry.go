package types

import "context"

// ConnectionRegistry defines an interface for registering and retrieving connections.
// This is the base read-only interface. For CRUD operations, see ConnectionAdminRegistry.
type ConnectionRegistry interface {
	// RegisterConnection adds a connection to the registry. If already registered, it will overwrite the existing one.
	RegisterConnection(connection Connection) error
	// GetConnection retrieves a connection by its unique ID. If not found, it returns an `types.ErrNotFound` error.
	GetConnection(id string) (Connection, error)
	// ListConnections returns a list of all registered connections.
	ListConnections() ([]Connection, error)
	// RemoveConnection removes a connection from the registry by its unique ID. If not found it returns an `types.ErrNotFound` error.
	RemoveConnection(id string) error
	// CreateConnection creates a new connection instance based on the provided configuration.
	//
	// If the connection type is not registered, it returns an `types.ErrNotFound` error.
	CreateConnection(ctx context.Context, config ConnectionConfig) (Connection, error)
}

// ConnectionAdminRegistry provides CRUD operations for connections.
// This interface is optional - implementations that support dynamic
// configuration changes should implement this in addition to ConnectionRegistry.
//
// Use type assertion to check if a registry supports admin operations:
//
//	adminRegistry, ok := registry.(types.ConnectionAdminRegistry)
//	if !ok {
//	    return fmt.Errorf("registry does not support admin operations")
//	}
type ConnectionAdminRegistry interface {
	ConnectionRegistry

	// UpdateConnection updates an existing connection's settings.
	// Coordinates drain/reconnect if RequiresReconnect() is true.
	// Returns ErrNotFound if the connection doesn't exist.
	UpdateConnection(ctx context.Context, id string, settings ConnectionSettingsConfig) error

	// DeleteConnection removes a connection after draining and closing it.
	// Returns ErrNotFound if the connection doesn't exist.
	DeleteConnection(ctx context.Context, id string) error

	// GetOrCreateConnection gets an existing connection or creates a new one.
	// Useful for ensuring idempotent operations.
	// If a connection with the same ID exists, returns it.
	// Otherwise, creates a new connection from the config.
	GetOrCreateConnection(ctx context.Context, config ConnectionConfig) (Connection, error)

	// ListConnectionIDs returns all connection IDs in the registry.
	ListConnectionIDs(ctx context.Context) ([]string, error)
}
