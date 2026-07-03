package shared

// Tag is a key-value pair used as a dimension on emitted metrics.
type Tag struct {
	Key   string
	Value string
}

// Metric namespace used by the bridge runtime.
const MetricNamespace = "GoBridge/Runtime"

// Lease metric names.
const (
	MetricLeaseAcquireLatency  = "LeaseAcquireLatency"
	MetricLeaseRenewLatency    = "LeaseRenewLatency"
	MetricLeaseAcquireFailures = "LeaseAcquireFailures"
	MetricLeaseExpiries        = "LeaseExpiries"
	MetricLeaseTransfers       = "LeaseTransfers"
)

// Outbox metric names.
const (
	MetricOutboxPersistLatency    = "OutboxPersistLatency"
	MetricOutboxDrainLatency      = "OutboxDrainLatency"
	MetricOutboxDepth             = "OutboxDepth"
	MetricOutboxClaimRecoveries   = "OutboxClaimRecoveries"
	MetricOutboxCompletions       = "OutboxCompletions"
	MetricOutboxExpiredBeforeSend = "OutboxExpiredBeforeSend"
	MetricOutboxReplayCount       = "OutboxReplayCount"
	MetricOutboxRecordFailures    = "OutboxRecordFailures"
	MetricOutboxDuplicateRisk     = "OutboxDuplicateRisk"
	// MetricOutboxDeferred counts claimed records the drainer could NOT process
	// this cycle (batch deadline expired before the send launched or completed)
	// and released/left for the next drain. They are neither successes nor hard
	// failures; a rising value under load flags a drain budget too small for the
	// batch size (see Drainer.drainBatch batch-deadline handling).
	MetricOutboxDeferred = "OutboxDeferred"
	// MetricOutboxClaimConflicts counts per-record claim transactions aborted
	// because a concurrent Persist/Claim/Complete touched the same item — as
	// distinct from a record-level conditional failure (another claimer
	// legitimately won the record, which is normal). A rising value explains
	// why a Claim returned fewer than `limit` records because of CONTENTION
	// rather than an empty backlog (lag), which is otherwise silent. Tagged
	// with the partition (TagKeyPartition).
	MetricOutboxClaimConflicts = "OutboxClaimConflicts"
	// MetricDrainSkippedNoLease counts drain cycles skipped because the drainer's
	// TokenFn reported no held lease. A continuously-rising value on a route that
	// is supposed to drain flags a misconfiguration (e.g. shared_outbox bound to
	// a non-exclusive session that never acquires a lease) rather than a normal
	// standby that legitimately holds no lease.
	MetricDrainSkippedNoLease = "DrainSkippedNoLease"
)

// Generic transport-agnostic delivery metric names.
const (
	MetricAckLatency           = "AckLatency"
	MetricVisibilityExtensions = "VisibilityExtensions"
)

// Delivery metric names.
const (
	MetricDeliveryE2ELatency = "DeliveryE2ELatency"
	MetricDLQEntries         = "DLQEntries"
	MetricDLQWriteFailures   = "DLQWriteFailures"
	MetricDeliveryPanics     = "DeliveryPanics"
)

// Throughput metric names.
const (
	MetricMessagesReceived = "MessagesReceived"
	MetricMessagesSent     = "MessagesSent"
	// MetricMessagesDropped counts messages the runtime terminated WITHOUT a
	// DLQ record and without a successful send: a permanent/expired/unsupported
	// outcome under a drop policy (or a missing DLQ store on a retry-unsupported
	// source). Together with MessagesReceived, MessagesSent, MetricDLQEntries and
	// in-flight it closes the conservation law received = sent + dropped + dlq +
	// inflight, so a rising Dropped is the single signal for silent message loss.
	MetricMessagesDropped = "MessagesDropped"
	// MetricMessagesFiltered counts messages a processor deliberately dropped
	// (shared.ErrMessageFiltered) under OnPermanentFailure=drop — a POLICY
	// discard, not a fault. Split from MessagesDropped so an intentional filter
	// rate never masks (or is masked by) genuine loss.
	MetricMessagesFiltered = "MessagesFiltered"
	// MetricMessagesExpired counts messages dropped because they expired before
	// delivery under OnExpired=drop (the ingress route-expired path and the
	// drainer expired-before-send path). Distinct from Filtered/Dropped so TTL
	// loss is separately observable.
	MetricMessagesExpired = "MessagesExpired"
	MetricRouteErrors     = "RouteErrors"
	// MetricReceiveCountUnparseable counts deliveries whose source-transport
	// redelivery-count header was PRESENT but uninterpretable as an integer, so
	// receiveCount failed open to a first delivery (count 0) and
	// MaxReplayAttempts could not cap replays (E5-FU3). Failing open is
	// deliberate — a good message is never DLQ'd on a parse error — but a
	// permanently-failing recoverable send on such a message would otherwise
	// retry unbounded with no signal; a rising value makes that observable and
	// flags a source stamping a malformed count.
	MetricReceiveCountUnparseable = "ReceiveCountUnparseable"
)

// Processor chain metric names.
const (
	MetricProcessorPanics   = "ProcessorPanics"
	MetricProcessorTimeouts = "ProcessorTimeouts"
)

// Circuit-breaker metric names. Emitted by the circuit-breaker processor
// (processors/circuitbreaker) around outbound protection state. The
// procs agent owns the emission sites; the name is declared here so the
// wire value is fixed once and shared. MetricCircuitBreakerStateChanged
// counts every open<->half-open<->closed transition (tag the breaker key
// and the new state); a spike is the leading indicator of a failing
// downstream dependency.
const (
	MetricCircuitBreakerStateChanged = "CircuitBreakerStateChanged"
	MetricCircuitBreakerTrips        = "CircuitBreakerTrips"
	MetricCircuitBreakerRejections   = "CircuitBreakerRejections"
)

// Session metric names. Emitted by the generic runtime session manager
// (runtime/session), not by any single transport adapter.
// MetricMQTTReconnects keeps its historical "MQTTReconnects" wire value to
// avoid an observability break, despite the transport-flavored identifier.
const (
	MetricMQTTReconnects    = "MQTTReconnects"
	MetricReconcileFailures = "ReconcileFailures"
	// MetricSessionRestarts counts per-session supervised restarts: a session
	// manager returned a transient error and was restarted in isolation
	// (capped backoff) instead of tearing down the whole runtime (C3-FU2).
	// A rising value flags a session that keeps failing to reconnect/re-acquire
	// its lease while the rest of the bridge stays up — alert on it.
	MetricSessionRestarts = "SessionRestarts"
)

// Standard dimension key names for metric tags.
const (
	TagKeyLeaseID   = "lease_id"
	TagKeyRouteID   = "route_id"
	TagKeySessionID = "session_id"
	TagKeyPartition = "partition"
	TagKeyCategory  = "category"
	TagKeyTransport = "transport"
	TagKeyEntity    = "entity"
	// TagKeyState dimensions a metric by a component lifecycle state — used by
	// the circuit-breaker state-change counter (open/half-open/closed).
	TagKeyState = "state"
	// TagKeyReason dimensions a drop/filter/expire counter by the terminal
	// reason so a single MessagesDropped series can be split by cause.
	TagKeyReason = "reason"
)
