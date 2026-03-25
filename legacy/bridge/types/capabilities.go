package types

// CapabilityType identifies a specific capability that a Source or Target supports.
// Use capabilities to query what features are available and adapt behavior accordingly.
type CapabilityType string

// ============================================================================
// Delivery Semantics Capabilities
// ============================================================================

// These capabilities describe the delivery guarantees of a Source or Target.
const (
	// CapabilityReceiveExactOnce indicates the source supports exact-once delivery.
	// Messages are delivered exactly once and automatically acknowledged on success.
	//
	// Suitable for: FIFO queues with deduplication, transactional systems.
	CapabilityReceiveExactOnce CapabilityType = "ReceiveExactOnce"

	// CapabilityReceiveAtLeastOnce indicates the source will redeliver unacked messages.
	// Messages may be delivered multiple times if not acknowledged.
	//
	// Suitable for: Most queue systems (SQS, RabbitMQ, Kafka with commits).
	// Consumer SHOULD handle duplicates idempotently.
	CapabilityReceiveAtLeastOnce CapabilityType = "ReceiveAtLeastOnce"

	// CapabilityReceiveAtMostOnce indicates messages are not redelivered.
	// Messages may be lost if processing fails.
	//
	// Suitable for: MQTT QoS 0, fire-and-forget telemetry.
	CapabilityReceiveAtMostOnce CapabilityType = "ReceiveAtMostOnce"

	// CapabilityPublishExactOnce indicates the target ensures exact-once delivery.
	// Each message is delivered exactly once to the target system.
	//
	// Suitable for: MQTT QoS 2, transactional sends.
	CapabilityPublishExactOnce CapabilityType = "PublishExactOnce"

	// CapabilityPublishAtLeastOnce indicates the target retries until acknowledged.
	// Messages are guaranteed to reach the target but may be duplicated.
	//
	// Suitable for: MQTT QoS 1, SQS sends with retries.
	CapabilityPublishAtLeastOnce CapabilityType = "PublishAtLeastOnce"

	// CapabilityPublishAtMostOnce indicates no delivery guarantee.
	// Send is attempted once without confirmation.
	//
	// Suitable for: MQTT QoS 0, best-effort telemetry.
	CapabilityPublishAtMostOnce CapabilityType = "PublishAtMostOnce"
)

// ============================================================================
// Source-Specific Capabilities
// ============================================================================

// These capabilities are specific to Source implementations.
const (
	// CapabilityRedelivery indicates Nack() can return messages for redelivery.
	// When set, calling Nack() on a SourceMessage will cause the message to be
	// redelivered by the source system (e.g., visibility timeout reset in SQS).
	//
	// Sources without this capability will log failures but cannot redeliver.
	CapabilityRedelivery CapabilityType = "Redelivery"

	// CapabilityExtendTimeout indicates Extend() is functional.
	// When set, calling Extend() on a SourceMessage will extend the processing
	// deadline (e.g., visibility timeout in SQS).
	//
	// Use for long-running message processing to prevent premature redelivery.
	CapabilityExtendTimeout CapabilityType = "ExtendTimeout"

	// CapabilityOrdering indicates messages are delivered in order.
	// When set, the source guarantees FIFO ordering of messages.
	//
	// Note: Ordering may be per-partition/group depending on the source.
	CapabilityOrdering CapabilityType = "Ordering"

	// CapabilityPrefetch indicates the source supports message prefetching.
	// When set, the source can fetch multiple messages ahead of processing.
	CapabilityPrefetch CapabilityType = "Prefetch"
)

// ============================================================================
// Target-Specific Capabilities
// ============================================================================

// These capabilities are specific to Target implementations.
const (
	// CapabilityBatching indicates the target supports batch sends.
	// When set, SendBatch() is optimized for multiple messages.
	CapabilityBatching CapabilityType = "Batching"

	// CapabilityTransactional indicates the target supports transactions.
	// When set, multiple sends can be committed atomically.
	CapabilityTransactional CapabilityType = "Transactional"

	// CapabilityNativeRetry indicates the target handles retries internally.
	// When set, the transport protocol handles retransmission (e.g., MQTT QoS 1/2).
	//
	// This capability is AUTO-DETECTED by the transport retry system:
	//   - If TransportRetryConfig.SkipNativeRetry=true (default) AND target has this capability,
	//     transport retry is skipped to avoid redundant retransmission.
	//   - If SkipNativeRetry=false, transport retry is used regardless of this capability.
	//
	// Transports that expose this capability:
	//   - MQTT QoS 1: PUBACK handshake (at-least-once)
	//   - MQTT QoS 2: PUBCOMP handshake (exactly-once)
	//   - SQS: Built-in retry with visibility timeout
	//   - Azure Service Bus: Native retry with dead-letter support
	//
	// Note: This only applies to transport-level delivery. Application-level
	// failures (e.g., middleware errors) still need MESSAGE RETRY via RetryManager.
	//
	// See: TransportRetryConfig.SkipNativeRetry
	CapabilityNativeRetry CapabilityType = "NativeRetry"
)

// ============================================================================
// Retry-Related Capabilities
// ============================================================================

// These capabilities relate to retry and dead-letter queue handling.
const (
	// CapabilityDeadLetterQueue indicates built-in DLQ support.
	// When set, failed messages can be automatically moved to a dead-letter queue.
	CapabilityDeadLetterQueue CapabilityType = "DeadLetterQueue"

	// CapabilityDelayedDelivery indicates support for delayed message delivery.
	// When set, messages can be scheduled for future delivery (retry backoff).
	CapabilityDelayedDelivery CapabilityType = "DelayedDelivery"
)

// ============================================================================
// Capability Struct and Helpers
// ============================================================================

// Capability represents a single capability with optional configuration value.
// The Type identifies the capability, and Value provides additional details.
type Capability struct {
	// Type identifies the capability.
	Type CapabilityType `json:"type"`

	// Value provides optional capability-specific configuration.
	// For example, CapabilityPrefetch might have Value=10 for prefetch count.
	Value any `json:"value,omitempty"`
}

// Capabilities is a collection of capabilities.
type Capabilities []Capability

// Has returns true if the capability type is present.
func (c Capabilities) Has(t CapabilityType) bool {
	for _, cap := range c {
		if cap.Type == t {
			return true
		}
	}
	return false
}

// Get returns the capability with the given type, or nil if not present.
func (c Capabilities) Get(t CapabilityType) *Capability {
	for i := range c {
		if c[i].Type == t {
			return &c[i]
		}
	}
	return nil
}

// GetValue returns the value of a capability, or the default if not present.
func (c Capabilities) GetValue(t CapabilityType, defaultValue any) any {
	cap := c.Get(t)
	if cap == nil || cap.Value == nil {
		return defaultValue
	}
	return cap.Value
}

// Add adds a capability to the collection.
func (c *Capabilities) Add(cap Capability) {
	*c = append(*c, cap)
}

// AddType adds a capability with just a type (no value).
func (c *Capabilities) AddType(t CapabilityType) {
	*c = append(*c, Capability{Type: t})
}

// AddWithValue adds a capability with a type and value.
func (c *Capabilities) AddWithValue(t CapabilityType, value any) {
	*c = append(*c, Capability{Type: t, Value: value})
}
