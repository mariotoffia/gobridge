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
	asbHeaderPartitionKey  = "asb.partition-key"
	asbHeaderContentType   = "asb.content-type"
	asbHeaderSubject       = "asb.subject"
	asbHeaderTo            = "asb.to"
	asbHeaderReplyTo       = "asb.reply-to"
	asbHeaderTTL           = "asb.ttl"
	asbHeaderEnqueuedTime  = "asb.enqueued-time"
	asbHeaderSequenceNum   = "asb.sequence-number"
	asbHeaderDeliveryCount = "asb.delivery-count"
)

// Reserved (x-bridge.*, UBIQUITOUS.md "Reserved header") application
// properties stamped by this adapter on the SCHEDULED COPY a delayed
// Retry enqueues. They live only on the Service Bus wire message —
// ingress strips every reserved ApplicationProperty (IsReservedHeader)
// before headers reach the envelope, so neither key ever appears in
// Envelope.Headers and an external producer cannot inject them past
// the ACL.
const (
	// asbPropRetryAttempt carries the accumulated 1-based receive count
	// at the moment the retry copy was scheduled. Scheduling a fresh
	// copy resets the broker's DeliveryCount to 1, so without this
	// counter the runtime's receive-count gate (MaxReplayAttempts) and
	// the broker's own MaxDeliveryCount would never fire — a poison
	// message would ping-pong forever. Ingress adds this value to the
	// broker DeliveryCount when stamping asb.delivery-count.
	asbPropRetryAttempt = "x-bridge.retry-attempt" // x-bridge-local: minted by servicebus egress, added to broker DeliveryCount and stripped at ingress; not a domain-registered header

	// asbPropOriginalMessageID preserves the FIRST delivery's MessageID
	// across retry copies. The copy's own MessageID is salted with the
	// attempt number so broker duplicate detection never silently
	// discards a scheduled retry; ingress restores this value as the
	// envelope ID so end-to-end (envelope-ID / idempotency) dedup still
	// sees one logical message.
	asbPropOriginalMessageID = "x-bridge.original-message-id" // x-bridge-local: minted by servicebus egress, restored as envelope ID and stripped at ingress; not a domain-registered header
)

var asbWellKnownHeaders = map[string]bool{
	asbHeaderMessageID:     true,
	asbHeaderCorrelationID: true,
	asbHeaderSessionID:     true,
	asbHeaderPartitionKey:  true,
	asbHeaderContentType:   true,
	asbHeaderSubject:       true,
	asbHeaderTo:            true,
	asbHeaderReplyTo:       true,
	asbHeaderTTL:           true,
	asbHeaderEnqueuedTime:  true,
	asbHeaderSequenceNum:   true,
	asbHeaderDeliveryCount: true,
}
