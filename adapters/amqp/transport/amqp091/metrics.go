package amqp091

// Metric names emitted by the AMQP 0-9-1 transport adapter. Relocated from
// domain/shared as part of shared-kernel slimming; the string values are the
// wire identities reported to CloudWatch/OTel and MUST NOT change.
const (
	MetricAMQP091ConnectLatency   = "AMQP091ConnectLatency"
	MetricAMQP091ReconcileLatency = "AMQP091ReconcileLatency"
	MetricAMQP091PublishLatency   = "AMQP091PublishLatency"
	MetricAMQP091ConsumeLatency   = "AMQP091ConsumeLatency"
	MetricAMQP091AckLatency       = "AMQP091AckLatency"
	MetricAMQP091Reconnects       = "AMQP091Reconnects"
	MetricAMQP091EventDropped     = "AMQP091EventDropped"
	// MetricAMQP091Blocked counts connection.blocked / connection.unblocked
	// transitions the broker raises under a resource alarm (memory/disk).
	// A non-zero, climbing count distinguishes broker pushback from
	// ordinary send timeouts.
	MetricAMQP091Blocked = "AMQP091Blocked"
	// MetricAMQP091PublisherDeclareFailed counts publish-side exchange
	// auto-declares that the broker rejected (e.g. PRECONDITION_FAILED against
	// an externally-managed exchange, or ACCESS_REFUSED under least-privilege
	// credentials). Publisher declare is best-effort: the failure is metered
	// here rather than aborting the session, because publishing to the exchange
	// still works when it already exists (ADV-F1-P3). A climbing count means the
	// bridge cannot own the exchange topology and an operator should pre-declare
	// it (or grant configure permission).
	MetricAMQP091PublisherDeclareFailed = "AMQP091PublisherDeclareFailed"
	// MetricAMQP091ReconnectRaceRetried counts permanent-classified consume
	// failures the receiver retried as transient reconnect races: a 403
	// ACCESS_REFUSED on an exclusive consumer (the broker holds the stale
	// consumer for ~2x heartbeat after a partition) and a 404 NOT_FOUND
	// while the session is still re-declaring topology after a reconnect.
	// A count that keeps climbing past the retry budget means the error is
	// a genuine misconfiguration, not a race.
	MetricAMQP091ReconnectRaceRetried = "AMQP091ReconnectRaceRetried"
	// MetricAMQP091DelayedRetryUnhonored counts delayed (backoff) retries
	// that could not be honored client-side. AMQP 0-9-1 has no native
	// delayed-redelivery primitive, so Retry(after>0) nacks with immediate
	// requeue: the runtime's requested spacing is silently lost and a
	// poison message can hot-loop on a classic queue unless the queue has
	// an x-delivery-limit / dead-letter-exchange guard. Mirrors
	// AMQP10DelayedRetryUnhonored on the amqp10 adapter.
	MetricAMQP091DelayedRetryUnhonored = "AMQP091DelayedRetryUnhonored"
)
