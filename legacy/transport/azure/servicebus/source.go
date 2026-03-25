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

// Source implements types.Source for Azure Service Bus message receiving.
type Source struct {
	id          string
	cfg         *SourceConfigImpl
	client      *azservicebus.Client
	receiver    *azservicebus.Receiver
	messages    chan *bridgeTypes.SourceMessage
	maxMessages int
	maxWaitTime time.Duration
	running     atomic.Bool
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	closeOnce   sync.Once
	closeErr    error
}

// Ensure Source implements types.Source
var _ bridgeTypes.Source = (*Source)(nil)

// NewSource creates a new Azure Service Bus source.
func NewSource(cfg *SourceConfigImpl) (*Source, error) {
	if cfg == nil {
		return nil, errors.New("config is required")
	}
	if cfg.QueueName == "" && (cfg.TopicName == "" || cfg.SubscriptionName == "") {
		return nil, errors.New("either queueName or (topicName + subscriptionName) is required")
	}
	if cfg.Connection.ConnectionString == "" && cfg.Connection.Namespace == "" {
		return nil, errors.New("either connectionString or namespace is required")
	}

	maxMessages := cfg.MaxMessages
	if maxMessages <= 0 {
		maxMessages = 10
	}

	maxWaitTime := cfg.MaxWaitTime
	if maxWaitTime <= 0 {
		maxWaitTime = 30 * time.Second
	}

	prefetch := cfg.Prefetch
	if prefetch <= 0 {
		prefetch = 100
	}

	return &Source{
		id:          cfg.ID,
		cfg:         cfg,
		maxMessages: maxMessages,
		maxWaitTime: maxWaitTime,
		messages:    make(chan *bridgeTypes.SourceMessage, prefetch),
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

	// Service Bus supports at-least-once delivery
	caps.AddType(bridgeTypes.CapabilityReceiveAtLeastOnce)

	// Service Bus supports Nack (abandon message)
	caps.AddType(bridgeTypes.CapabilityRedelivery)

	// Service Bus supports extending lock
	caps.AddType(bridgeTypes.CapabilityExtendTimeout)

	// Service Bus supports ordering via sessions
	if s.cfg.SessionID != "" {
		caps.AddType(bridgeTypes.CapabilityOrdering)
	}

	return caps
}

// Start begins receiving messages.
func (s *Source) Start(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("source already running")
	}

	// Create client
	client, err := s.createClient()
	if err != nil {
		s.running.Store(false)
		return fmt.Errorf("failed to create Service Bus client: %w", err)
	}
	s.client = client

	// Create receiver
	receiver, err := s.createReceiver(client)
	if err != nil {
		_ = client.Close(ctx)
		s.running.Store(false)
		return fmt.Errorf("failed to create receiver: %w", MapError(err))
	}
	s.receiver = receiver

	// Create cancellable context
	ctx, s.cancel = context.WithCancel(ctx)

	// Start polling goroutine
	s.wg.Add(1)
	go s.pollMessages(ctx)

	return nil
}

// createClient creates the Service Bus client.
func (s *Source) createClient() (*azservicebus.Client, error) {
	// Build client options with TLS config if provided
	opts := s.buildClientOptions()

	if s.cfg.Connection.ConnectionString != "" {
		return azservicebus.NewClientFromConnectionString(
			s.cfg.Connection.ConnectionString,
			opts,
		)
	}

	// Use Azure Identity
	var cred azcore.TokenCredential
	var err error

	if s.cfg.Connection.UseManagedIdentity {
		cred, err = azidentity.NewManagedIdentityCredential(nil)
	} else if s.cfg.Connection.ClientID != "" && s.cfg.Connection.ClientSecret != "" {
		cred, err = azidentity.NewClientSecretCredential(
			s.cfg.Connection.TenantID,
			s.cfg.Connection.ClientID,
			s.cfg.Connection.ClientSecret,
			nil,
		)
	} else {
		cred, err = azidentity.NewDefaultAzureCredential(nil)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to create credential: %w", err)
	}

	return azservicebus.NewClient(s.cfg.Connection.Namespace, cred, opts)
}

// buildClientOptions builds Azure SDK client options with TLS configuration.
func (s *Source) buildClientOptions() *azservicebus.ClientOptions {
	// Check if any TLS configuration is needed
	if s.cfg.Connection.TLSConfig == nil &&
		s.cfg.Connection.CaPEM == "" &&
		!s.cfg.Connection.InsecureSkipVerify {
		return nil
	}

	opts := &azservicebus.ClientOptions{}

	// Use provided TLSConfig directly if available
	if s.cfg.Connection.TLSConfig != nil {
		opts.TLSConfig = s.cfg.Connection.TLSConfig
		return opts
	}

	// Build TLS config from CaPEM and InsecureSkipVerify
	tlsConfig := buildTLSConfig(
		s.cfg.Connection.CaPEM,
		s.cfg.Connection.InsecureSkipVerify,
	)
	if tlsConfig != nil {
		opts.TLSConfig = tlsConfig
	}

	return opts
}

// createReceiver creates the message receiver.
func (s *Source) createReceiver(client *azservicebus.Client) (*azservicebus.Receiver, error) {
	opts := &azservicebus.ReceiverOptions{}

	// Set receive mode
	if s.cfg.ReceiveMode == "ReceiveAndDelete" {
		opts.ReceiveMode = azservicebus.ReceiveModeReceiveAndDelete
	} else {
		opts.ReceiveMode = azservicebus.ReceiveModePeekLock
	}

	// Set sub-queue
	switch s.cfg.SubQueue {
	case "deadletter":
		opts.SubQueue = azservicebus.SubQueueDeadLetter
	case "transferdeadletter":
		opts.SubQueue = azservicebus.SubQueueTransfer
	}

	// Create receiver based on queue or topic/subscription
	if s.cfg.QueueName != "" {
		return client.NewReceiverForQueue(s.cfg.QueueName, opts)
	}

	return client.NewReceiverForSubscription(
		s.cfg.TopicName,
		s.cfg.SubscriptionName,
		opts,
	)
}

// pollMessages continuously polls for messages.
func (s *Source) pollMessages(ctx context.Context) {
	defer s.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Receive messages
		messages, err := s.receiver.ReceiveMessages(ctx, s.maxMessages, nil)

		if err != nil {
			// Check if context was cancelled
			if ctx.Err() != nil {
				return
			}
			// Brief pause before retry on error
			time.Sleep(time.Second)
			continue
		}

		// Process each message
		for _, msg := range messages {
			srcMsg := s.convertMessage(ctx, msg)

			select {
			case <-ctx.Done():
				return
			case s.messages <- srcMsg:
			}
		}
	}
}

// convertMessage converts a Service Bus message to a bridge message.
func (s *Source) convertMessage(ctx context.Context, msg *azservicebus.ReceivedMessage) *bridgeTypes.SourceMessage {
	// Determine topic
	topic := s.cfg.QueueName
	if topic == "" {
		topic = s.cfg.TopicName
	}
	if msg.Subject != nil {
		topic = *msg.Subject
	}

	bridgeMsg := bridgeTypes.Message{
		CreatedAt: time.Now(),
		Topic:     topic,
		Payload:   msg.Body,
		Metadata:  make(map[string]any),
	}

	// Extract message properties
	if msg.MessageID != "" {
		bridgeMsg.Metadata["messageId"] = msg.MessageID
	}
	if msg.CorrelationID != nil {
		bridgeMsg.Metadata["correlationId"] = *msg.CorrelationID
	}
	if msg.SessionID != nil {
		bridgeMsg.Metadata["sessionId"] = *msg.SessionID
	}
	if msg.ContentType != nil {
		bridgeMsg.Metadata["contentType"] = *msg.ContentType
	}
	if msg.Subject != nil {
		bridgeMsg.Metadata["subject"] = *msg.Subject
	}
	if msg.To != nil {
		bridgeMsg.Metadata["to"] = *msg.To
	}
	if msg.ReplyTo != nil {
		bridgeMsg.Metadata["replyTo"] = *msg.ReplyTo
	}
	if msg.TimeToLive != nil {
		bridgeMsg.TTL = *msg.TimeToLive
	}

	// Extract application properties
	for key, value := range msg.ApplicationProperties {
		bridgeMsg.Metadata[key] = value
	}

	return &bridgeTypes.SourceMessage{
		Message: bridgeMsg,
		Ack: func() error {
			err := s.receiver.CompleteMessage(ctx, msg, nil)
			if err != nil {
				return MapError(err)
			}
			return nil
		},
		Nack: func(reason error) error {
			err := s.receiver.AbandonMessage(ctx, msg, nil)
			if err != nil {
				return MapError(err)
			}
			return nil
		},
		Extend: func(extendCtx context.Context) error {
			err := s.receiver.RenewMessageLock(extendCtx, msg, nil)
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

		// Close receiver and client
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if s.receiver != nil {
			_ = s.receiver.Close(ctx)
		}
		if s.client != nil {
			s.closeErr = s.client.Close(ctx)
		}

		// Close messages channel
		close(s.messages)
	})

	return s.closeErr
}
