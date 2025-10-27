package types

import (
	"context"
)

type BridgeServerType string

const (
	BridgeServerTypeMQTT            BridgeServerType = "MQTT"
	BridgeServerTypeAzureServiceBus BridgeServerType = "AzureServiceBus"
)

type BridgeQosLevel struct {
	// Level is a QoS level supported by the server/topic.
	//
	// If the server do not support QoS levels as `int` it may require the `Custom` field to be set.
	Level int
	// Custom holds any additional metadata for the QoS level.
	Custom map[string]any
}

type BridgeMessage struct {
	// Topic is the topic of the message. This is always a concrete topic string and do never contain
	// any wildcards.
	Topic string
	// Payload is the payload of the message.
	Payload []byte
	// Qos is the QoS level of the message.
	Qos *BridgeQosLevel
	// Metadata is any additional metadata associated with the message.
	Metadata map[string]any
}

// BridgeServer is a interface for a remote server connection. This
// may be e.g. a MQTT broker or a _Azure Service Bus_ instance.
//
// NOTE: A `BridgeServer` may be unidirectional or bidirectional.
//
// If it is a _MQ_ type, it should also implement the `BridgePublisher` and
// `BridgeSubscriber` interfaces since this is the base interface for all bridge servers.
type BridgeServer interface {
	// GetID returns the unique identifier of the server.
	GetID() string

	// GetType returns the type of the server (e.g., "MQTT", "AzureServiceBus").
	GetType() BridgeServerType
	// ListenAndServe starts the server connection and listens for incoming messages.
	//
	// This is only applicable for bidirectional servers. If not bidirectional, this method
	// will return `ServerNotBidirectionalError`.
	//
	// TIP: Use a context that can be cancelled to stop the server.
	ListenAndServe(ctx context.Context) error
}

// BridgeServers is a slice of BridgeServer interfaces.
type BridgeServers []BridgeServer
