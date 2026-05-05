package servicebus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/logging"
)

// headersToMessage maps envelope headers back to a fresh
// *azservicebus.Message, extracting well-known asb.* keys into typed
// SDK properties and routing the rest into ApplicationProperties.
// Reserved x-bridge.* headers and asb.* well-known keys are filtered
// out of ApplicationProperties.
func headersToMessage(headers map[string]any) *azservicebus.Message {
	msg := &azservicebus.Message{}

	if v, ok := headers[asbHeaderMessageID]; ok {
		if s, ok := v.(string); ok {
			msg.MessageID = &s
		}
	}
	if v, ok := headers[asbHeaderCorrelationID]; ok {
		if s, ok := v.(string); ok {
			msg.CorrelationID = &s
		}
	}
	if v, ok := headers[asbHeaderSessionID]; ok {
		if s, ok := v.(string); ok {
			msg.SessionID = &s
		}
	}
	if v, ok := headers[asbHeaderContentType]; ok {
		if s, ok := v.(string); ok {
			msg.ContentType = &s
		}
	}
	if v, ok := headers[asbHeaderSubject]; ok {
		if s, ok := v.(string); ok {
			msg.Subject = &s
		}
	}
	if v, ok := headers[asbHeaderTo]; ok {
		if s, ok := v.(string); ok {
			msg.To = &s
		}
	}
	if v, ok := headers[asbHeaderReplyTo]; ok {
		if s, ok := v.(string); ok {
			msg.ReplyTo = &s
		}
	}
	if v, ok := headers[asbHeaderTTL]; ok {
		var d time.Duration
		switch val := v.(type) {
		case time.Duration:
			d = val
		case string:
			if parsed, err := time.ParseDuration(val); err == nil {
				d = parsed
			}
		case int:
			d = time.Duration(val) * time.Second
		case int64:
			d = time.Duration(val) * time.Second
		case float64:
			d = time.Duration(val * float64(time.Second))
		}
		if d > 0 {
			msg.TimeToLive = &d
		}
	}

	var appProps map[string]any
	for k, v := range headers {
		if asbWellKnownHeaders[k] || strings.HasPrefix(k, asbHeaderPrefix) {
			continue
		}
		if domain.IsReservedHeader(k) {
			continue
		}
		if appProps == nil {
			appProps = make(map[string]any, len(headers))
		}
		appProps[k] = v
	}
	msg.ApplicationProperties = appProps

	return msg
}

// envelopeToMessage builds the *azservicebus.Message that represents
// env on the wire. Headers map onto SDK system properties and
// ApplicationProperties; envelope ID/Subject win over header-supplied
// equivalents; expiry is converted to TimeToLive when in the future.
//
// defaultSessionID is used when env has no asb.session-id header and
// the sender is configured with a default session.
func envelopeToMessage(env *domain.Envelope, defaultSessionID string, clk clock.Clock) *azservicebus.Message {
	if clk == nil {
		clk = clock.System
	}
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
		if ttl := env.RemainingTTL(clk); ttl > 0 {
			msg.TimeToLive = &ttl
		}
	}

	sessionID := defaultSessionID
	var appProps map[string]any

	for k, v := range env.Headers {
		switch k {
		case asbHeaderSessionID:
			if sv, ok := v.(string); ok {
				sessionID = sv
			}
		case asbHeaderCorrelationID:
			if sv, ok := v.(string); ok {
				msg.CorrelationID = &sv
			}
		case asbHeaderContentType:
			if sv, ok := v.(string); ok {
				msg.ContentType = &sv
			}
		case asbHeaderReplyTo:
			if sv, ok := v.(string); ok {
				msg.ReplyTo = &sv
			}
		case asbHeaderTo:
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

// buildRetryMessage constructs a fresh *azservicebus.Message that
// mirrors a previously-received message, suitable for re-enqueueing
// via ScheduleMessages on the retry path.
func buildRetryMessage(received *azservicebus.ReceivedMessage) *azservicebus.Message {
	out := &azservicebus.Message{
		Body:    received.Body,
		Subject: received.Subject,
	}
	if received.MessageID != "" {
		out.MessageID = &received.MessageID
	}
	if received.SessionID != nil {
		out.SessionID = received.SessionID
	}
	if received.ContentType != nil {
		out.ContentType = received.ContentType
	}
	if received.CorrelationID != nil {
		out.CorrelationID = received.CorrelationID
	}
	if len(received.ApplicationProperties) > 0 {
		out.ApplicationProperties = received.ApplicationProperties
	}
	if received.ReplyTo != nil {
		out.ReplyTo = received.ReplyTo
	}
	if received.To != nil {
		out.To = received.To
	}
	if received.TimeToLive != nil {
		out.TimeToLive = received.TimeToLive
	}
	return out
}

// sendOne builds the SDK message from env and dispatches a single
// SendMessage call against the asbSenderAPI seam. Errors are
// classified to *domain.BridgeError before they cross the seam.
func sendOne(ctx context.Context, client asbSenderAPI, env *domain.Envelope, defaultSessionID string, clk clock.Clock) error {
	msg := envelopeToMessage(env, defaultSessionID, clk)
	if err := client.SendMessage(ctx, msg, nil); err != nil {
		return MapError(err)
	}
	return nil
}

// sendChunk packages chunk into one or more SDK MessageBatch values,
// flushing as needed when AddMessage reports the batch is full
// (azservicebus.ErrMessageTooLarge). Oversized messages that don't fit
// in any batch are sent individually via SendMessage. Returns the
// number of messages successfully accepted by the broker.
//
// All SDK-typed plumbing (MessageBatch, ErrMessageTooLarge) stays
// inside this helper so the Sender stays SDK-free.
func sendChunk(
	ctx context.Context,
	client asbSenderAPI,
	chunk []*domain.Envelope,
	defaultSessionID string,
	clk clock.Clock,
	logger *slog.Logger,
	entity string,
) (int, error) {
	var sent int

	msgBatch, err := client.NewMessageBatch(ctx, nil)
	if err != nil {
		return sent, MapError(err)
	}
	if msgBatch == nil {
		return sent, MapError(fmt.Errorf("servicebus sender: NewMessageBatch returned nil batch"))
	}

	for _, env := range chunk {
		sbMsg := envelopeToMessage(env, defaultSessionID, clk)

		addErr := msgBatch.AddMessage(sbMsg, nil)
		if addErr == nil {
			continue
		}
		if !errors.Is(addErr, azservicebus.ErrMessageTooLarge) {
			return sent, MapError(addErr)
		}

		if logging.DebugEnabled(logger) {
			logger.Log(ctx, logging.LevelDebug, "servicebus: message overflow, sending individually",
				"entity", entity)
		}

		if msgBatch.NumMessages() > 0 {
			if err := client.SendMessageBatch(ctx, msgBatch, nil); err != nil {
				return sent, MapError(err)
			}
			sent += int(msgBatch.NumMessages())
		}

		if err := sendOne(ctx, client, env, defaultSessionID, clk); err != nil {
			return sent, err
		}
		sent++

		// Start a fresh batch for remaining messages in this chunk.
		msgBatch, err = client.NewMessageBatch(ctx, nil)
		if err != nil {
			return sent, MapError(err)
		}
	}

	if msgBatch != nil && msgBatch.NumMessages() > 0 {
		if err := client.SendMessageBatch(ctx, msgBatch, nil); err != nil {
			return sent, MapError(err)
		}
		sent += int(msgBatch.NumMessages())
	}

	return sent, nil
}
