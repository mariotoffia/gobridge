package sqs

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
)

// Target implements types.Target for SQS message sending.
//
// # Delivery Guarantee Contract
//
// Target.Send() returns:
//   - nil: Message accepted by SQS. SQS now owns delivery.
//     Caller MUST NOT retry as SQS guarantees at-least-once delivery.
//   - error: Message NOT accepted. Classified as recoverable or permanent.
type Target struct {
	id             string
	cfg            *TargetConfigImpl
	client         *sqs.Client
	queueURL       string
	delaySeconds   int32
	batchSize      int
	timeout        time.Duration
	messageGroupID string
	running        atomic.Bool
	mu             sync.RWMutex
	closeOnce      sync.Once
	closeErr       error
}

// Ensure Target implements types.Target
var _ bridgeTypes.Target = (*Target)(nil)

// NewTarget creates a new SQS target.
func NewTarget(cfg *TargetConfigImpl) (*Target, error) {
	if cfg == nil {
		return nil, errors.New("config is required")
	}
	if cfg.QueueURL == "" && cfg.QueueName == "" {
		return nil, errors.New("either queueUrl or queueName is required")
	}

	delaySeconds := cfg.DelaySeconds
	if delaySeconds < 0 {
		delaySeconds = 0
	} else if delaySeconds > 900 {
		delaySeconds = 900
	}

	batchSize := cfg.BatchSize
	if batchSize <= 0 || batchSize > 10 {
		batchSize = 10
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &Target{
		id:             cfg.ID,
		cfg:            cfg,
		queueURL:       cfg.QueueURL,
		delaySeconds:   delaySeconds,
		batchSize:      batchSize,
		timeout:        timeout,
		messageGroupID: cfg.MessageGroupID,
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

	// SQS provides at-least-once delivery
	caps.AddType(bridgeTypes.CapabilityPublishAtLeastOnce)

	// SQS handles retries internally
	caps.AddType(bridgeTypes.CapabilityNativeRetry)

	// SQS supports delayed delivery
	caps.AddType(bridgeTypes.CapabilityDelayedDelivery)

	return caps
}

// Connect initializes the SQS client.
func (t *Target) Connect(ctx context.Context) error {
	if t.running.Load() {
		return nil // Already connected
	}

	// Build AWS config
	awsCfg, err := t.buildAWSConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to build AWS config: %w", err)
	}

	// Create SQS client
	t.client = sqs.NewFromConfig(awsCfg)

	// Resolve queue URL if needed
	if t.queueURL == "" {
		result, err := t.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{
			QueueName: aws.String(t.cfg.QueueName),
		})
		if err != nil {
			return fmt.Errorf("failed to get queue URL: %w", MapError(err))
		}
		t.queueURL = *result.QueueUrl
	}

	t.running.Store(true)
	return nil
}

// buildAWSConfig builds the AWS configuration.
func (t *Target) buildAWSConfig(ctx context.Context) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{}

	if t.cfg.Connection.Region != "" {
		opts = append(opts, config.WithRegion(t.cfg.Connection.Region))
	}

	if t.cfg.Connection.Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(t.cfg.Connection.Profile))
	}

	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return awsCfg, err
	}

	// Custom endpoint (for LocalStack)
	if t.cfg.Connection.Endpoint != "" {
		awsCfg.BaseEndpoint = aws.String(t.cfg.Connection.Endpoint)
	}

	return awsCfg, nil
}

// Send sends a message to the SQS queue.
//
// Returns nil when SQS accepts the message. SQS then owns delivery.
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

	// Build send input
	input := &sqs.SendMessageInput{
		QueueUrl:    aws.String(t.queueURL),
		MessageBody: aws.String(string(msg.Payload)),
	}

	// Set delay
	delay := t.delaySeconds
	if retryDelay, ok := msg.Metadata["retryDelay"].(int32); ok && retryDelay > 0 {
		delay = retryDelay
	}
	if delay > 0 {
		input.DelaySeconds = delay
	}

	// Set message group ID for FIFO queues
	groupID := t.messageGroupID
	if msgGroupID, ok := msg.Metadata["messageGroupId"].(string); ok && msgGroupID != "" {
		groupID = msgGroupID
	}
	if groupID != "" {
		input.MessageGroupId = aws.String(groupID)
		// FIFO queues require deduplication ID
		dedupID := generateDeduplicationID(msg)
		input.MessageDeduplicationId = aws.String(dedupID)
	}

	// Build message attributes from metadata
	input.MessageAttributes = buildMessageAttributes(msg)

	// Send the message
	_, err := t.client.SendMessage(sendCtx, input)
	if err != nil {
		return MapError(err)
	}

	return nil
}

// SendBatch sends multiple messages in a batch.
// Returns the number of successfully sent messages and an error if any failed.
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

	// Process in batches of up to 10
	for i := 0; i < len(msgs); i += t.batchSize {
		end := i + t.batchSize
		if end > len(msgs) {
			end = len(msgs)
		}
		batch := msgs[i:end]

		entries := make([]types.SendMessageBatchRequestEntry, 0, len(batch))
		for j, msg := range batch {
			entry := types.SendMessageBatchRequestEntry{
				Id:          aws.String(strconv.Itoa(j)),
				MessageBody: aws.String(string(msg.Payload)),
			}

			// Set delay
			if t.delaySeconds > 0 {
				entry.DelaySeconds = t.delaySeconds
			}

			// Set message group ID for FIFO queues
			if t.messageGroupID != "" {
				entry.MessageGroupId = aws.String(t.messageGroupID)
				entry.MessageDeduplicationId = aws.String(generateDeduplicationID(msg))
			}

			// Build message attributes
			entry.MessageAttributes = buildMessageAttributes(msg)

			entries = append(entries, entry)
		}

		result, err := t.client.SendMessageBatch(sendCtx, &sqs.SendMessageBatchInput{
			QueueUrl: aws.String(t.queueURL),
			Entries:  entries,
		})
		if err != nil {
			return sent, MapError(err)
		}

		sent += len(result.Successful)

		if len(result.Failed) > 0 {
			// Return error for first failure
			failed := result.Failed[0]
			return sent, bridgeTypes.ErrUnavailable.
				With("code", *failed.Code).
				With("message", *failed.Message).
				WithMessage(*failed.Message)
		}
	}

	return sent, nil
}

// Close releases resources.
func (t *Target) Close() error {
	t.closeOnce.Do(func() {
		t.running.Store(false)
		// SQS client doesn't need explicit cleanup
	})

	return t.closeErr
}

// buildMessageAttributes builds SQS message attributes from message metadata.
func buildMessageAttributes(msg bridgeTypes.Message) map[string]types.MessageAttributeValue {
	if len(msg.Metadata) == 0 {
		return nil
	}

	attrs := make(map[string]types.MessageAttributeValue)

	// Add topic as an attribute (useful for routing)
	if msg.Topic != "" {
		attrs["Topic"] = types.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(msg.Topic),
		}
	}

	// Add standard metadata
	for key, value := range msg.Metadata {
		// Skip internal metadata
		if key == "messageGroupId" || key == "retryDelay" {
			continue
		}

		switch v := value.(type) {
		case string:
			attrs[key] = types.MessageAttributeValue{
				DataType:    aws.String("String"),
				StringValue: aws.String(v),
			}
		case []byte:
			attrs[key] = types.MessageAttributeValue{
				DataType:    aws.String("Binary"),
				BinaryValue: v,
			}
		case int, int32, int64, float32, float64:
			attrs[key] = types.MessageAttributeValue{
				DataType:    aws.String("Number"),
				StringValue: aws.String(fmt.Sprintf("%v", v)),
			}
		}
	}

	if len(attrs) == 0 {
		return nil
	}

	return attrs
}

// generateDeduplicationID generates a deduplication ID for FIFO queues.
func generateDeduplicationID(msg bridgeTypes.Message) string {
	hash := md5.New()
	hash.Write(msg.Payload)
	hash.Write([]byte(msg.Topic))
	hash.Write([]byte(msg.CreatedAt.String()))
	return hex.EncodeToString(hash.Sum(nil))
}

