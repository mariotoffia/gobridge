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

// HeaderGobridgeSubject is the AMQP 0-9-1 user-header key used to
// round-trip the logical Envelope.Subject through AMQP, distinct from
// the transport-level routing key. envelopeToPublishing writes this
// header into the AMQP Headers table when env.Subject() is non-empty;
// deliveryToEnvelope reads it back into env.Subject(). Inbound headers
// carrying this key from a peer bridge are honoured (subject-preserving
// round trip is intentional); the generic user-header pass-through
// skips this reserved key so the typed extraction wins.
//
// Unlike the amqp091.* well-known headers, this is a cross-transport
// subject carrier (the same key is used by the MQTT adapter), so it is
// NOT registered in amqp091WellKnown / amqp091Prefix.
const HeaderGobridgeSubject = "gobridge.subject"

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
