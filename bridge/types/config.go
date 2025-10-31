package types

// Tag represents a key-value pair where the `Tag.Value` is optional (empty).
type Tag struct {
	// Key is the tag key. e.g. queue-name
	Key string `json:"key"`
	// Value is the optional tag value. e.g. my-queue
	Value string `json:"value,omitempty"`
}

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

type ResourceBasedLookupConfig interface {
	// GetResources returns the list of resources to be used to do the lookup.
	//
	// These are key value pairs to match in e.g. AWS resource lookup API to
	// find the correct resource to use.
	GetResources() []Tag
	// AllowMultipleResourceMatches indicates whether multiple resource matches are allowed.
	//
	// If `false`, only one resource match is allowed and an error will be returned
	// if multiple resources are found.
	AllowMultipleResourceMatches() bool
}

type SourceConfig interface {
	Config
	ResourceBasedLookupConfig
	// GetQoS returns the desired QoS level when publishing messages to publish targets.
	//
	// If the transport does not support QoS levels, it may return `nil`.
	GetQoS() *QosLevel
}

type TargetConfig interface {
	Config
	ResourceBasedLookupConfig
}

// TopicConfig represents a configuration for one or more topics.
type TopicConfig interface {
	Config
	// GetTopics returns the list of topics this configuration applies to.
	//
	// NOTE: That the topics may be wildcard topics depending on the `Connection` capabilities.
	//
	// If this list is empty, the `GetID` returns the one and only topic.
	GetTopics() []string
	// GetMeta may return any additional metadata associated with the topic configuration.
	GetMeta() map[string]any
}

type TopicSubscriberConfig interface {
	TopicConfig
	// GetQoS returns the desired QoS level for the topic configuration.
	//
	// If the transport does not support QoS levels, it may return `nil`.
	GetQoS() *QosLevel
}
type TopicPublisherConfig interface {
	TopicConfig
}
