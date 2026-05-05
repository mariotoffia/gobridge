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

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"
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
		if domain.IsReservedHeader(k) || strings.HasPrefix(k, headerPrefix) {
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
// types to the caller.
func (r *receiverLink) Receive(
	ctx context.Context,
	defaultSubject string,
	logger *slog.Logger,
	metrics ports.MetricsExporter,
	clk clock.Clock,
) (*Delivery, error) {
	msg, err := r.raw.Receive(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("amqp10: receive: %w", err)
	}
	env := messageToEnvelope(msg, defaultSubject, clk)
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
// domain.Envelope. The defaultSubject parameter is used when the
// message does not carry one of its own.
func messageToEnvelope(msg *amqp.Message, defaultSubject string, clk clock.Clock) *domain.Envelope {
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

	subject := defaultSubject
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

	env := &domain.Envelope{
		ID:        msgID,
		Subject:   subject,
		Payload:   body,
		Headers:   headers,
		CreatedAt: clk.Now(),
	}
	if msg.Properties != nil && msg.Properties.AbsoluteExpiryTime != nil {
		env.ExpiresAt = *msg.Properties.AbsoluteExpiryTime
	}
	return env
}

func generateEnvelopeID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("amqp10: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
