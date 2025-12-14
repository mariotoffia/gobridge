package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
)

// Source implements types.Source for SQS message receiving.
type Source struct {
	id                string
	cfg               *SourceConfigImpl
	client            *sqs.Client
	queueURL          string
	messages          chan *bridgeTypes.SourceMessage
	maxMessages       int32
	waitTimeSeconds   int32
	visibilityTimeout int32
	running           atomic.Bool
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	closeOnce         sync.Once
	closeErr          error
}

// Ensure Source implements types.Source
var _ bridgeTypes.Source = (*Source)(nil)

// NewSource creates a new SQS source.
func NewSource(cfg *SourceConfigImpl) (*Source, error) {
	if cfg == nil {
		return nil, errors.New("config is required")
	}
	if cfg.QueueURL == "" && cfg.QueueName == "" {
		return nil, errors.New("either queueUrl or queueName is required")
	}

	maxMessages := cfg.MaxMessages
	if maxMessages <= 0 || maxMessages > 10 {
		maxMessages = 10
	}

	waitTimeSeconds := cfg.WaitTimeSeconds
	if waitTimeSeconds < 0 {
		waitTimeSeconds = 0
	} else if waitTimeSeconds > 20 {
		waitTimeSeconds = 20
	}

	visibilityTimeout := cfg.VisibilityTimeout
	if visibilityTimeout <= 0 {
		visibilityTimeout = 30 // Default 30 seconds
	}

	prefetch := cfg.Prefetch
	if prefetch <= 0 {
		prefetch = 100
	}

	return &Source{
		id:                cfg.ID,
		cfg:               cfg,
		queueURL:          cfg.QueueURL,
		maxMessages:       maxMessages,
		waitTimeSeconds:   waitTimeSeconds,
		visibilityTimeout: visibilityTimeout,
		messages:          make(chan *bridgeTypes.SourceMessage, prefetch),
	}, nil
}

// GetID returns the unique identifier of the source.
func (s *Source) GetID() string {
	return s.id
}

// GetTransportType returns the transport type.
func (s *Source) GetTransportType() bridgeTypes.TransportType {
	return TransportType
}

// Capabilities returns the capabilities of this source.
func (s *Source) Capabilities() bridgeTypes.Capabilities {
	caps := bridgeTypes.Capabilities{}

	// SQS supports at-least-once delivery
	caps.AddType(bridgeTypes.CapabilityReceiveAtLeastOnce)

	// SQS supports Nack (message becomes visible again after timeout)
	caps.AddType(bridgeTypes.CapabilityRedelivery)

	// SQS supports extending visibility timeout
	caps.AddType(bridgeTypes.CapabilityExtendTimeout)

	return caps
}

// Start begins receiving messages.
func (s *Source) Start(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("source already running")
	}

	// Build AWS config
	awsCfg, err := s.buildAWSConfig(ctx)
	if err != nil {
		s.running.Store(false)
		return fmt.Errorf("failed to build AWS config: %w", err)
	}

	// Create SQS client
	s.client = sqs.NewFromConfig(awsCfg)

	// Resolve queue URL if needed
	if s.queueURL == "" {
		result, err := s.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{
			QueueName: aws.String(s.cfg.QueueName),
		})
		if err != nil {
			s.running.Store(false)
			return fmt.Errorf("failed to get queue URL: %w", MapError(err))
		}
		s.queueURL = *result.QueueUrl
	}

	// Create cancellable context
	ctx, s.cancel = context.WithCancel(ctx)

	// Start polling goroutine
	s.wg.Add(1)
	go s.pollMessages(ctx)

	return nil
}

// buildAWSConfig builds the AWS configuration.
func (s *Source) buildAWSConfig(ctx context.Context) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{}

	if s.cfg.Connection.Region != "" {
		opts = append(opts, config.WithRegion(s.cfg.Connection.Region))
	}

	if s.cfg.Connection.Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(s.cfg.Connection.Profile))
	}

	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return awsCfg, err
	}

	// Custom endpoint (for LocalStack)
	if s.cfg.Connection.Endpoint != "" {
		awsCfg.BaseEndpoint = aws.String(s.cfg.Connection.Endpoint)
	}

	return awsCfg, nil
}

// pollMessages continuously polls for messages.
func (s *Source) pollMessages(ctx context.Context) {
	defer s.wg.Done()

	attributeNames := s.cfg.MessageAttributeNames
	if len(attributeNames) == 0 {
		attributeNames = []string{"All"}
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Receive messages
		output, err := s.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:              aws.String(s.queueURL),
			MaxNumberOfMessages:   s.maxMessages,
			WaitTimeSeconds:       s.waitTimeSeconds,
			VisibilityTimeout:     s.visibilityTimeout,
			MessageAttributeNames: attributeNames,
			AttributeNames:        []types.QueueAttributeName{types.QueueAttributeNameAll},
		})

		if err != nil {
			// Check if context was cancelled
			if ctx.Err() != nil {
				return
			}
			// Log error and retry after a short delay
			time.Sleep(time.Second)
			continue
		}

		// Process each message
		for _, msg := range output.Messages {
			srcMsg := s.convertMessage(ctx, msg)

			select {
			case <-ctx.Done():
				return
			case s.messages <- srcMsg:
			}
		}
	}
}

// convertMessage converts an SQS message to a bridge message.
func (s *Source) convertMessage(ctx context.Context, msg types.Message) *bridgeTypes.SourceMessage {
	receiptHandle := *msg.ReceiptHandle

	bridgeMsg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Topic:     s.queueURL, // Use queue URL as topic
		Payload:   []byte(*msg.Body),
		Metadata:  make(map[string]any),
	}

	// Extract message ID
	if msg.MessageId != nil {
		bridgeMsg.Metadata["messageId"] = *msg.MessageId
	}

	// Extract message attributes
	for key, attr := range msg.MessageAttributes {
		if attr.StringValue != nil {
			bridgeMsg.Metadata[key] = *attr.StringValue
		} else if attr.BinaryValue != nil {
			bridgeMsg.Metadata[key] = attr.BinaryValue
		}
	}

	// Extract system attributes
	for key, value := range msg.Attributes {
		bridgeMsg.Metadata["sqs:"+string(key)] = value
	}

	// Try to extract topic from message body if it looks like an SNS notification
	if msg.Body != nil {
		var snsNotification struct {
			TopicArn string `json:"TopicArn"`
			Subject  string `json:"Subject"`
			Message  string `json:"Message"`
		}
		if err := json.Unmarshal([]byte(*msg.Body), &snsNotification); err == nil && snsNotification.TopicArn != "" {
			bridgeMsg.Topic = snsNotification.TopicArn
			if snsNotification.Subject != "" {
				bridgeMsg.Metadata["subject"] = snsNotification.Subject
			}
			// Unwrap SNS message
			bridgeMsg.Payload = []byte(snsNotification.Message)
		}
	}

	return &bridgeTypes.SourceMessage{
		Message: bridgeMsg,
		Ack: func() error {
			_, err := s.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl:      aws.String(s.queueURL),
				ReceiptHandle: aws.String(receiptHandle),
			})
			if err != nil {
				return MapError(err)
			}
			return nil
		},
		Nack: func(reason error) error {
			// Make message visible immediately by setting visibility to 0
			_, err := s.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
				QueueUrl:          aws.String(s.queueURL),
				ReceiptHandle:     aws.String(receiptHandle),
				VisibilityTimeout: 0,
			})
			if err != nil {
				return MapError(err)
			}
			return nil
		},
		Extend: func(extendCtx context.Context) error {
			_, err := s.client.ChangeMessageVisibility(extendCtx, &sqs.ChangeMessageVisibilityInput{
				QueueUrl:          aws.String(s.queueURL),
				ReceiptHandle:     aws.String(receiptHandle),
				VisibilityTimeout: s.visibilityTimeout,
			})
			if err != nil {
				return MapError(err)
			}
			return nil
		},
	}
}

// Messages returns the channel that receives messages.
func (s *Source) Messages() <-chan *bridgeTypes.SourceMessage {
	return s.messages
}

// Close stops the source and releases resources.
func (s *Source) Close() error {
	s.closeOnce.Do(func() {
		s.running.Store(false)

		if s.cancel != nil {
			s.cancel()
		}

		// Wait for polling to stop
		s.wg.Wait()

		// Close messages channel
		close(s.messages)
	})

	return s.closeErr
}

