package amqp10

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// headersToMessage maps envelope headers to AMQP 1.0 message properties
// and application properties.
func headersToMessage(headers map[string]any) *amqp.Message {
	msg := &amqp.Message{}
	props := &amqp.MessageProperties{}
	hasProps := false

	if v, ok := headers[headerMessageID]; ok {
		props.MessageID = v
		hasProps = true
	}
	if v, ok := headers[headerCorrelationID]; ok {
		props.CorrelationID = v
		hasProps = true
	}
	if v, ok := headers[headerContentType]; ok {
		if s, ok := v.(string); ok {
			props.ContentType = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerContentEncoding]; ok {
		if s, ok := v.(string); ok {
			props.ContentEncoding = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerSubject]; ok {
		if s, ok := v.(string); ok {
			props.Subject = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerTo]; ok {
		if s, ok := v.(string); ok {
			props.To = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerReplyTo]; ok {
		if s, ok := v.(string); ok {
			props.ReplyTo = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerGroupID]; ok {
		if s, ok := v.(string); ok {
			props.GroupID = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerGroupSequence]; ok {
		switch n := v.(type) {
		case uint32:
			props.GroupSequence = &n
			hasProps = true
		case int:
			if n >= 0 {
				u := uint32(n)
				props.GroupSequence = &u
				hasProps = true
			}
		}
	}
	if v, ok := headers[headerReplyToGroupID]; ok {
		if s, ok := v.(string); ok {
			props.ReplyToGroupID = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerCreationTime]; ok {
		if t, ok := v.(time.Time); ok {
			props.CreationTime = &t
			hasProps = true
		}
	}
	if v, ok := headers[headerAbsoluteExpiry]; ok {
		if t, ok := v.(time.Time); ok {
			props.AbsoluteExpiryTime = &t
			hasProps = true
		}
	}

	if hasProps {
		msg.Properties = props
	}

	var appProps map[string]any
	for k, v := range headers {
		if wellKnownHeaders[k] || strings.HasPrefix(k, headerPrefix) {
			continue
		}
		if messaging.IsReservedHeader(k) {
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

// envelopeToMessage builds an outbound *amqp.Message from an envelope,
// merging headers, payload, and any envelope-level fields (ID, subject,
// expiry, creation time) into a single SDK message.
func envelopeToMessage(env *messaging.Envelope) *amqp.Message {
	msg := headersToMessage(env.Headers)
	msg.Data = [][]byte{env.Payload}

	if msg.Properties == nil {
		msg.Properties = &amqp.MessageProperties{}
	}
	if env.ID != "" {
		msg.Properties.MessageID = env.ID
	}
	if env.Subject != "" {
		msg.Properties.Subject = &env.Subject
	}
	if env.HasExpiry() {
		expiry := env.ExpiresAt
		msg.Properties.AbsoluteExpiryTime = &expiry
	}
	if !env.CreatedAt.IsZero() {
		msg.Properties.CreationTime = &env.CreatedAt
	}
	return msg
}

// senderLink wraps a *amqp.Sender, exposing only an envelope-typed
// Send. *amqp.Sender.Send is documented as safe for concurrent use, so
// this wrapper preserves that contract: SendEnvelope may be invoked
// from many goroutines at once.
type senderLink struct {
	raw *amqp.Sender
}

// SendEnvelope serialises the envelope into an AMQP 1.0 message and
// publishes it over the link. Errors flow through unwrapped so callers
// can MapError them at the seam.
func (s *senderLink) SendEnvelope(ctx context.Context, env *messaging.Envelope) error {
	msg := envelopeToMessage(env)
	if err := s.raw.Send(ctx, msg, nil); err != nil {
		return fmt.Errorf("amqp10: send: %w", err)
	}
	return nil
}

// Close closes the link with the supplied context as detach timeout.
func (s *senderLink) Close(ctx context.Context) error {
	if s == nil || s.raw == nil {
		return nil
	}
	if err := s.raw.Close(ctx); err != nil {
		return fmt.Errorf("amqp10: sender close: %w", err)
	}
	return nil
}
