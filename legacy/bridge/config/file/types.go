// Package file provides a file-based ConfigSource implementation for loading
// bridge configuration from YAML or JSON files.
package file

import (
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// Format specifies the configuration file format.
type Format string

const (
	// FormatAuto auto-detects the format based on file extension.
	FormatAuto Format = "auto"
	// FormatYAML specifies YAML format.
	FormatYAML Format = "yaml"
	// FormatJSON specifies JSON format.
	FormatJSON Format = "json"
)

// ============================================================================
// File Configuration Schema
// ============================================================================

// FileConfig is the root configuration file structure.
// This represents the complete configuration that can be loaded from a file.
type FileConfig struct {
	// Bridge contains bridge-level settings.
	Bridge BridgeSection `yaml:"bridge" json:"bridge"`
	// Connections defines transport connections.
	Connections []ConnectionConfig `yaml:"connections" json:"connections"`
	// Pipelines defines message pipelines.
	Pipelines []PipelineConfig `yaml:"pipelines" json:"pipelines"`
	// Routes defines multi-pipeline routes.
	Routes []RouteConfig `yaml:"routes" json:"routes"`
}

// BridgeSection contains bridge-level configuration.
type BridgeSection struct {
	// ID is the unique identifier for this bridge instance.
	ID string `yaml:"id" json:"id"`
	// ClusterID is the cluster this bridge belongs to.
	ClusterID string `yaml:"clusterId" json:"clusterId"`
	// ShutdownTimeout is the graceful shutdown timeout.
	ShutdownTimeout string `yaml:"shutdownTimeout" json:"shutdownTimeout"`
	// DrainTimeout is the pipeline drain timeout.
	DrainTimeout string `yaml:"drainTimeout" json:"drainTimeout"`
	// TransportRetry contains default transport retry settings.
	TransportRetry *TransportRetrySection `yaml:"transportRetry,omitempty" json:"transportRetry,omitempty"`
	// FlowControl contains default flow control settings.
	FlowControl *FlowControlSection `yaml:"flowControl,omitempty" json:"flowControl,omitempty"`
}

// TransportRetrySection contains transport retry configuration.
type TransportRetrySection struct {
	// InitialBackoff is the initial backoff duration (e.g., "1s").
	InitialBackoff string `yaml:"initialBackoff" json:"initialBackoff"`
	// MaxBackoff is the maximum backoff duration (e.g., "5m").
	MaxBackoff string `yaml:"maxBackoff" json:"maxBackoff"`
	// Multiplier is the backoff multiplier.
	Multiplier float64 `yaml:"multiplier" json:"multiplier"`
	// Jitter is the random jitter factor (0.0 to 1.0).
	Jitter float64 `yaml:"jitter" json:"jitter"`
	// InfrastructureBackoffMultiplier multiplies backoff for infrastructure errors.
	InfrastructureBackoffMultiplier float64 `yaml:"infrastructureBackoffMultiplier" json:"infrastructureBackoffMultiplier"`
	// SkipNativeRetry skips retry for transports with native retry.
	SkipNativeRetry bool `yaml:"skipNativeRetry" json:"skipNativeRetry"`
}

// FlowControlSection contains flow control configuration.
type FlowControlSection struct {
	// MaxInFlight is the maximum number of in-flight messages.
	MaxInFlight int `yaml:"maxInFlight" json:"maxInFlight"`
	// DefaultMessageTTL is the default message TTL (e.g., "2m").
	DefaultMessageTTL string `yaml:"defaultMessageTTL" json:"defaultMessageTTL"`
}

// ConnectionConfig defines a transport connection.
type ConnectionConfig struct {
	// ID is the unique identifier for this connection.
	ID string `yaml:"id" json:"id"`
	// Type is the transport type (e.g., "mqtt", "sqs", "servicebus").
	Type string `yaml:"type" json:"type"`
	// BrokerURLs are the broker/endpoint URLs.
	BrokerURLs []string `yaml:"brokerUrls" json:"brokerUrls"`
	// ClientID is the client identifier (for protocols like MQTT).
	ClientID string `yaml:"clientId,omitempty" json:"clientId,omitempty"`
	// Credentials contains authentication credentials.
	Credentials *CredentialsSection `yaml:"credentials,omitempty" json:"credentials,omitempty"`
	// Options contains transport-specific options.
	Options map[string]any `yaml:"options,omitempty" json:"options,omitempty"`
	// TransportRetry overrides transport retry settings.
	TransportRetry *TransportRetrySection `yaml:"transportRetry,omitempty" json:"transportRetry,omitempty"`
}

// CredentialsSection contains authentication credentials.
type CredentialsSection struct {
	// Username for basic auth.
	Username string `yaml:"username,omitempty" json:"username,omitempty"`
	// Password for basic auth.
	Password string `yaml:"password,omitempty" json:"password,omitempty"`
	// URI is a credentials resolver URI (e.g., "pms://...").
	URI string `yaml:"uri,omitempty" json:"uri,omitempty"`
	// CACert is the CA certificate (inline or file path).
	CACert string `yaml:"caCert,omitempty" json:"caCert,omitempty"`
	// ClientCert is the client certificate (inline or file path).
	ClientCert string `yaml:"clientCert,omitempty" json:"clientCert,omitempty"`
	// ClientKey is the client private key (inline or file path).
	ClientKey string `yaml:"clientKey,omitempty" json:"clientKey,omitempty"`
}

// PipelineConfig defines a message pipeline.
type PipelineConfig struct {
	// ID is the unique identifier for this pipeline.
	ID string `yaml:"id" json:"id"`
	// Source defines the message source.
	Source SourceSection `yaml:"source" json:"source"`
	// Target defines the message target.
	Target TargetSection `yaml:"target" json:"target"`
	// Middlewares are the middleware names to apply.
	Middlewares []string `yaml:"middlewares,omitempty" json:"middlewares,omitempty"`
	// Mode is the pipeline mode (e.g., "streaming", "batch").
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// FlowControl overrides flow control settings.
	FlowControl *FlowControlSection `yaml:"flowControl,omitempty" json:"flowControl,omitempty"`
	// Retry configures message retry policy.
	Retry *MessageRetrySection `yaml:"retry,omitempty" json:"retry,omitempty"`
}

// SourceSection defines a pipeline source.
type SourceSection struct {
	// ConnectionID references a connection by ID.
	ConnectionID string `yaml:"connectionId,omitempty" json:"connectionId,omitempty"`
	// Type is the source type (if not using a connection).
	Type string `yaml:"type,omitempty" json:"type,omitempty"`
	// Topics are the topics/queues to subscribe to.
	Topics []string `yaml:"topics,omitempty" json:"topics,omitempty"`
	// Options contains source-specific options.
	Options map[string]any `yaml:"options,omitempty" json:"options,omitempty"`
}

// TargetSection defines a pipeline target.
type TargetSection struct {
	// ConnectionID references a connection by ID.
	ConnectionID string `yaml:"connectionId,omitempty" json:"connectionId,omitempty"`
	// Type is the target type (if not using a connection).
	Type string `yaml:"type,omitempty" json:"type,omitempty"`
	// Topic is the destination topic/queue.
	Topic string `yaml:"topic,omitempty" json:"topic,omitempty"`
	// Options contains target-specific options.
	Options map[string]any `yaml:"options,omitempty" json:"options,omitempty"`
}

// MessageRetrySection contains message retry configuration.
type MessageRetrySection struct {
	// MaxAttempts is the maximum retry attempts.
	MaxAttempts int `yaml:"maxAttempts" json:"maxAttempts"`
	// InitialBackoff is the initial backoff duration.
	InitialBackoff string `yaml:"initialBackoff" json:"initialBackoff"`
	// MaxBackoff is the maximum backoff duration.
	MaxBackoff string `yaml:"maxBackoff" json:"maxBackoff"`
	// Multiplier is the backoff multiplier.
	Multiplier float64 `yaml:"multiplier" json:"multiplier"`
}

// RouteConfig defines a multi-pipeline route.
type RouteConfig struct {
	// ID is the unique identifier for this route.
	ID string `yaml:"id" json:"id"`
	// PipelineIDs are the pipeline IDs in this route.
	PipelineIDs []string `yaml:"pipelineIds" json:"pipelineIds"`
}

// ============================================================================
// ConfigItem Implementation
// ============================================================================

// fileConfigItem implements types.ConfigItem for file-based configuration.
type fileConfigItem struct {
	partitionKey string
	sortKey      string
	itemType     types.ConfigItemType
	version      int64
	data         any
	updatedAt    time.Time
}

// GetPartitionKey returns the partition key.
func (i *fileConfigItem) GetPartitionKey() string {
	return i.partitionKey
}

// GetSortKey returns the sort key.
func (i *fileConfigItem) GetSortKey() string {
	return i.sortKey
}

// GetType returns the config item type.
func (i *fileConfigItem) GetType() types.ConfigItemType {
	return i.itemType
}

// GetVersion returns the version.
func (i *fileConfigItem) GetVersion() int64 {
	return i.version
}

// GetData returns the configuration data.
func (i *fileConfigItem) GetData() any {
	return i.data
}

// GetUpdatedAt returns when the item was last updated.
func (i *fileConfigItem) GetUpdatedAt() time.Time {
	return i.updatedAt
}

// Ensure fileConfigItem implements types.ConfigItem
var _ types.ConfigItem = (*fileConfigItem)(nil)
