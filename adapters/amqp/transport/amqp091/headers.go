package amqp091

import (
	"fmt"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain"
)

// Well-known header keys for AMQP 0-9-1 system properties.
const (
	HeaderMessageID       = "amqp091.message-id"
	HeaderCorrelationID   = "amqp091.correlation-id"
	HeaderContentType     = "amqp091.content-type"
	HeaderContentEncoding = "amqp091.content-encoding"
	HeaderReplyTo         = "amqp091.reply-to"
	HeaderType            = "amqp091.type"
	HeaderAppID           = "amqp091.app-id"
	HeaderDeliveryMode    = "amqp091.delivery-mode"
	HeaderPriority        = "amqp091.priority"
	HeaderExpiration      = "amqp091.expiration"
	HeaderTimestamp       = "amqp091.timestamp"
	HeaderExchange        = "amqp091.exchange"
	HeaderRoutingKey      = "amqp091.routing-key"
	HeaderDeliveryTag     = "amqp091.delivery-tag"
	HeaderRedelivered     = "amqp091.redelivered"
	HeaderConsumerTag     = "amqp091.consumer-tag"
)

const amqp091Prefix = "amqp091."

var amqp091WellKnown = map[string]bool{
	HeaderMessageID:       true,
	HeaderCorrelationID:   true,
	HeaderContentType:     true,
	HeaderContentEncoding: true,
	HeaderReplyTo:         true,
	HeaderType:            true,
	HeaderAppID:           true,
	HeaderDeliveryMode:    true,
	HeaderPriority:        true,
	HeaderExpiration:      true,
	HeaderTimestamp:       true,
	HeaderExchange:        true,
	HeaderRoutingKey:      true,
	HeaderDeliveryTag:     true,
	HeaderRedelivered:     true,
	HeaderConsumerTag:     true,
}

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
		if domain.IsReservedHeader(k) || strings.HasPrefix(k, amqp091Prefix) {
			continue
		}
		h[k] = v
	}

	return h
}

// headersToPublishing maps envelope headers back to an amqp091.Publishing.
// Well-known amqp091.* headers are extracted into typed AMQP properties;
// remaining headers (excluding amqp091.* prefixed and reserved x-bridge.*)
// are placed in the AMQP Headers table.
func headersToPublishing(headers map[string]any) amqp.Publishing {
	pub := amqp.Publishing{}
	if headers == nil {
		return pub
	}

	if v, ok := headers[HeaderMessageID].(string); ok {
		pub.MessageId = v
	}
	if v, ok := headers[HeaderCorrelationID].(string); ok {
		pub.CorrelationId = v
	}
	if v, ok := headers[HeaderContentType].(string); ok {
		pub.ContentType = v
	}
	if v, ok := headers[HeaderContentEncoding].(string); ok {
		pub.ContentEncoding = v
	}
	if v, ok := headers[HeaderReplyTo].(string); ok {
		pub.ReplyTo = v
	}
	if v, ok := headers[HeaderType].(string); ok {
		pub.Type = v
	}
	if v, ok := headers[HeaderAppID].(string); ok {
		pub.AppId = v
	}
	if v, ok := headers[HeaderDeliveryMode].(uint8); ok {
		pub.DeliveryMode = v
	}
	if v, ok := headers[HeaderPriority].(uint8); ok {
		pub.Priority = v
	}
	if v, ok := headers[HeaderExpiration].(string); ok {
		pub.Expiration = v
	}
	if v, ok := headers[HeaderTimestamp].(time.Time); ok {
		pub.Timestamp = v
	}

	var table amqp.Table
	for k, v := range headers {
		if amqp091WellKnown[k] || strings.HasPrefix(k, amqp091Prefix) {
			continue
		}
		if domain.IsReservedHeader(k) {
			continue
		}
		if table == nil {
			table = make(amqp.Table, len(headers))
		}
		table[k] = v
	}
	pub.Headers = table

	return pub
}

// envelopeToPublishing builds an amqp091.Publishing from a domain.Envelope.
// It maps the envelope body, ID, subject, TTL, and headers.
func envelopeToPublishing(env *domain.Envelope, cfg SenderConfig) amqp.Publishing {
	pub := headersToPublishing(env.Headers)
	pub.Body = env.Payload

	if env.ID != "" && pub.MessageId == "" {
		pub.MessageId = env.ID
	}

	if env.HasExpiry() {
		if ttl := env.RemainingTTL(); ttl > 0 {
			pub.Expiration = fmt.Sprintf("%d", ttl.Milliseconds())
		}
	}

	if pub.ContentType == "" && env.Headers != nil {
		if ct, ok := env.Headers[domain.HeaderContentType].(string); ok {
			pub.ContentType = ct
		}
	}

	return pub
}
