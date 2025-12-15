package types

import (
	"context"
	"errors"
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
//
// # Shared Connection Pattern
//
// For transports that use a single underlying connection for both subscribing and publishing
// (like MQTT), the Connection can implement SourceProvider and TargetProvider to create
// Source and Target instances that share the underlying transport connection.
//
// Use SourceProvider() and TargetProvider() to check if the connection supports creating
// sources and targets respectively. These return nil if not supported.
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
	// If the _override_ configuration is passed, all source and target are configured according to it before
	// accepting messages. Any earlier configuration e.g from `Registry.CreateConnection` will be discarded.
	//
	// If it fails to configure the connection it will return an error.
	//
	// If the source and targets support dynamic re-configuration, an external actor may change the configuration
	// during runtime.
	//
	// It will still be in _Start_ mode until the `Close` method is called.
	Start(ctx context.Context, override ConnectionConfig) error
	// Capabilities returns the capabilities supported by the connection (and the different topics/subscribers/publishers).
	//
	// NOTE: Depending on the configuration of the `SubscriberSource`, `Publisher`, and even specific topic
	// within those, the capabilities may vary.
	//
	// If zero topics are presented, it should return the generic ("most supported") capabilities of the connection.
	Capabilities(topics ...string) map[string]Capabilities
	// SourceProvider returns a SourceProvider if this connection supports creating sources
	// that share the underlying transport connection. Returns nil if not supported.
	//
	// This enables transports like MQTT to use a single connection for both subscribing
	// and publishing, where Source and Target instances share the same client.
	SourceProvider() SourceProvider
	// TargetProvider returns a TargetProvider if this connection supports creating targets
	// that share the underlying transport connection. Returns nil if not supported.
	//
	// This enables transports like MQTT to use a single connection for both subscribing
	// and publishing, where Source and Target instances share the same client.
	TargetProvider() TargetProvider
}

// SourceProvider creates Source instances from a shared Connection.
// This is used by connections that support creating sources that share the underlying
// transport connection (e.g., MQTT where subscriptions and publishing use the same client).
type SourceProvider interface {
	// CreateSource creates a new Source from the given configuration.
	// The Source shares the underlying transport connection with other sources/targets
	// created from the same Connection.
	//
	// The Source's Close() method should NOT close the underlying connection -
	// only the Connection's Close() should do that.
	CreateSource(ctx context.Context, config SourceConfig) (Source, error)
}

// TargetProvider creates Target instances from a shared Connection.
// This is used by connections that support creating targets that share the underlying
// transport connection (e.g., MQTT where subscriptions and publishing use the same client).
type TargetProvider interface {
	// CreateTarget creates a new Target from the given configuration.
	// The Target shares the underlying transport connection with other sources/targets
	// created from the same Connection.
	//
	// The Target's Close() method should NOT close the underlying connection -
	// only the Connection's Close() should do that.
	CreateTarget(ctx context.Context, config TargetConfig) (Target, error)
}

// Connections is a slice of Connection interfaces.
type Connections []Connection

// Close implements the `io.Closer` interface for the Connections slice.
// It closes all connections and joins all errors using errors.Join.
func (c Connections) Close() error {
	var errs []error

	for _, conn := range c {
		if err := conn.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}
