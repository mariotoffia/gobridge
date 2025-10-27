package types

import "context"

// TransportRegistry defines an interface for registering and retrieving servers.
type TransportRegistry interface {
	// RegisterTransport adds a server to the registry. If already registered, it will overwrite the existing one.
	RegisterTransport(server Transport) error
	// GetTransport retrieves a server by its unique ID. If not found, it returns an `types.ErrNotFound` error.
	GetTransport(id string) (Transport, error)
	// ListTransports returns a list of all registered transports.
	ListTransports() ([]Transport, error)
	// RemoveTransport removes a transport from the registry by its unique ID. If not found it returns an `types.ErrNotFound` error.
	RemoveTransport(id string) error
	// CreateTransport creates a new transport instance based on the provided configuration.
	//
	// If the transport type is not registered, it returns an `types.ErrNotFound` error.
	CreateTransport(ctx context.Context, config TransportConfig) (Transport, error)
}
