package types

// Config is required for all `ConnectionConfig` parts.
type Config interface {
	// GetID returns the unique identifier of the subscriber configuration. This _ID_ must be unique
	// across restarts and uniquely identify the subscriber configuration within the `ConnectionConfig`.
	GetID() string
	// GetTransportType returns the type of the transport (e.g., "MQTT", "AzureServiceBus").
	GetTransportType() TransportType
}

// ConnectionConfig defines the configuration required to create a Connection and configure all parts of it.
type ConnectionConfig interface {
	Config
	// GetBridgeID returns the unique identifier of the bridge this connection belongs to.
	//
	// This is useful when multiple bridges collaborates together to e.g. share publishers but
	// re-/elective handle subscriptions for e.g. failover.
	GetBridgeID() string
}

type TopicConfigTarget int

const (
	// TopicConfigTargetPublisher indicates that the topic configuration is for a `Publisher`.
	TopicConfigTargetPublisher TopicConfigTarget = 1
	// TopicConfigTargetSubscriber indicates that the topic configuration is for a `SubscriberSource`.
	TopicConfigTargetSubscriber TopicConfigTarget = 2
)

// TopicConfig represents a configuration for one or more topics.
type TopicConfig interface {
	Config
	// GetTopics returns the list of topics this configuration applies to.
	//
	// NOTE: That the topics may be wildcard topics depending on the `Connection` capabilities.
	//
	// If this list is empty, the `GetID` returns the one and only topic.
	GetTopics() []string
	// GetQoS returns the desired QoS level for the topic configuration.
	//
	// If the transport does not support QoS levels, it may return `nil`.
	GetQoS() *QosLevel
	// GetMeta may return any additional metadata associated with the topic configuration.
	GetMeta() map[string]any
	// GetTopicTarget returns whether this topic configuration is for a `Publisher` or `SubscriberSource`.
	GetTopicTarget() TopicConfigTarget
}
