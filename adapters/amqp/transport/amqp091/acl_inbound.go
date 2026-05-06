package amqp091

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"strings"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// deliveryToHeaders maps an amqp091.Delivery's system properties and
// user-defined headers to an envelope header map. Reserved x-bridge.*
// headers from the AMQP Headers table are stripped to prevent injection.
func deliveryToHeaders(d amqp.Delivery) map[string]any {
	h := make(map[string]any, 16+len(d.Headers))

	if d.MessageId != "" {
		h[HeaderMessageID] = d.MessageId
	}
	if d.CorrelationId != "" {
		h[HeaderCorrelationID] = d.CorrelationId
	}
	if d.ContentType != "" {
		h[HeaderContentType] = d.ContentType
	}
	if d.ContentEncoding != "" {
		h[HeaderContentEncoding] = d.ContentEncoding
	}
	if d.ReplyTo != "" {
		h[HeaderReplyTo] = d.ReplyTo
	}
	if d.Type != "" {
		h[HeaderType] = d.Type
	}
	if d.AppId != "" {
		h[HeaderAppID] = d.AppId
	}
	if d.DeliveryMode != 0 {
		h[HeaderDeliveryMode] = d.DeliveryMode
	}
	if d.Priority != 0 {
		h[HeaderPriority] = d.Priority
	}
	if d.Expiration != "" {
		h[HeaderExpiration] = d.Expiration
	}
	if !d.Timestamp.IsZero() {
		h[HeaderTimestamp] = d.Timestamp
	}
	if d.Exchange != "" {
		h[HeaderExchange] = d.Exchange
	}
	if d.RoutingKey != "" {
		h[HeaderRoutingKey] = d.RoutingKey
	}

	h[HeaderDeliveryTag] = d.DeliveryTag
	h[HeaderRedelivered] = d.Redelivered

	if d.ConsumerTag != "" {
		h[HeaderConsumerTag] = d.ConsumerTag
	}

	for k, v := range d.Headers {
		if messaging.IsReservedHeader(k) || strings.HasPrefix(k, amqp091Prefix) {
			continue
		}
		h[k] = v
	}

	return h
}

// deliveryToEnvelope translates an inbound *amqp091.Delivery to a fresh
// messaging.Envelope. The CreatedAt field falls back to clk.Now() when the
// inbound message carries no timestamp.
func deliveryToEnvelope(d amqp.Delivery, clk clock.Clock) *messaging.Envelope {
	if clk == nil {
		clk = clock.System
	}
	env := &messaging.Envelope{
		ID:        d.MessageId,
		Subject:   d.RoutingKey,
		Payload:   d.Body,
		Headers:   deliveryToHeaders(d),
		CreatedAt: clk.Now(),
	}
	if env.ID == "" {
		env.ID = generateEnvelopeID()
	}
	if !d.Timestamp.IsZero() {
		env.CreatedAt = d.Timestamp
	}
	return env
}

func generateEnvelopeID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("amqp091: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func generateConsumerTag() string {
	b := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("amqp091: crypto/rand unavailable: " + err.Error())
	}
	return "gobridge-" + hex.EncodeToString(b)
}
