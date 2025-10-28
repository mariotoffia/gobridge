package types

import (
	"context"
	"io"
)

type TransportType string

const (
	TransportTypeMQTT            TransportType = "MQTT"
	TransportTypeAzureServiceBus TransportType = "AzureServiceBus"
	TransportTypeSQS             TransportType = "SQS"
)

// Connection is a interface for a remote server connection. This
// may be e.g. a _MQTT_ broker or a _Azure Service Bus_ instance.
//
// NOTE: A `Connection` may be unidirectional or bidirectional.
//
// If it is a _MQ_ type, it should also implement the `Publisher` and
// `SubscriberSource` interfaces since this is the base interface for all bridge connections.
//
// It implements `io.Closer` to allow proper resource cleanup such as draining the messages and
// return when closed and fully drained.
type Connection interface {
	// Close closes the connection and releases all resources by first draining all messages and orderly
	// shutting down the connection(s).
	//
	// This has to be called after a `Start` to properly release all resources.
	io.Closer
	// GetID returns the unique identifier of the connection. This ID is a persistent _ID_ that
	// should uniquely identify the connection across restarts.
	GetID() string
	// GetTransportType returns the type of the connection (e.g., "MQTT", "AzureServiceBus").
	GetTransportType() TransportType
	// Start starts the connection and listens for incoming messages and also
	// able to send messages.
	//
	// This is only applicable for bidirectional connections. If not bidirectional, this method
	// will return `ConnectionNotBidirectionalError`.
	//
	// TIP: Use a context that can be cancelled to stop the connection.
	//
	// When the _initial_ configuration is passed, all source and target are configured according to it before
	// accepting messages. If it fails to configure the connection it will return an error.
	//
	// If the source and targets support dynamic re-configuration, an external actor may change the configuration
	// during runtime.
	//
	// It will still be in _Start_ mode until the `Close` method is called.
	Start(ctx context.Context, initial ConnectionConfig) error
	// Capabilities returns the capabilities supported by the connection (and the different topics/subscribers/publishers).
	//
	// NOTE: Depending on the configuration of the `SubscriberSource`, `Publisher`, and even specific topic
	// within those, the capabilities may vary.
	//
	// If zero topics are presented, it should return the generic ("most supported") capabilities of the connection.
	Capabilities(topics ...string) map[string]Capabilities
}

// Connections is a slice of Connection interfaces.
type Connections []Connection
