package amqp091

// Well-known header keys for AMQP 0-9-1 system properties. Stored as
// envelope headers when an inbound delivery is translated, and consumed
// when an outbound envelope is converted back to a publishing.
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
