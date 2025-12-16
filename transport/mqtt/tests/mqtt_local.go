// Package mqtttests provides test utilities and helpers for MQTT transport testing.
package mqtttests

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
	"github.com/mariotoffia/gobridge/tests/docker"
	"github.com/mariotoffia/gobridge/transport/mqtt"
)

// ============================================================================
// MQTT Mosquitto Helper
// ============================================================================

// MQTTLocalHelper manages Mosquitto container resources for testing.
type MQTTLocalHelper struct {
	t         *testing.T
	container *docker.MosquittoContainer
	brokerURL string

	// testClient is a dedicated MQTT client for test operations
	testClient *autopaho.ConnectionManager

	// receivedMessages stores messages received by the test client
	receivedMessages []ReceivedMessage
	messagesMu       sync.Mutex

	// subscriptions tracks active subscriptions for cleanup
	subscriptions []string
}

// ReceivedMessage represents a message received during testing.
type ReceivedMessage struct {
	Topic     string
	Payload   []byte
	QoS       byte
	Retain    bool
	Timestamp time.Time
}

// NewMQTTLocalHelper creates a helper with Mosquitto container.
// If container is nil, creates a new Mosquitto container.
func NewMQTTLocalHelper(t *testing.T, container *docker.MosquittoContainer) *MQTTLocalHelper {
	t.Helper()

	var err error
	if container == nil {
		ctx := context.Background()
		container, err = docker.DefaultMosquittoConfig().Start(ctx)
		if err != nil {
			t.Fatalf("failed to start Mosquitto: %v", err)
		}
	}

	brokerURL := container.BrokerURL()

	return &MQTTLocalHelper{
		t:                t,
		container:        container,
		brokerURL:        brokerURL,
		receivedMessages: make([]ReceivedMessage, 0),
		subscriptions:    make([]string, 0),
	}
}

// BrokerURL returns the Mosquitto broker URL (tcp://host:port).
func (h *MQTTLocalHelper) BrokerURL() string {
	return h.brokerURL
}

// Container returns the underlying Mosquitto container.
func (h *MQTTLocalHelper) Container() *docker.MosquittoContainer {
	return h.container
}

// ============================================================================
// Test Client Operations
// ============================================================================

// StartTestClient creates and starts a dedicated test client for publishing/subscribing.
func (h *MQTTLocalHelper) StartTestClient(ctx context.Context) error {
	h.t.Helper()

	if h.testClient != nil {
		return nil // Already started
	}

	clientID := fmt.Sprintf("test-helper-%d", time.Now().UnixNano())

	cfg, err := buildTestClientConfig(h.brokerURL, clientID, h.handleTestMessage)
	if err != nil {
		return fmt.Errorf("failed to build test client config: %w", err)
	}

	client, err := autopaho.NewConnection(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to create test client: %w", err)
	}

	// Wait for connection
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := client.AwaitConnection(connectCtx); err != nil {
		return fmt.Errorf("test client failed to connect: %w", err)
	}

	h.testClient = client
	return nil
}

// handleTestMessage processes messages received by the test client.
func (h *MQTTLocalHelper) handleTestMessage(m *paho.Publish) {
	h.messagesMu.Lock()
	defer h.messagesMu.Unlock()

	h.receivedMessages = append(h.receivedMessages, ReceivedMessage{
		Topic:     m.Topic,
		Payload:   m.Payload,
		QoS:       m.QoS,
		Retain:    m.Retain,
		Timestamp: time.Now(),
	})
}

// StopTestClient disconnects the test client.
func (h *MQTTLocalHelper) StopTestClient(ctx context.Context) error {
	if h.testClient == nil {
		return nil
	}

	err := h.testClient.Disconnect(ctx)
	h.testClient = nil
	return err
}

// ============================================================================
// Publishing Operations
// ============================================================================

// Publish sends a message using the test client.
func (h *MQTTLocalHelper) Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error {
	h.t.Helper()

	if h.testClient == nil {
		if err := h.StartTestClient(ctx); err != nil {
			return err
		}
	}

	_, err := h.testClient.Publish(ctx, &paho.Publish{
		Topic:   topic,
		QoS:     qos,
		Retain:  retain,
		Payload: payload,
	})
	return err
}

// PublishString is a convenience method that publishes a string message.
func (h *MQTTLocalHelper) PublishString(ctx context.Context, topic, message string, qos byte) error {
	return h.Publish(ctx, topic, []byte(message), qos, false)
}

// PublishRetained publishes a retained message.
func (h *MQTTLocalHelper) PublishRetained(ctx context.Context, topic string, payload []byte, qos byte) error {
	return h.Publish(ctx, topic, payload, qos, true)
}

// ClearRetained clears a retained message by publishing an empty payload with retain=true.
func (h *MQTTLocalHelper) ClearRetained(ctx context.Context, topic string) error {
	return h.Publish(ctx, topic, []byte{}, 0, true)
}

// ============================================================================
// Subscription Operations
// ============================================================================

// Subscribe subscribes to a topic using the test client.
func (h *MQTTLocalHelper) Subscribe(ctx context.Context, topic string, qos byte) error {
	h.t.Helper()

	if h.testClient == nil {
		if err := h.StartTestClient(ctx); err != nil {
			return err
		}
	}

	_, err := h.testClient.Subscribe(ctx, &paho.Subscribe{
		Subscriptions: []paho.SubscribeOptions{
			{Topic: topic, QoS: qos},
		},
	})
	if err == nil {
		h.subscriptions = append(h.subscriptions, topic)
	}
	return err
}

// Unsubscribe unsubscribes from a topic.
func (h *MQTTLocalHelper) Unsubscribe(ctx context.Context, topic string) error {
	h.t.Helper()

	if h.testClient == nil {
		return nil
	}

	_, err := h.testClient.Unsubscribe(ctx, &paho.Unsubscribe{
		Topics: []string{topic},
	})
	return err
}

// ============================================================================
// Message Retrieval
// ============================================================================

// GetReceivedMessages returns all messages received by the test client.
func (h *MQTTLocalHelper) GetReceivedMessages() []ReceivedMessage {
	h.messagesMu.Lock()
	defer h.messagesMu.Unlock()

	result := make([]ReceivedMessage, len(h.receivedMessages))
	copy(result, h.receivedMessages)
	return result
}

// ClearReceivedMessages clears the received messages buffer.
func (h *MQTTLocalHelper) ClearReceivedMessages() {
	h.messagesMu.Lock()
	defer h.messagesMu.Unlock()
	h.receivedMessages = h.receivedMessages[:0]
}

// WaitForMessages waits until the expected number of messages is received or timeout.
func (h *MQTTLocalHelper) WaitForMessages(ctx context.Context, count int) ([]ReceivedMessage, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return h.GetReceivedMessages(), ctx.Err()
		case <-ticker.C:
			msgs := h.GetReceivedMessages()
			if len(msgs) >= count {
				return msgs, nil
			}
		}
	}
}

// WaitForMessageOnTopic waits for a message on a specific topic.
func (h *MQTTLocalHelper) WaitForMessageOnTopic(ctx context.Context, topic string) (*ReceivedMessage, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			msgs := h.GetReceivedMessages()
			for i := range msgs {
				if msgs[i].Topic == topic {
					return &msgs[i], nil
				}
			}
		}
	}
}

// ============================================================================
// Config Builders
// ============================================================================

// NewSourceConfig creates a SourceConfigImpl for testing.
func (h *MQTTLocalHelper) NewSourceConfig(id string, topics []string, qos int) *mqtt.SourceConfigImpl {
	return &mqtt.SourceConfigImpl{
		ID: id,
		Connection: mqtt.ConnectionConfig{
			BrokerURL:  h.brokerURL,
			CleanStart: true,
		},
		Topics: topics,
		QoS:    qos,
	}
}

// NewTargetConfig creates a TargetConfigImpl for testing.
func (h *MQTTLocalHelper) NewTargetConfig(id string, defaultTopic string, qos int) *mqtt.TargetConfigImpl {
	return &mqtt.TargetConfigImpl{
		ID: id,
		Connection: mqtt.ConnectionConfig{
			BrokerURL:  h.brokerURL,
			CleanStart: true,
		},
		DefaultTopic: defaultTopic,
		QoS:          qos,
		Timeout:      10 * time.Second,
	}
}

// NewConnectionConfig creates an MQTTConnectionConfig for testing.
func (h *MQTTLocalHelper) NewConnectionConfig(id string) *mqtt.MQTTConnectionConfig {
	return &mqtt.MQTTConnectionConfig{
		ID: id,
		Connection: mqtt.ConnectionConfig{
			BrokerURL:  h.brokerURL,
			CleanStart: true,
		},
	}
}

// ============================================================================
// Cleanup
// ============================================================================

// Cleanup stops the test client and cleans up resources.
func (h *MQTTLocalHelper) Cleanup(ctx context.Context) {
	// Stop test client
	if h.testClient != nil {
		// Unsubscribe from all topics
		for _, topic := range h.subscriptions {
			h.Unsubscribe(ctx, topic)
		}
		h.StopTestClient(ctx)
	}
}

// ============================================================================
// Helper Functions
// ============================================================================

// buildTestClientConfig creates autopaho config for the test client.
func buildTestClientConfig(brokerURL, clientID string, handler func(*paho.Publish)) (autopaho.ClientConfig, error) {
	serverURL, err := parseURL(brokerURL)
	if err != nil {
		return autopaho.ClientConfig{}, err
	}

	return autopaho.ClientConfig{
		ServerUrls:                    serverURL,
		KeepAlive:                     30,
		CleanStartOnInitialConnection: true,
		ClientConfig: paho.ClientConfig{
			ClientID: clientID,
			Router:   paho.NewSingleHandlerRouter(handler),
		},
	}, nil
}

// parseURL parses broker URL into []*url.URL.
func parseURL(brokerURL string) ([]*url.URL, error) {
	u, err := url.Parse(brokerURL)
	if err != nil {
		return nil, err
	}
	return []*url.URL{u}, nil
}

// ============================================================================
// Unique Name Generation
// ============================================================================

// UniqueTopic generates a unique topic name for testing.
func UniqueTopic(prefix string) string {
	return fmt.Sprintf("%s/%d", prefix, time.Now().UnixNano())
}

// UniqueClientID generates a unique client ID for testing.
func UniqueClientID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}
