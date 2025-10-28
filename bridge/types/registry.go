package types

import "context"

// ConnectionRegistry defines an interface for registering and retrieving connections.
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
