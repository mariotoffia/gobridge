package servicebus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
)

// Target implements types.Target for Azure Service Bus message sending.
//
// # Delivery Guarantee Contract
//
// Target.Send() returns:
//   - nil: Message accepted by Service Bus. Service Bus now owns delivery.
//     Caller MUST NOT retry.
//   - error: Message NOT accepted. Classified as recoverable or permanent.
//
// Service Bus provides at-least-once delivery guarantee once it accepts a message.
type Target struct {
	id               string
	cfg              *TargetConfigImpl
	client           *azservicebus.Client
	sender           *azservicebus.Sender
	defaultSessionID string
	batchSize        int
	timeout          time.Duration
	running          atomic.Bool
	mu               sync.RWMutex
	closeOnce        sync.Once
	closeErr         error
}

// Ensure Target implements types.Target
var _ bridgeTypes.Target = (*Target)(nil)

// NewTarget creates a new Azure Service Bus target.
func NewTarget(cfg *TargetConfigImpl) (*Target, error) {
	if cfg == nil {
		return nil, errors.New("config is required")
	}
	if cfg.QueueName == "" && cfg.TopicName == "" {
		return nil, errors.New("either queueName or topicName is required")
	}
	if cfg.Connection.ConnectionString == "" && cfg.Connection.Namespace == "" {
		return nil, errors.New("either connectionString or namespace is required")
	}

	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 10
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &Target{
		id:               cfg.ID,
		cfg:              cfg,
		defaultSessionID: cfg.DefaultSessionID,
		batchSize:        batchSize,
		timeout:          timeout,
	}, nil
}

// GetID returns the unique identifier of the target.
func (t *Target) GetID() string {
	return t.id
}

// GetTransportType returns the transport type.
func (t *Target) GetTransportType() bridgeTypes.TransportType {
	return TransportType
}

// Capabilities returns the capabilities of this target.
func (t *Target) Capabilities() bridgeTypes.Capabilities {
	caps := bridgeTypes.Capabilities{}

	// Service Bus provides at-least-once delivery
	caps.AddType(bridgeTypes.CapabilityPublishAtLeastOnce)

	// Service Bus has native retry
	caps.AddType(bridgeTypes.CapabilityNativeRetry)

	// Service Bus has native DLQ
	caps.AddType(bridgeTypes.CapabilityDeadLetterQueue)

	// Service Bus supports scheduled/delayed delivery
	caps.AddType(bridgeTypes.CapabilityDelayedDelivery)

	return caps
}

// Connect initializes the Service Bus client and sender.
func (t *Target) Connect(ctx context.Context) error {
	if t.running.Load() {
		return nil // Already connected
	}

	// Create client
	client, err := t.createClient()
	if err != nil {
		return fmt.Errorf("failed to create Service Bus client: %w", err)
	}
	t.client = client

	// Create sender
	sender, err := t.createSender(client)
	if err != nil {
		_ = client.Close(ctx)
		return fmt.Errorf("failed to create sender: %w", MapError(err))
	}
	t.sender = sender

	t.running.Store(true)
	return nil
}

// createClient creates the Service Bus client.
func (t *Target) createClient() (*azservicebus.Client, error) {
	if t.cfg.Connection.ConnectionString != "" {
		return azservicebus.NewClientFromConnectionString(
			t.cfg.Connection.ConnectionString,
			nil,
		)
	}

	// Use Azure Identity
	var cred azcore.TokenCredential
	var err error

	if t.cfg.Connection.UseManagedIdentity {
		cred, err = azidentity.NewManagedIdentityCredential(nil)
	} else if t.cfg.Connection.ClientID != "" && t.cfg.Connection.ClientSecret != "" {
		cred, err = azidentity.NewClientSecretCredential(
			t.cfg.Connection.TenantID,
			t.cfg.Connection.ClientID,
			t.cfg.Connection.ClientSecret,
			nil,
		)
	} else {
		cred, err = azidentity.NewDefaultAzureCredential(nil)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to create credential: %w", err)
	}

	return azservicebus.NewClient(t.cfg.Connection.Namespace, cred, nil)
}

// createSender creates the message sender.
func (t *Target) createSender(client *azservicebus.Client) (*azservicebus.Sender, error) {
	if t.cfg.QueueName != "" {
		return client.NewSender(t.cfg.QueueName, nil)
	}
	return client.NewSender(t.cfg.TopicName, nil)
}

// Send sends a message to Service Bus.
//
// Returns nil when Service Bus accepts the message. Service Bus then owns delivery.
func (t *Target) Send(ctx context.Context, msg bridgeTypes.Message) error {
	// Ensure connected
	if !t.running.Load() {
		if err := t.Connect(ctx); err != nil {
			return err
		}
	}

	// Apply timeout
	sendCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	// Build Service Bus message
	sbMsg := &azservicebus.Message{
		Body: msg.Payload,
	}

	// Set subject from topic
	if msg.Topic != "" {
		sbMsg.Subject = &msg.Topic
	}

	// Set session ID
	sessionID := t.defaultSessionID
	if sid, ok := msg.Metadata["sessionId"].(string); ok && sid != "" {
		sessionID = sid
	}
	if sessionID != "" {
		sbMsg.SessionID = &sessionID
	}

	// Set TTL
	if msg.TTL > 0 {
		sbMsg.TimeToLive = &msg.TTL
	}

	// Set message ID
	if msgID, ok := msg.Metadata["messageId"].(string); ok {
		sbMsg.MessageID = &msgID
	}

	// Set correlation ID
	if corrID, ok := msg.Metadata["correlationId"].(string); ok {
		sbMsg.CorrelationID = &corrID
	}

	// Set content type
	if ct, ok := msg.Metadata["contentType"].(string); ok {
		sbMsg.ContentType = &ct
	}

	// Set reply to
	if replyTo, ok := msg.Metadata["replyTo"].(string); ok {
		sbMsg.ReplyTo = &replyTo
	}

	// Set application properties from metadata
	sbMsg.ApplicationProperties = make(map[string]any)
	for key, value := range msg.Metadata {
		// Skip well-known properties
		if key == "messageId" || key == "correlationId" || key == "sessionId" ||
			key == "contentType" || key == "replyTo" || key == "subject" || key == "to" {
			continue
		}
		sbMsg.ApplicationProperties[key] = value
	}

	// Check for scheduled delivery
	if scheduledTime, ok := msg.Metadata["scheduledEnqueueTime"].(time.Time); ok {
		_, err := t.sender.ScheduleMessages(sendCtx, []*azservicebus.Message{sbMsg}, scheduledTime, nil)
		if err != nil {
			return MapError(err)
		}
		return nil
	}

	// Send the message
	err := t.sender.SendMessage(sendCtx, sbMsg, nil)
	if err != nil {
		return MapError(err)
	}

	return nil
}

// SendBatch sends multiple messages in a batch.
func (t *Target) SendBatch(ctx context.Context, msgs []bridgeTypes.Message) (sent int, err error) {
	// Ensure connected
	if !t.running.Load() {
		if err := t.Connect(ctx); err != nil {
			return 0, err
		}
	}

	// Apply timeout
	sendCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	// Process in batches
	for i := 0; i < len(msgs); i += t.batchSize {
		end := i + t.batchSize
		if end > len(msgs) {
			end = len(msgs)
		}
		batch := msgs[i:end]

		// Create batch
		msgBatch, err := t.sender.NewMessageBatch(sendCtx, nil)
		if err != nil {
			return sent, MapError(err)
		}

		// Add messages to batch
		for _, msg := range batch {
			sbMsg := t.buildMessage(msg)
			err := msgBatch.AddMessage(sbMsg, nil)
			if err != nil {
				// Batch full or message too large
				if errors.Is(err, azservicebus.ErrMessageTooLarge) {
					// Send what we have and continue
					if msgBatch.NumMessages() > 0 {
						if err := t.sender.SendMessageBatch(sendCtx, msgBatch, nil); err != nil {
							return sent, MapError(err)
						}
						sent += int(msgBatch.NumMessages())
					}
					// Try to send this message individually
					if err := t.Send(ctx, msg); err != nil {
						return sent, err
					}
					sent++
					continue
				}
				return sent, MapError(err)
			}
		}

		// Send the batch
		if msgBatch.NumMessages() > 0 {
			if err := t.sender.SendMessageBatch(sendCtx, msgBatch, nil); err != nil {
				return sent, MapError(err)
			}
			sent += int(msgBatch.NumMessages())
		}
	}

	return sent, nil
}

// buildMessage builds a Service Bus message from a bridge message.
func (t *Target) buildMessage(msg bridgeTypes.Message) *azservicebus.Message {
	sbMsg := &azservicebus.Message{
		Body: msg.Payload,
	}

	if msg.Topic != "" {
		sbMsg.Subject = &msg.Topic
	}

	sessionID := t.defaultSessionID
	if sid, ok := msg.Metadata["sessionId"].(string); ok && sid != "" {
		sessionID = sid
	}
	if sessionID != "" {
		sbMsg.SessionID = &sessionID
	}

	if msg.TTL > 0 {
		sbMsg.TimeToLive = &msg.TTL
	}

	if msgID, ok := msg.Metadata["messageId"].(string); ok {
		sbMsg.MessageID = &msgID
	}

	if corrID, ok := msg.Metadata["correlationId"].(string); ok {
		sbMsg.CorrelationID = &corrID
	}

	if ct, ok := msg.Metadata["contentType"].(string); ok {
		sbMsg.ContentType = &ct
	}

	sbMsg.ApplicationProperties = make(map[string]any)
	for key, value := range msg.Metadata {
		if key == "messageId" || key == "correlationId" || key == "sessionId" ||
			key == "contentType" || key == "replyTo" || key == "subject" || key == "to" {
			continue
		}
		sbMsg.ApplicationProperties[key] = value
	}

	return sbMsg
}

// Close releases resources.
func (t *Target) Close() error {
	t.closeOnce.Do(func() {
		t.running.Store(false)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if t.sender != nil {
			_ = t.sender.Close(ctx)
		}
		if t.client != nil {
			t.closeErr = t.client.Close(ctx)
		}
	})

	return t.closeErr
}

