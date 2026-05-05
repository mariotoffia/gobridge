package amqp10

// AMQP 1.0 message property header keys. These are the well-known
// keys used to round-trip system-defined message properties through
// the envelope's free-form Headers map. Constants live in this
// SDK-free file so non-ACL files can reference them when consulting
// envelope headers.
const (
	headerMessageID       = "amqp10.message-id"
	headerCorrelationID   = "amqp10.correlation-id"
	headerContentType     = "amqp10.content-type"
	headerContentEncoding = "amqp10.content-encoding"
	headerSubject         = "amqp10.subject"
	headerTo              = "amqp10.to"
	headerReplyTo         = "amqp10.reply-to"
	headerGroupID         = "amqp10.group-id"
	headerGroupSequence   = "amqp10.group-sequence"
	headerReplyToGroupID  = "amqp10.reply-to-group-id"
	headerCreationTime    = "amqp10.creation-time"
	headerAbsoluteExpiry  = "amqp10.absolute-expiry-time"
	headerDeliveryCount   = "amqp10.delivery-count"
)

const headerPrefix = "amqp10."

var wellKnownHeaders = map[string]bool{
	headerMessageID:       true,
	headerCorrelationID:   true,
	headerContentType:     true,
	headerContentEncoding: true,
	headerSubject:         true,
	headerTo:              true,
	headerReplyTo:         true,
	headerGroupID:         true,
	headerGroupSequence:   true,
	headerReplyToGroupID:  true,
	headerCreationTime:    true,
	headerAbsoluteExpiry:  true,
	headerDeliveryCount:   true,
}
