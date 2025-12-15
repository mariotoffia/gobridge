package types

import (
	"context"
	"time"
)

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
	// GetTransportRetryConfig returns connection-specific transport retry configuration.
	// Returns nil to use bridge defaults.
	//
	// This is for TRANSPORT RETRY (infrastructure failures like connection, subscribe, publish).
	// Configuration hierarchy: Bridge (default) -> Connection (override)
	GetTransportRetryConfig() *TransportRetryConfig
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

// Note: SourceConfig is defined in source.go
// Note: TargetConfig is defined in target.go

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

// ============================================================================
// Configuration Source and Management
// ============================================================================

// ConfigChangeType indicates what type of configuration change occurred.
type ConfigChangeType string

const (
	// ConfigChangeAdd indicates a new configuration item was added.
	ConfigChangeAdd ConfigChangeType = "add"
	// ConfigChangeUpdate indicates an existing configuration item was updated.
	ConfigChangeUpdate ConfigChangeType = "update"
	// ConfigChangeDelete indicates a configuration item was deleted.
	ConfigChangeDelete ConfigChangeType = "delete"
)

// ConfigItemType indicates the type of configuration item.
type ConfigItemType string

const (
	ConfigItemTypePipeline     ConfigItemType = "pipeline"
	ConfigItemTypeRoute        ConfigItemType = "route"
	ConfigItemTypeSource       ConfigItemType = "source"
	ConfigItemTypeTarget       ConfigItemType = "target"
	ConfigItemTypeTopic        ConfigItemType = "topic"
	ConfigItemTypeSubscription ConfigItemType = "subscription"
	ConfigItemTypeConnection   ConfigItemType = "connection"
	ConfigItemTypeMiddleware   ConfigItemType = "middleware"
)

// ConfigItem represents a single configuration item.
// Designed to be friendly with DynamoDB-style storage (partition key + sort key).
type ConfigItem interface {
	// GetPartitionKey returns the partition key for this item.
	// Example: "cluster:default", "bridge:bridge-1", "pipeline:mqtt-to-sqs"
	GetPartitionKey() string
	// GetSortKey returns the sort key for this item.
	// Example: "topic:sensors/temperature", "subscription:queue-1"
	GetSortKey() string
	// GetType returns the type of configuration item.
	GetType() ConfigItemType
	// GetVersion returns the version of this item (for optimistic locking).
	GetVersion() int64
	// GetData returns the actual configuration data.
	GetData() any
	// GetUpdatedAt returns when this item was last updated.
	GetUpdatedAt() time.Time
}

// ConfigChange represents a change to the configuration.
type ConfigChange struct {
	// Type indicates what type of change occurred.
	Type ConfigChangeType `json:"type"`
	// Item is the configuration item that changed.
	// For delete operations, this contains the item before deletion.
	Item ConfigItem `json:"item"`
	// Timestamp is when the change occurred.
	Timestamp time.Time `json:"timestamp"`
}

// ConfigSource provides configuration to the bridge.
// This is the primary interface for loading configuration from external sources
// (DynamoDB, Consul, files, etc.).
//
// The bridge uses ConfigSource directly, or through a ClusterConfigurator
// which decorates it with cluster-awareness.
type ConfigSource interface {
	// Discover performs initial configuration discovery.
	// Called during bridge startup to load all relevant configuration.
	Discover(ctx context.Context) ([]ConfigItem, error)

	// Watch returns a channel that receives configuration changes.
	// The implementation may use polling or push-based notifications.
	// The channel is closed when the context is cancelled.
	Watch(ctx context.Context) (<-chan ConfigChange, error)

	// Get retrieves a specific configuration item.
	Get(ctx context.Context, partitionKey, sortKey string) (ConfigItem, error)

	// List retrieves all items matching the partition key.
	List(ctx context.Context, partitionKey string) ([]ConfigItem, error)
}

// ConfigWriter allows writing configuration (for admin APIs).
// Not all ConfigSource implementations need to support writing.
type ConfigWriter interface {
	// Write creates or updates a configuration item.
	// The version in the item is used for optimistic locking:
	// - If version is 0, creates a new item (fails if exists)
	// - If version > 0, updates only if current version matches
	Write(ctx context.Context, item ConfigItem) error

	// Delete removes a configuration item.
	// If version > 0, deletes only if current version matches.
	Delete(ctx context.Context, partitionKey, sortKey string, version int64) error
}

// ConfigSourceWriter combines read and write operations.
type ConfigSourceWriter interface {
	ConfigSource
	ConfigWriter
}

// ConfigNotifier provides notification-based configuration updates.
// This is an optional interface that ConfigSource implementations may support
// for push-based notifications instead of polling.
type ConfigNotifier interface {
	// Subscribe registers a handler for configuration changes.
	// Returns an unsubscribe function to stop receiving notifications.
	Subscribe(handler func(ConfigChange)) (unsubscribe func())
}

// ConfigPoller provides poll-based configuration updates.
// This is an optional interface for ConfigSource implementations
// that support efficient polling.
type ConfigPoller interface {
	// Poll retrieves all changes since the given timestamp.
	Poll(ctx context.Context, since time.Time) ([]ConfigChange, error)
}
