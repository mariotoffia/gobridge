package types

import (
	"context"
)

type TransportType string

const (
	TransportTypeMQTT            TransportType = "MQTT"
	TransportTypeAzureServiceBus TransportType = "AzureServiceBus"
	TransportTypeSQS             TransportType = "SQS"
)

// Transport is a interface for a remote server connection. This
// may be e.g. a _MQTT_ broker or a _Azure Service Bus_ instance.
//
// NOTE: A `Transport` may be unidirectional or bidirectional.
//
// If it is a _MQ_ type, it should also implement the `Publisher` and
// `Subscriber` interfaces since this is the base interface for all bridge transports.
type Transport interface {
	// GetID returns the unique identifier of the transport. This ID is a persistent _ID_ that
	// should uniquely identify the transport across restarts.
	GetID() string
	// GetTransportType returns the type of the transport (e.g., "MQTT", "AzureServiceBus").
	GetTransportType() TransportType
	// ListenAndServe starts the transport connection and listens for incoming messages.
	//
	// This is only applicable for bidirectional transports. If not bidirectional, this method
	// will return `TransportNotBidirectionalError`.
	//
	// TIP: Use a context that can be cancelled to stop the transport.
	ListenAndServe(ctx context.Context) error
}

// Transports is a slice of Transport interfaces.
type Transports []Transport

// TransportConfig defines the configuration required to create a Transport.
type TransportConfig interface {
	// GetID returns the unique identifier of the transport. This ID is a persistent _ID_ that
	// should uniquely identify the transport across restarts.
	GetID() string
	// GetTransportType returns the type of the transport (e.g., "MQTT", "AzureServiceBus").
	GetTransportType() TransportType
	// GetCustomConfig returns any custom configuration specific to the transport type.
	//
	// This is handed over to the `ServerRegistry` when a new transport is wanted.
	GetCustomConfig() any
}
