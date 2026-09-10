package servicebus

// Metric names emitted by the Azure Service Bus transport adapter. Relocated
// from domain/shared as part of shared-kernel slimming; the string values are
// the wire identities reported to CloudWatch/OTel and MUST NOT change.
const (
	MetricASBReceiveLatency    = "ASBReceiveLatency"
	MetricASBCompleteLatency   = "ASBCompleteLatency"
	MetricASBSendLatency       = "ASBSendLatency"
	MetricASBSendBatchLatency  = "ASBSendBatchLatency"
	MetricASBScheduleLatency   = "ASBScheduleLatency"
	MetricASBLockRenewals      = "ASBLockRenewals"
	MetricASBMalformedMessages = "ASBMalformedMessages"

	// MetricASBReceiveFailures counts failed ReceiveMessages polls
	// (transport-level receive errors that trigger poll backoff).
	MetricASBReceiveFailures = "ASBReceiveFailures"

	// MetricASBLockRenewalCapExceeded counts deliveries whose lock
	// auto-renewal hit ReceiverConfig.MaxLockRenewalDuration and had
	// their processing context cancelled (hung-pipeline guard).
	MetricASBLockRenewalCapExceeded = "ASBLockRenewalCapExceeded"

	// MetricASBLockRenewalFailures counts INDIVIDUAL lock-renewal
	// failures (per-delivery auto-extend and the session renewer).
	// MetricASBLockRenewals counts only successes, so a renewer failing
	// silently was previously invisible; this makes renewal degradation
	// alertable.
	MetricASBLockRenewalFailures = "ASBLockRenewalFailures"

	// MetricASBLockRenewerStopped signals that a lock renewer gave up
	// after autoExtendMaxFailures consecutive failures: the per-delivery
	// auto-extend loop stops and cancels processing, and the shared
	// session renewer reports its current session as persistently
	// un-renewable (it keeps running to cover future sessions — see
	// runSessionRenewer). A non-zero rate here means locks are lapsing
	// and messages are being redelivered/dead-lettered.
	//
	// Dimensioned by asbTagKeyRenewerScope so operators can tell an
	// imminent redelivery from a recoverable degradation: scope
	// "delivery" (a per-message auto-extend loop returned — the message
	// WILL redeliver) vs "session" (the shared session renewer is
	// degraded but STILL running / self-healing).
	MetricASBLockRenewerStopped = "ASBLockRenewerStopped"
)

// Tag keys and values dimensioning the ASB transport metrics. There is no
// shared.TagKeyScope in the shared kernel, so this module-local key
// follows the shared.Tag idiom (shared.Tag{Key, Value}).
const (
	// asbTagKeyRenewerScope dimensions MetricASBLockRenewerStopped by
	// which renewer stopped (asbRenewerScope* below).
	asbTagKeyRenewerScope = "scope"

	// asbRenewerScopeDelivery: a per-message auto-extend loop returned
	// after autoExtendMaxFailures and cancelled processing — the message
	// WILL be redelivered.
	asbRenewerScopeDelivery = "delivery"
	// asbRenewerScopeSession: the shared session renewer is persistently
	// failing on its current session but is STILL running; it self-heals
	// on the next successful renewal or a newly accepted session.
	asbRenewerScopeSession = "session"
)
