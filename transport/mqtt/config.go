// Package mqtt provides MQTT v5 transport implementation using paho.golang.
//
// # MQTT QoS and Delivery Guarantees
//
// This implementation follows a clear responsibility handoff model:
//
//   - QoS 0: Fire-and-forget. NO acknowledgment. Send() returns nil on socket write,
//     but this does NOT guarantee broker received the message. NOT RECOMMENDED for
//     reliable delivery scenarios.
//
//   - QoS 1: At-least-once. PUBACK confirms broker received message. Send() blocks until
//     PUBACK is received. When Send() returns nil, the broker owns delivery to subscribers.
//     Paho handles protocol-level retransmission if PUBACK is not received.
//
//   - QoS 2: Exactly-once. Full handshake (PUBREC/PUBREL/PUBCOMP). Send() blocks until
//     PUBCOMP is received. Highest reliability but also highest latency.
//
// # External Retry Integration (e.g., SQS-backed)
//
// For clustered/durable retry scenarios, use QoS 1 or QoS 2:
//
//  1. Pipeline receives message from SQS source
//  2. Target.Send() publishes with QoS 1/2
//  3. If Send() returns nil (PUBACK/PUBCOMP received):
//     - Broker has accepted the message
//     - Ack the SQS message - broker now owns subscriber delivery
//     - Do NOT retry via SQS
//  4. If Send() returns error:
//     - Check error.IsRecoverable
//     - If recoverable: Nack SQS message for retry
//     - If permanent: Send to DLQ
//
// IMPORTANT: QoS 0 should NOT be used with external retry managers because there's
// no confirmation that the broker received the message. You cannot reliably decide
// when to ack the source message.
package mqtt

import (
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

const (
	// TransportType is the transport type identifier for MQTT.
	TransportType types.TransportType = "MQTT"
)

// ConnectionConfig holds common connection settings for MQTT.
type ConnectionConfig struct {
	// BrokerURL is the MQTT broker URL (e.g., "tcp://localhost:1883").
	// Deprecated: Use BrokerURLs for multi-broker support.
	BrokerURL string `json:"brokerUrl"`

	// BrokerURLs is the list of MQTT broker URLs for high-availability.
	// The client will try each URL in order until one connects.
	BrokerURLs []string `json:"brokerUrls,omitempty"`

	// ClientID is the MQTT client identifier.
	// If empty, a unique ID is generated.
	ClientID string `json:"clientId,omitempty"`

	// Username for authentication (optional).
	Username string `json:"username,omitempty"`

	// Password for authentication (optional).
	Password string `json:"password,omitempty"`

	// Credentials for authentication (optional).
	// Supports both inline credentials and URI references.
	Credentials *types.Credentials `json:"credentials,omitempty"`

	// CleanStart indicates whether to start a clean session.
	// If true, any previous session state is discarded.
	CleanStart bool `json:"cleanStart"`

	// SessionExpiryInterval is how long the broker should keep the session
	// after the client disconnects (in seconds). 0 means session ends on disconnect.
	SessionExpiryInterval uint32 `json:"sessionExpiryInterval,omitempty"`

	// KeepAlive is the keep-alive interval in seconds.
	KeepAlive uint16 `json:"keepAlive,omitempty"`

	// ConnectTimeout is the timeout for establishing a connection.
	ConnectTimeout time.Duration `json:"connectTimeout,omitempty"`

	// TLS configures TLS/SSL for the connection.
	TLS *TLSConfig `json:"tls,omitempty"`
}

// RequiresReconnect compares with another config and returns true if changes require reconnect.
func (c *ConnectionConfig) RequiresReconnect(other *MQTTConnectionSettings) bool {
	// BrokerURLs changes require reconnect
	if !slicesEqual(c.getBrokerURLs(), other.GetBrokerURLs()) {
		return true
	}

	// ClientID changes require reconnect
	if c.ClientID != other.clientID {
		return true
	}

	// Credentials changes require reconnect
	if !credentialsEqual(c.Credentials, other.credentials) {
		return true
	}

	// TLS changes require reconnect
	if c.TLS != nil && other.tls != nil {
		if c.TLS.Enable != other.tls.Enable ||
			c.TLS.InsecureSkipVerify != other.tls.InsecureSkipVerify ||
			c.TLS.CACertFile != other.tls.CACertFile ||
			c.TLS.CertFile != other.tls.CertFile ||
			c.TLS.KeyFile != other.tls.KeyFile {
			return true
		}
	} else if (c.TLS != nil) != (other.tls != nil) {
		return true
	}

	return false
}

// getBrokerURLs returns the broker URLs, handling both old and new fields.
func (c *ConnectionConfig) getBrokerURLs() []string {
	if len(c.BrokerURLs) > 0 {
		return c.BrokerURLs
	}
	if c.BrokerURL != "" {
		return []string{c.BrokerURL}
	}
	return nil
}

// slicesEqual compares two string slices for equality.
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// MQTTConnectionSettings implements types.ConnectionSettingsConfig for MQTT.
type MQTTConnectionSettings struct {
	id          string
	brokerURLs  []string
	clientID    string
	credentials *types.Credentials
	keepAlive   *DefaultKeepAliveConfig
	tls         *TLSConfig
}

// DefaultKeepAliveConfig provides keep-alive settings for MQTT.
type DefaultKeepAliveConfig struct {
	interval time.Duration
	timeout  time.Duration
}

func (c *DefaultKeepAliveConfig) GetInterval() time.Duration {
	return c.interval
}

func (c *DefaultKeepAliveConfig) GetTimeout() time.Duration {
	return c.timeout
}

// Ensure MQTTConnectionSettings implements types.ConnectionSettingsConfig
var _ types.ConnectionSettingsConfig = (*MQTTConnectionSettings)(nil)

// NewMQTTConnectionSettings creates new connection settings.
func NewMQTTConnectionSettings(id string) *MQTTConnectionSettings {
	return &MQTTConnectionSettings{id: id}
}

// WithBrokerURLs sets the broker URLs.
func (s *MQTTConnectionSettings) WithBrokerURLs(urls []string) *MQTTConnectionSettings {
	s.brokerURLs = urls
	return s
}

// WithClientID sets the client ID.
func (s *MQTTConnectionSettings) WithClientID(clientID string) *MQTTConnectionSettings {
	s.clientID = clientID
	return s
}

// WithCredentials sets the credentials.
// Use this for both inline credentials (UsernamePasswordCredentials, TlsCredentials)
// and URI-based credentials (e.g., "pms://tenant/app/mqtt-creds").
//
// Use the bridge/credentials/builders to build `*types.Credentials` objects.
//
// Example:
//
//	s.WithCredentials(credentials.NewCredentialsBuilder().
//		WithUsernamePassword("username", "password").
//		Build())
func (s *MQTTConnectionSettings) WithCredentials(creds *types.Credentials) *MQTTConnectionSettings {
	s.credentials = creds
	return s
}

// WithTLS sets TLS configuration.
func (s *MQTTConnectionSettings) WithTLS(tls *TLSConfig) *MQTTConnectionSettings {
	s.tls = tls
	return s
}

// WithKeepAlive sets keep-alive settings.
func (s *MQTTConnectionSettings) WithKeepAlive(interval, timeout time.Duration) *MQTTConnectionSettings {
	s.keepAlive = &DefaultKeepAliveConfig{interval: interval, timeout: timeout}
	return s
}

func (s *MQTTConnectionSettings) GetID() string {
	return s.id
}

func (s *MQTTConnectionSettings) GetTransportType() types.TransportType {
	return TransportType
}

func (s *MQTTConnectionSettings) GetBrokerURLs() []string {
	return s.brokerURLs
}

func (s *MQTTConnectionSettings) GetCredentials() *types.Credentials {
	return s.credentials
}

func (s *MQTTConnectionSettings) GetKeepAlive() types.KeepAliveConfig {
	if s.keepAlive == nil {
		return nil
	}
	return s.keepAlive
}

func (s *MQTTConnectionSettings) RequiresReconnect(other types.ConnectionSettingsConfig) bool {
	otherMQTT, ok := other.(*MQTTConnectionSettings)
	if !ok {
		return true // Different type, definitely needs reconnect
	}

	// BrokerURLs changes require reconnect
	if !slicesEqual(s.brokerURLs, otherMQTT.brokerURLs) {
		return true
	}

	// ClientID changes require reconnect
	if s.clientID != otherMQTT.clientID {
		return true
	}

	// Credentials changes require reconnect
	if !credentialsEqual(s.credentials, otherMQTT.credentials) {
		return true
	}

	// TLS changes require reconnect
	if s.tls != nil && otherMQTT.tls != nil {
		if s.tls.Enable != otherMQTT.tls.Enable ||
			s.tls.InsecureSkipVerify != otherMQTT.tls.InsecureSkipVerify {
			return true
		}
	} else if (s.tls != nil) != (otherMQTT.tls != nil) {
		return true
	}

	return false
}

// credentialsEqual compares two Credentials for equality.
// Returns true if both are nil or if they have the same content.
func credentialsEqual(a, b *types.Credentials) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if len(a.Type) != len(b.Type) {
		return false
	}
	for i := range a.Type {
		if a.Type[i] != b.Type[i] {
			return false
		}
	}
	// Note: Deep comparison of Credentials is complex.
	// For now, if types match and lengths match, assume equal.
	// A more thorough comparison would serialize and compare.
	return len(a.Credentials) == len(b.Credentials)
}

// TLSConfig holds TLS configuration.
type TLSConfig struct {
	// Enable enables TLS.
	Enable bool `json:"enable"`

	// InsecureSkipVerify skips certificate verification.
	// Should only be used for testing.
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`

	// CACertFile is the path to the CA certificate file.
	CACertFile string `json:"caCertFile,omitempty"`

	// CertFile is the path to the client certificate file.
	CertFile string `json:"certFile,omitempty"`

	// KeyFile is the path to the client key file.
	KeyFile string `json:"keyFile,omitempty"`
}

// SourceConfigImpl implements types.SourceConfig for MQTT subscriptions.
type SourceConfigImpl struct {
	// ID is the unique identifier for this source.
	ID string `json:"id"`

	// Connection holds the MQTT connection settings.
	Connection ConnectionConfig `json:"connection"`

	// Topics is the list of topics to subscribe to.
	// Supports MQTT wildcards (+ for single level, # for multi-level).
	Topics []string `json:"topics"`

	// QoS is the QoS level for subscriptions.
	QoS int `json:"qos"`

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
	return &types.QosLevel{Level: c.QoS}
}

func (c *SourceConfigImpl) GetPrefetch() int {
	return 0 // MQTT doesn't support prefetch in the same way as queues
}

func (c *SourceConfigImpl) GetResources() []types.Tag {
	return c.Resources
}

func (c *SourceConfigImpl) AllowMultipleResourceMatches() bool {
	return c.AllowMultiple
}

// TargetConfigImpl implements types.TargetConfig for MQTT publishing.
type TargetConfigImpl struct {
	// ID is the unique identifier for this target.
	ID string `json:"id"`

	// Connection holds the MQTT connection settings.
	Connection ConnectionConfig `json:"connection"`

	// DefaultTopic is the default topic to publish to.
	// Individual messages may override this.
	DefaultTopic string `json:"defaultTopic,omitempty"`

	// QoS is the default QoS level for publishing.
	QoS int `json:"qos"`

	// Retain indicates whether messages should be retained by the broker.
	Retain bool `json:"retain"`

	// MessageExpiry is the message expiry interval in seconds (MQTT v5).
	// 0 means no expiry.
	MessageExpiry uint32 `json:"messageExpiry,omitempty"`

	// Timeout is the timeout for publish operations.
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
	return &types.QosLevel{Level: c.QoS}
}

func (c *TargetConfigImpl) GetBatchSize() int {
	return 0 // MQTT doesn't support batching
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
