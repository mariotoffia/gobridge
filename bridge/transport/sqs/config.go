// Package sqs provides SQS transport implementation using AWS SDK v2.
//
// # SQS as a Message Source
//
// SQS provides reliable message delivery with built-in retry and dead-letter queue support.
// When used as a source:
//
//   - Ack(): Deletes the message from the queue
//   - Nack(): Does NOT delete the message; it becomes visible again after visibility timeout
//   - Extend(): Extends visibility timeout to prevent redelivery during long processing
//
// # SQS as a Message Target
//
// When used as a target, messages are sent to an SQS queue.
// Send() returns nil when SQS acknowledges receipt. SQS then owns delivery.
//
// # SQS as a Retry Backing Store
//
// SQS can be used as the backing store for a RetryManager, providing:
//   - Durable retry queue that survives pod/container restarts
//   - Configurable retry delays via DelaySeconds
//   - Dead-letter queue integration for exhausted retries
//   - Clusterable retry handling (any worker can process retries)
package sqs

import (
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

const (
	// TransportType is the transport type identifier for SQS.
	TransportType types.TransportType = "SQS"
)

// ConnectionConfig holds common connection settings for SQS.
type ConnectionConfig struct {
	// Region is the AWS region.
	Region string `json:"region"`

	// Endpoint is the SQS endpoint URL (for LocalStack or custom endpoints).
	// If empty, the default AWS endpoint is used.
	Endpoint string `json:"endpoint,omitempty"`

	// UsePathStyle uses path-style URLs (required for LocalStack).
	UsePathStyle bool `json:"usePathStyle,omitempty"`

	// Profile is the AWS profile to use (optional).
	Profile string `json:"profile,omitempty"`
}

// SourceConfigImpl implements types.SourceConfig for SQS message receiving.
type SourceConfigImpl struct {
	// ID is the unique identifier for this source.
	ID string `json:"id"`

	// Connection holds the AWS connection settings.
	Connection ConnectionConfig `json:"connection"`

	// QueueURL is the SQS queue URL.
	// Either QueueURL or QueueName must be specified.
	QueueURL string `json:"queueUrl,omitempty"`

	// QueueName is the SQS queue name (will be resolved to URL).
	QueueName string `json:"queueName,omitempty"`

	// MaxMessages is the maximum number of messages to receive per poll (1-10).
	MaxMessages int32 `json:"maxMessages,omitempty"`

	// WaitTimeSeconds is the long poll wait time (0-20 seconds).
	WaitTimeSeconds int32 `json:"waitTimeSeconds,omitempty"`

	// VisibilityTimeout is the visibility timeout in seconds.
	VisibilityTimeout int32 `json:"visibilityTimeout,omitempty"`

	// Prefetch is the maximum number of messages to buffer locally.
	Prefetch int `json:"prefetch,omitempty"`

	// MessageAttributeNames are the message attribute names to retrieve.
	// Use ["All"] to retrieve all attributes.
	MessageAttributeNames []string `json:"messageAttributeNames,omitempty"`

	// Resources for resource-based lookup (optional).
	Resources []types.Tag `json:"resources,omitempty"`

	// AllowMultiple allows multiple resource matches.
	AllowMultiple bool `json:"allowMultiple,omitempty"`
}

// Ensure SourceConfigImpl implements types.SourceConfig
var _ types.SourceConfig = (*SourceConfigImpl)(nil)

func (c *SourceConfigImpl) GetID() string {
	return c.ID
}

func (c *SourceConfigImpl) GetTransportType() types.TransportType {
	return TransportType
}

func (c *SourceConfigImpl) GetQoS() *types.QosLevel {
	// SQS is at-least-once by nature
	return &types.QosLevel{Level: 1}
}

func (c *SourceConfigImpl) GetPrefetch() int {
	return c.Prefetch
}

func (c *SourceConfigImpl) GetResources() []types.Tag {
	return c.Resources
}

func (c *SourceConfigImpl) AllowMultipleResourceMatches() bool {
	return c.AllowMultiple
}

// TargetConfigImpl implements types.TargetConfig for SQS message sending.
type TargetConfigImpl struct {
	// ID is the unique identifier for this target.
	ID string `json:"id"`

	// Connection holds the AWS connection settings.
	Connection ConnectionConfig `json:"connection"`

	// QueueURL is the SQS queue URL.
	// Either QueueURL or QueueName must be specified.
	QueueURL string `json:"queueUrl,omitempty"`

	// QueueName is the SQS queue name (will be resolved to URL).
	QueueName string `json:"queueName,omitempty"`

	// DelaySeconds is the delay before the message becomes visible (0-900).
	DelaySeconds int32 `json:"delaySeconds,omitempty"`

	// BatchSize is the number of messages to send in a batch (1-10).
	BatchSize int `json:"batchSize,omitempty"`

	// Timeout is the timeout for send operations.
	Timeout time.Duration `json:"timeout,omitempty"`

	// MessageGroupID is the message group ID for FIFO queues.
	MessageGroupID string `json:"messageGroupId,omitempty"`

	// Resources for resource-based lookup (optional).
	Resources []types.Tag `json:"resources,omitempty"`

	// AllowMultiple allows multiple resource matches.
	AllowMultiple bool `json:"allowMultiple,omitempty"`
}

// Ensure TargetConfigImpl implements types.TargetConfig
var _ types.TargetConfig = (*TargetConfigImpl)(nil)

func (c *TargetConfigImpl) GetID() string {
	return c.ID
}

func (c *TargetConfigImpl) GetTransportType() types.TransportType {
	return TransportType
}

func (c *TargetConfigImpl) GetDefaultQoS() *types.QosLevel {
	return &types.QosLevel{Level: 1}
}

func (c *TargetConfigImpl) GetBatchSize() int {
	return c.BatchSize
}

func (c *TargetConfigImpl) GetTimeout() *time.Duration {
	if c.Timeout == 0 {
		return nil
	}
	return &c.Timeout
}

func (c *TargetConfigImpl) GetResources() []types.Tag {
	return c.Resources
}

func (c *TargetConfigImpl) AllowMultipleResourceMatches() bool {
	return c.AllowMultiple
}
