package servicebus

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.Sender      = (*Sender)(nil)
	_ ports.BatchSender = (*Sender)(nil)
)

const (
	asbHeaderPrefix  = "asb."
	asbSessionID     = "asb.session-id"
	asbCorrelationID = "asb.correlation-id"
	asbContentType   = "asb.content-type"
	asbReplyTo       = "asb.reply-to"
	asbTo            = "asb.to"
)

// Sender implements ports.Sender and ports.BatchSender for Azure Service Bus.
type Sender struct {
	cfg       SenderConfig
	client    asbSenderAPI
	asbClient *azservicebus.Client
}

// NewSender creates a Service Bus Sender. The underlying AMQP connection
// is established lazily on the first Send call unless cfg.Client is
// injected for testing.
func NewSender(cfg SenderConfig) (*Sender, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Sender{cfg: cfg, client: cfg.Client}, nil
}

// Send submits a single envelope to Service Bus.
func (s *Sender) Send(ctx context.Context, env *domain.Envelope) error {
	if err := s.ensureClient(ctx); err != nil {
		return err
	}

	sendCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	msg := s.buildMessage(env)
	if err := s.client.SendMessage(sendCtx, msg, nil); err != nil {
		return MapError(err)
	}
	return nil
}

// SendBatch sends multiple envelopes in batches of up to cfg.BatchSize.
// ASB batches are size-limited; when a message overflows the batch, the
// current batch is flushed and the oversized message is sent individually.
// Returns the number of successfully sent messages.
func (s *Sender) SendBatch(ctx context.Context, envs []*domain.Envelope) (int, error) {
	if err := s.ensureClient(ctx); err != nil {
		return 0, err
	}

	sendCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	var sent int

	for i := 0; i < len(envs); i += s.cfg.BatchSize {
		end := i + s.cfg.BatchSize
		if end > len(envs) {
			end = len(envs)
		}
		chunk := envs[i:end]

		msgBatch, err := s.client.NewMessageBatch(sendCtx, nil)
		if err != nil {
			return sent, MapError(err)
		}
		if msgBatch == nil {
			return sent, MapError(fmt.Errorf("servicebus sender: NewMessageBatch returned nil batch"))
		}

		for _, env := range chunk {
			sbMsg := s.buildMessage(env)

			addErr := msgBatch.AddMessage(sbMsg, nil)
			if addErr == nil {
				continue
			}
			if !errors.Is(addErr, azservicebus.ErrMessageTooLarge) {
				return sent, MapError(addErr)
			}

			if msgBatch.NumMessages() > 0 {
				if err := s.client.SendMessageBatch(sendCtx, msgBatch, nil); err != nil {
					return sent, MapError(err)
				}
				sent += int(msgBatch.NumMessages())
			}

			if err := s.sendSingle(sendCtx, env); err != nil {
				return sent, err
			}
			sent++

			// Start a fresh batch for remaining messages in this chunk.
			msgBatch, err = s.client.NewMessageBatch(sendCtx, nil)
			if err != nil {
				return sent, MapError(err)
			}
		}

		if msgBatch.NumMessages() > 0 {
			if err := s.client.SendMessageBatch(sendCtx, msgBatch, nil); err != nil {
				return sent, MapError(err)
			}
			sent += int(msgBatch.NumMessages())
		}
	}

	return sent, nil
}

// Close tears down the Service Bus sender and the underlying AMQP connection.
func (s *Sender) Close(ctx context.Context) error {
	var firstErr error
	if s.client != nil {
		firstErr = s.client.Close(ctx)
	}
	if s.asbClient != nil {
		if err := s.asbClient.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Sender) ensureClient(ctx context.Context) error {
	if s.client != nil {
		return nil
	}

	asbClient, err := buildClient(s.cfg.Connection)
	if err != nil {
		return fmt.Errorf("servicebus sender: %w", err)
	}

	entityName := s.cfg.QueueName
	if entityName == "" {
		entityName = s.cfg.TopicName
	}

	sender, err := asbClient.NewSender(entityName, nil)
	if err != nil {
		_ = asbClient.Close(ctx)
		return fmt.Errorf("servicebus sender: create sender for %q: %w", entityName, err)
	}

	s.client = sender
	s.asbClient = asbClient
	return nil
}

func (s *Sender) sendSingle(ctx context.Context, env *domain.Envelope) error {
	msg := s.buildMessage(env)
	if err := s.client.SendMessage(ctx, msg, nil); err != nil {
		return MapError(err)
	}
	return nil
}

func (s *Sender) buildMessage(env *domain.Envelope) *azservicebus.Message {
	msg := &azservicebus.Message{
		Body: env.Payload,
	}

	if env.Subject != "" {
		msg.Subject = &env.Subject
	}
	if env.ID != "" {
		msg.MessageID = &env.ID
	}
	if env.HasExpiry() {
		if ttl := env.RemainingTTL(); ttl > 0 {
			msg.TimeToLive = &ttl
		}
	}

	sessionID := s.cfg.DefaultSessionID
	var appProps map[string]any

	for k, v := range env.Headers {
		switch k {
		case asbSessionID:
			if sv, ok := v.(string); ok {
				sessionID = sv
			}
		case asbCorrelationID:
			if sv, ok := v.(string); ok {
				msg.CorrelationID = &sv
			}
		case asbContentType:
			if sv, ok := v.(string); ok {
				msg.ContentType = &sv
			}
		case asbReplyTo:
			if sv, ok := v.(string); ok {
				msg.ReplyTo = &sv
			}
		case asbTo:
			if sv, ok := v.(string); ok {
				msg.To = &sv
			}
		default:
			if strings.HasPrefix(k, asbHeaderPrefix) {
				continue
			}
			if appProps == nil {
				appProps = make(map[string]any, len(env.Headers))
			}
			appProps[k] = v
		}
	}

	if sessionID != "" {
		msg.SessionID = &sessionID
	}
	if len(appProps) > 0 {
		msg.ApplicationProperties = appProps
	}

	return msg
}
