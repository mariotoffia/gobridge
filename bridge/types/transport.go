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

// Transport is a interface for a remote server connection. This
// may be e.g. a _MQTT_ broker or a _Azure Service Bus_ instance.
//
// NOTE: A `Transport` may be unidirectional or bidirectional.
//
// If it is a _MQ_ type, it should also implement the `Publisher` and
// `SubscriberSource` interfaces since this is the base interface for all bridge transports.
//
// It implements `io.Closer` to allow proper resource cleanup such as draining the messages and
// return when closed and fully drained.
type Transport interface {
	// Close closes the transport and releases all resources by first draining all messages and orderly
	// shutting down the connection(s).
	//
	// This has to be called after a `Start` to properly release all resources.
	io.Closer
	// GetID returns the unique identifier of the transport. This ID is a persistent _ID_ that
	// should uniquely identify the transport across restarts.
	GetID() string
	// GetTransportType returns the type of the transport (e.g., "MQTT", "AzureServiceBus").
	GetTransportType() TransportType
	// Start starts the transport connection and listens for incoming messages and also
	// able to send messages.
	//
	// This is only applicable for bidirectional transports. If not bidirectional, this method
	// will return `TransportNotBidirectionalError`.
	//
	// TIP: Use a context that can be cancelled to stop the transport.
	//
	// When the _initial_ configuration is passed, all source and target are configured according to it before
	// accepting messages. If it fails to configure the transport it will return an error.
	//
	// If the source and targets support dynamic re-configuration, an external actor may change the configuration
	// during runtime.
	//
	// It will still be in _Start_ mode until the `Close` method is called.
	Start(ctx context.Context, initial TransportConfig) error
	// Capabilities returns the capabilities supported by the transport (and the different topics/subscribers/publishers).
	//
	// NOTE: Depending on the configuration of the `SubscriberSource`, `Publisher`, and even specific topic
	// within those, the capabilities may vary.
	//
	// If zero topics are presented, it should return the generic ("most supported") capabilities of the transport.
	Capabilities(topics ...string) map[string]Capabilities
}

// Transports is a slice of Transport interfaces.
type Transports []Transport
