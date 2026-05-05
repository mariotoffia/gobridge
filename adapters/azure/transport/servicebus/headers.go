package servicebus

// Well-known header keys for Azure Service Bus system properties.
// Stored as envelope headers when an inbound message is translated by
// acl_inbound.go's messageToHeaders, and consumed when an outbound
// envelope is converted back to a *azservicebus.Message in
// acl_outbound.go's envelopeToMessage / headersToMessage.
const (
	asbHeaderPrefix        = "asb."
	asbHeaderMessageID     = "asb.message-id"
	asbHeaderCorrelationID = "asb.correlation-id"
	asbHeaderSessionID     = "asb.session-id"
	asbHeaderContentType   = "asb.content-type"
	asbHeaderSubject       = "asb.subject"
	asbHeaderTo            = "asb.to"
	asbHeaderReplyTo       = "asb.reply-to"
	asbHeaderTTL           = "asb.ttl"
	asbHeaderEnqueuedTime  = "asb.enqueued-time"
	asbHeaderSequenceNum   = "asb.sequence-number"
	asbHeaderDeliveryCount = "asb.delivery-count"
)

var asbWellKnownHeaders = map[string]bool{
	asbHeaderMessageID:     true,
	asbHeaderCorrelationID: true,
	asbHeaderSessionID:     true,
	asbHeaderContentType:   true,
	asbHeaderSubject:       true,
	asbHeaderTo:            true,
	asbHeaderReplyTo:       true,
	asbHeaderTTL:           true,
	asbHeaderEnqueuedTime:  true,
	asbHeaderSequenceNum:   true,
	asbHeaderDeliveryCount: true,
}
