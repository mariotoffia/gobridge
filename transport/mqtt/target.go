package mqtt

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
	"github.com/mariotoffia/gobridge/bridge/types"
)

// Target implements types.Target for MQTT publishing.
//
// # Delivery Guarantee Contract
//
// Target.Send() returns:
//   - nil: Message accepted by MQTT broker.
//   - error: Message NOT accepted. Error is classified as recoverable or permanent.
//
// # QoS Behavior and Return Semantics
//
//   - QoS 0: Fire-and-forget. Returns nil on successful socket write.
//     WARNING: This does NOT confirm broker received the message!
//     Do NOT use with external retry managers - you cannot reliably decide when to ack.
//
//   - QoS 1: Returns nil when PUBACK received from broker.
//     The broker has accepted the message and owns delivery to subscribers.
//     Caller MUST NOT retry - the broker handles subscriber delivery.
//
//   - QoS 2: Returns nil when PUBCOMP received from broker.
//     Exactly-once delivery to broker confirmed.
//     Caller MUST NOT retry.
//
// # External Retry Integration (e.g., SQS-backed)
//
// Use QoS 1 or 2 with external retry managers:
//   - Send() blocks until PUBACK/PUBCOMP (broker confirmation)
//   - On nil return: Ack source message - broker owns delivery
//   - On error return: Check IsRecoverable to decide retry vs DLQ
type Target struct {
	id           string
	config       *TargetConfigImpl
	client       *autopaho.ConnectionManager
	defaultTopic string
	qos          byte
	retain       bool
	timeout      time.Duration
	running      atomic.Bool
	cancel       context.CancelFunc
	mu           sync.RWMutex
	closeOnce    sync.Once
	closeErr     error

	// sharedClient indicates the client is managed externally (by MQTTConnection)
	// and should NOT be closed when the Target is closed.
	sharedClient bool
}

// Ensure Target implements types.Target
var _ types.Target = (*Target)(nil)

// NewTarget creates a new MQTT target that manages its own connection.
// For shared connection mode, use NewTargetWithClient instead.
func NewTarget(config *TargetConfigImpl) (*Target, error) {
	if config == nil {
		return nil, errors.New("config is required")
	}
	if config.Connection.BrokerURL == "" {
		return nil, errors.New("broker URL is required")
	}

	qos := byte(config.QoS)
	if qos > 2 {
		qos = 1 // Default to QoS 1
	}

	timeout := config.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &Target{
		id:           config.ID,
		config:       config,
		defaultTopic: config.DefaultTopic,
		qos:          qos,
		retain:       config.Retain,
		timeout:      timeout,
		sharedClient: false,
	}, nil
}

// NewTargetWithClient creates a new MQTT target that uses a shared client.
// The client is managed externally (typically by MQTTConnection) and will NOT
// be closed when the Target is closed.
func NewTargetWithClient(config *TargetConfigImpl, client *autopaho.ConnectionManager) (*Target, error) {
	if config == nil {
		return nil, errors.New("config is required")
	}
	if client == nil {
		return nil, errors.New("client is required for shared client mode")
	}

	qos := byte(config.QoS)
	if qos > 2 {
		qos = 1 // Default to QoS 1
	}

	timeout := config.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &Target{
		id:           config.ID,
		config:       config,
		client:       client,
		defaultTopic: config.DefaultTopic,
		qos:          qos,
		retain:       config.Retain,
		timeout:      timeout,
		sharedClient: true,
		running:      atomic.Bool{},
	}, nil
}

// GetID returns the unique identifier of the target.
func (t *Target) GetID() string {
	return t.id
}

// GetTransportType returns the transport type.
func (t *Target) GetTransportType() types.TransportType {
	return TransportType
}

// Capabilities returns the capabilities of this target.
func (t *Target) Capabilities() types.Capabilities {
	caps := types.Capabilities{}

	switch t.qos {
	case 0:
		caps.AddType(types.CapabilityPublishAtMostOnce)
	case 1:
		caps.AddType(types.CapabilityPublishAtLeastOnce)
		caps.AddType(types.CapabilityNativeRetry)
	case 2:
		caps.AddType(types.CapabilityPublishExactOnce)
		caps.AddType(types.CapabilityNativeRetry)
	}

	return caps
}

// Connect establishes the connection to the MQTT broker.
// This is called automatically on first Send if not already connected.
// For shared client mode, this just marks the target as ready since the
// connection is managed by MQTTConnection.
func (t *Target) Connect(ctx context.Context) error {
	if t.running.Load() {
		return nil // Already connected
	}

	if t.sharedClient {
		// Shared mode - client is already connected, just mark as running
		t.running.Store(true)
		return nil
	}

	// Standalone mode - create our own connection
	return t.connectStandalone(ctx)
}

// connectStandalone creates and connects to MQTT broker with a dedicated connection.
func (t *Target) connectStandalone(ctx context.Context) error {
	// Parse broker URL
	serverURL, err := url.Parse(t.config.Connection.BrokerURL)
	if err != nil {
		return fmt.Errorf("invalid broker URL: %w", err)
	}

	// Create cancellable context
	ctx, t.cancel = context.WithCancel(ctx)

	// Build client configuration
	clientID := t.config.Connection.ClientID
	if clientID == "" {
		clientID = fmt.Sprintf("gobridge-target-%s-%d", t.id, time.Now().UnixNano())
	}

	cliCfg := autopaho.ClientConfig{
		ServerUrls: []*url.URL{serverURL},
		KeepAlive:  30,
		ClientConfig: paho.ClientConfig{
			ClientID: clientID,
		},
	}

	// Configure authentication
	if t.config.Connection.Username != "" {
		cliCfg.ConnectUsername = t.config.Connection.Username
		cliCfg.ConnectPassword = []byte(t.config.Connection.Password)
	}

	// Configure session
	cliCfg.CleanStartOnInitialConnection = t.config.Connection.CleanStart
	if t.config.Connection.SessionExpiryInterval > 0 {
		cliCfg.SessionExpiryInterval = t.config.Connection.SessionExpiryInterval
	}

	// Configure TLS
	if t.config.Connection.TLS != nil && t.config.Connection.TLS.Enable {
		tlsConfig, err := buildTLSConfig(t.config.Connection.TLS)
		if err != nil {
			return fmt.Errorf("failed to build TLS config: %w", err)
		}
		cliCfg.TlsCfg = tlsConfig
	}

	// Create connection manager
	client, err := autopaho.NewConnection(ctx, cliCfg)
	if err != nil {
		return fmt.Errorf("failed to create MQTT client: %w", err)
	}

	t.client = client
	t.running.Store(true)

	// Wait for initial connection
	connectTimeout := t.config.Connection.ConnectTimeout
	if connectTimeout == 0 {
		connectTimeout = 30 * time.Second
	}

	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	if err := client.AwaitConnection(connectCtx); err != nil {
		t.client = nil
		t.running.Store(false)
		return fmt.Errorf("failed to connect to MQTT broker: %w", MapError(err))
	}

	return nil
}

// Send sends a message to the MQTT broker.
//
// Return values:
//   - nil: Message ACCEPTED by broker. For QoS 1/2, PUBACK/PUBCOMP received.
//     Broker now owns delivery. Caller MUST NOT retry.
//   - error: Message NOT accepted. Classified as recoverable or permanent.
func (t *Target) Send(ctx context.Context, msg types.Message) error {
	// Ensure connected
	if !t.running.Load() {
		if err := t.Connect(ctx); err != nil {
			return err
		}
	}

	// Determine topic
	topic := msg.Topic
	if topic == "" {
		topic = t.defaultTopic
	}
	if topic == "" {
		return types.ErrInvalidTopic.WithMessage("no topic specified and no default topic configured")
	}

	// Determine QoS (message can override target default)
	qos := t.qos
	if msg.Qos != nil {
		qos = byte(msg.Qos.Level)
		if qos > 2 {
			qos = t.qos
		}
	}

	// Build publish packet
	publish := &paho.Publish{
		Topic:   topic,
		QoS:     qos,
		Retain:  t.retain,
		Payload: msg.Payload,
	}

	// Set MQTT v5 properties
	publish.Properties = &paho.PublishProperties{}

	// Message expiry
	if msg.TTL > 0 {
		expiry := uint32(msg.TTL.Seconds())
		publish.Properties.MessageExpiry = &expiry
	} else if t.config.MessageExpiry > 0 {
		publish.Properties.MessageExpiry = &t.config.MessageExpiry
	}

	// Extract properties from metadata
	if msg.Metadata != nil {
		if correlationData, ok := msg.Metadata["correlationData"].([]byte); ok {
			publish.Properties.CorrelationData = correlationData
		}
		if responseTopic, ok := msg.Metadata["responseTopic"].(string); ok {
			publish.Properties.ResponseTopic = responseTopic
		}
		if contentType, ok := msg.Metadata["contentType"].(string); ok {
			publish.Properties.ContentType = contentType
		}
		if userProps, ok := msg.Metadata["userProperties"].(map[string]string); ok {
			for k, v := range userProps {
				publish.Properties.User = append(publish.Properties.User, paho.UserProperty{Key: k, Value: v})
			}
		}
	}

	// Apply timeout
	publishCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	// Publish the message
	// For QoS 0: Returns immediately after writing to socket
	// For QoS 1: Blocks until PUBACK received
	// For QoS 2: Blocks until PUBCOMP received
	resp, err := t.client.Publish(publishCtx, publish)
	if err != nil {
		return MapError(err)
	}

	// Check response reason code for QoS 1/2
	if resp != nil && resp.ReasonCode != 0 && resp.ReasonCode != 0x10 {
		return MapPublishError(nil, resp.ReasonCode)
	}

	// Success - broker accepted the message
	return nil
}

// SendBatch sends multiple messages.
// MQTT doesn't support native batching, so this sends sequentially.
func (t *Target) SendBatch(ctx context.Context, msgs []types.Message) (sent int, err error) {
	for i, msg := range msgs {
		if err := t.Send(ctx, msg); err != nil {
			return i, err
		}
	}
	return len(msgs), nil
}

// Close disconnects from the broker and releases resources.
// For shared client mode, this does NOT close the underlying MQTT connection -
// that is managed by the MQTTConnection that created this target.
func (t *Target) Close() error {
	t.closeOnce.Do(func() {
		t.running.Store(false)

		if t.cancel != nil {
			t.cancel()
		}

		if !t.sharedClient && t.client != nil {
			// Standalone mode - disconnect and close client
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			t.closeErr = t.client.Disconnect(ctx)
		}
		// Shared mode - don't close the client, it's managed by MQTTConnection
	})

	return t.closeErr
}
