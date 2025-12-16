// Package servicebus provides Azure Service Bus transport implementation.
//
// # Azure Service Bus Delivery Guarantees
//
// Azure Service Bus provides reliable message delivery with:
//   - At-least-once delivery (default)
//   - Sessions for ordered/grouped processing
//   - Dead-letter queue for failed messages
//   - Scheduled message delivery
//
// # Message Lifecycle
//
// When receiving messages:
//   - Ack(): Completes the message (removes from queue/subscription)
//   - Nack(): Abandons the message (returns to queue for redelivery)
//   - Extend(): Renews the message lock to prevent timeout
//
// When sending messages:
//   - Send() returns nil when Service Bus accepts the message
//   - Service Bus then owns delivery to receivers
//   - Do NOT retry after successful send
//
// # Integration with External Retry
//
// Azure Service Bus has built-in retry and dead-letter support. When used as a target:
//   - Send() blocks until Service Bus acknowledges
//   - On nil return: Message accepted - do not retry externally
//   - On error return: Check IsRecoverable for retry decision
package servicebus

import (
	"crypto/tls"
	"crypto/x509"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

const (
	// TransportType is the transport type identifier for Azure Service Bus.
	TransportType types.TransportType = "AzureServiceBus"
)

// ConnectionConfig holds common connection settings for Azure Service Bus.
type ConnectionConfig struct {
	// ConnectionString is the Service Bus connection string.
	// Either ConnectionString or (Namespace + Credential) must be provided.
	ConnectionString string `json:"connectionString,omitempty"`

	// Namespace is the fully qualified Service Bus namespace.
	// Example: "myservicebus.servicebus.windows.net"
	Namespace string `json:"namespace,omitempty"`

	// UseManagedIdentity uses Azure Managed Identity for authentication.
	// Only used when ConnectionString is not provided.
	UseManagedIdentity bool `json:"useManagedIdentity,omitempty"`

	// TenantID is the Azure AD tenant ID (for service principal auth).
	TenantID string `json:"tenantId,omitempty"`

	// ClientID is the Azure AD client ID (for service principal auth).
	ClientID string `json:"clientId,omitempty"`

	// ClientSecret is the Azure AD client secret (for service principal auth).
	ClientSecret string `json:"clientSecret,omitempty"`

	// TLSConfig provides custom TLS configuration for the connection.
	// This is primarily used for testing with self-signed certificates.
	// If nil, the default TLS configuration is used.
	TLSConfig *tls.Config `json:"-"`

	// CaPEM is the CA certificate(s) in PEM format for verifying the server.
	// Can be inline PEM data or a URI (e.g., "pms://path/to/ca") to resolve.
	// If types.IsServerURI() returns true, the URI will be resolved via
	// the credentials resolver.
	CaPEM string `json:"caPEM,omitempty"`

	// InsecureSkipVerify skips TLS certificate verification.
	// WARNING: Only use for testing with self-signed certificates.
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// SourceConfigImpl implements types.SourceConfig for Service Bus message receiving.
type SourceConfigImpl struct {
	// ID is the unique identifier for this source.
	ID string `json:"id"`

	// Connection holds the Azure connection settings.
	Connection ConnectionConfig `json:"connection"`

	// QueueName is the queue to receive from.
	// Either QueueName or (TopicName + SubscriptionName) must be specified.
	QueueName string `json:"queueName,omitempty"`

	// TopicName is the topic to receive from (requires SubscriptionName).
	TopicName string `json:"topicName,omitempty"`

	// SubscriptionName is the subscription on the topic.
	SubscriptionName string `json:"subscriptionName,omitempty"`

	// SessionID enables session-based receiving.
	// If set, only messages with this session ID are received.
	SessionID string `json:"sessionId,omitempty"`

	// MaxMessages is the maximum number of messages to receive per batch.
	MaxMessages int `json:"maxMessages,omitempty"`

	// MaxWaitTime is the maximum time to wait for messages.
	MaxWaitTime time.Duration `json:"maxWaitTime,omitempty"`

	// Prefetch is the number of messages to prefetch.
	Prefetch int32 `json:"prefetch,omitempty"`

	// ReceiveMode is the receive mode (PeekLock or ReceiveAndDelete).
	// Default is PeekLock.
	ReceiveMode string `json:"receiveMode,omitempty"`

	// SubQueue specifies which sub-queue to receive from.
	// Options: "" (main queue), "deadletter", "transferdeadletter"
	SubQueue string `json:"subQueue,omitempty"`

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
	// Service Bus is at-least-once by default
	return &types.QosLevel{Level: 1}
}

func (c *SourceConfigImpl) GetPrefetch() int {
	return int(c.Prefetch)
}

func (c *SourceConfigImpl) GetResources() []types.Tag {
	return c.Resources
}

func (c *SourceConfigImpl) AllowMultipleResourceMatches() bool {
	return c.AllowMultiple
}

// TargetConfigImpl implements types.TargetConfig for Service Bus message sending.
type TargetConfigImpl struct {
	// ID is the unique identifier for this target.
	ID string `json:"id"`

	// Connection holds the Azure connection settings.
	Connection ConnectionConfig `json:"connection"`

	// QueueName is the queue to send to.
	// Either QueueName or TopicName must be specified.
	QueueName string `json:"queueName,omitempty"`

	// TopicName is the topic to send to.
	TopicName string `json:"topicName,omitempty"`

	// DefaultSessionID is the default session ID for messages.
	DefaultSessionID string `json:"defaultSessionId,omitempty"`

	// BatchSize is the maximum number of messages per batch.
	BatchSize int `json:"batchSize,omitempty"`

	// Timeout is the timeout for send operations.
	Timeout time.Duration `json:"timeout,omitempty"`

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

// buildTLSConfig creates a tls.Config from CA PEM data and InsecureSkipVerify flag.
// If caPEM is a URI (detected by types.IsServerURI), it should be resolved by the caller
// before passing to this function.
// Returns nil if no TLS configuration is needed.
func buildTLSConfig(caPEM string, insecureSkipVerify bool) *tls.Config {
	if caPEM == "" && !insecureSkipVerify {
		return nil
	}

	tlsConfig := &tls.Config{
		InsecureSkipVerify: insecureSkipVerify,
	}

	// Add CA certificates if provided
	if caPEM != "" && !types.IsServerURI(caPEM) {
		// Inline PEM data - parse and add to root CAs
		certPool := x509.NewCertPool()
		if certPool.AppendCertsFromPEM([]byte(caPEM)) {
			tlsConfig.RootCAs = certPool
		}
	}

	return tlsConfig
}
