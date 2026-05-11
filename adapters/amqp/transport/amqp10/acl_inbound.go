package amqp10

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// messageToHeaders maps AMQP 1.0 message properties and application
// properties to envelope headers. Reserved bridge headers and AMQP
// 1.0 well-known headers are filtered.
func messageToHeaders(msg *amqp.Message) map[string]any {
	size := len(msg.ApplicationProperties) + 13
	h := make(map[string]any, size)

	if msg.Properties != nil {
		if msg.Properties.MessageID != nil {
			h[headerMessageID] = msg.Properties.MessageID
		}
		if msg.Properties.CorrelationID != nil {
			h[headerCorrelationID] = msg.Properties.CorrelationID
		}
		if msg.Properties.ContentType != nil {
			h[headerContentType] = *msg.Properties.ContentType
		}
		if msg.Properties.ContentEncoding != nil {
			h[headerContentEncoding] = *msg.Properties.ContentEncoding
		}
		if msg.Properties.Subject != nil {
			h[headerSubject] = *msg.Properties.Subject
		}
		if msg.Properties.To != nil {
			h[headerTo] = *msg.Properties.To
		}
		if msg.Properties.ReplyTo != nil {
			h[headerReplyTo] = *msg.Properties.ReplyTo
		}
		if msg.Properties.GroupID != nil {
			h[headerGroupID] = *msg.Properties.GroupID
		}
		if msg.Properties.GroupSequence != nil {
			h[headerGroupSequence] = *msg.Properties.GroupSequence
		}
		if msg.Properties.ReplyToGroupID != nil {
			h[headerReplyToGroupID] = *msg.Properties.ReplyToGroupID
		}
		if msg.Properties.CreationTime != nil {
			h[headerCreationTime] = *msg.Properties.CreationTime
		}
		if msg.Properties.AbsoluteExpiryTime != nil {
			h[headerAbsoluteExpiry] = *msg.Properties.AbsoluteExpiryTime
		}
	}

	if msg.Header != nil {
		h[headerDeliveryCount] = msg.Header.DeliveryCount
	}

	for k, v := range msg.ApplicationProperties {
		if messaging.IsReservedHeader(k) || strings.HasPrefix(k, headerPrefix) {
			continue
		}
		h[k] = v
	}

	return h
}

// receiverLink wraps a *amqp.Receiver, exposing only the operations
// the adapter's Receiver needs and converting incoming messages to
// domain-typed *Delivery values inside the ACL.
type receiverLink struct {
	raw *amqp.Receiver
}

// Receive blocks until a message is available, then translates it into
// a fresh *Delivery (with envelope, settler, etc.) without leaking SDK
// types to the caller. The receiver does NOT fall back to the link
// address when Properties.Subject is missing; an inbound message
// without Properties.Subject yields an Envelope with empty Subject.
func (r *receiverLink) Receive(
	ctx context.Context,
	logger *slog.Logger,
	metrics ports.MetricsExporter,
	clk clock.Clock,
) (*Delivery, error) {
	msg, err := r.raw.Receive(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("amqp10: receive: %w", err)
	}
	env, err := messageToEnvelope(msg, clk)
	if err != nil {
		// Reject malformed message at the broker so it is not redelivered
		// in an infinite loop, then surface the classified error to the
		// caller. We deliberately swallow Reject's error: the receive-side
		// classification is the authoritative signal.
		_ = r.raw.RejectMessage(ctx, msg, nil)
		return nil, err
	}
	return NewDelivery(env, msg, r.raw, logger, metrics, clk), nil
}

// Close closes the receiver link. The supplied context bounds the
// detach timeout.
func (r *receiverLink) Close(ctx context.Context) error {
	if r == nil || r.raw == nil {
		return nil
	}
	if err := r.raw.Close(ctx); err != nil {
		return fmt.Errorf("amqp10: receiver close: %w", err)
	}
	return nil
}

// messageToEnvelope translates an inbound *amqp.Message into a fresh
// messaging.Envelope. The logical Envelope.Subject is sourced solely
// from Properties.Subject; if the inbound message has no Subject the
// envelope's Subject is left empty (no fallback to the link address).
// The raw amqp10.subject header is still recorded under
// envelope.Headers() via messageToHeaders for full property round-trip.
func messageToEnvelope(msg *amqp.Message, clk clock.Clock) (*messaging.Envelope, error) {
	if clk == nil {
		clk = clock.System
	}
	headers := messageToHeaders(msg)

	var msgID string
	if msg.Properties != nil && msg.Properties.MessageID != nil {
		if s, ok := msg.Properties.MessageID.(string); ok {
			msgID = s
		}
	}
	if msgID == "" {
		msgID = generateEnvelopeID()
	}

	var subject string
	if msg.Properties != nil && msg.Properties.Subject != nil {
		subject = *msg.Properties.Subject
	}

	var body []byte
	if len(msg.Data) > 0 {
		body = msg.Data[0]
	} else if msg.Value != nil {
		if b, ok := msg.Value.([]byte); ok {
			body = b
		}
	}

	env, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID:        msgID,
		Subject:   subject,
		Payload:   body,
		Headers:   headers,
		CreatedAt: clk.Now(),
	}, clk.Now())
	if err != nil {
		return nil, wrapEnvelopeErr(err)
	}
	if msg.Properties != nil && msg.Properties.AbsoluteExpiryTime != nil {
		env.ExpiresAt = *msg.Properties.AbsoluteExpiryTime
	}
	return env, nil
}

func generateEnvelopeID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("amqp10: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
