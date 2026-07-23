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
	// MetricBrokerHealthStepDown counts step-downs an active exclusive owner
	// initiated because its broker path stayed non-converged (disconnected / not
	// re-subscribed) beyond the configured broker-health threshold, so a healthy
	// standby can take over a node-local broker outage (CLUSTER-2). Only emitted
	// when broker_health_step_down is configured (opt-in).
	MetricBrokerHealthStepDown = "BrokerHealthStepDown"
)

// Outbox metric names.
const (
	MetricOutboxPersistLatency = "OutboxPersistLatency"
	MetricOutboxDrainLatency   = "OutboxDrainLatency"
	// MetricOutboxDepth is the partition-keyed gauge of PENDING outbox records —
	// the true backlog waiting to be drained. It is emitted from two sites, each
	// reporting a real pending count (never a claim-batch size):
	//   - the ingress path (runtime.InstrumentedOutboxStore.QueryPending) emits
	//     the pending count it observed, bounded by the query limit (the
	//     configured MaxOutboxDepth); and
	//   - the drain path (runtime/outbox.Drainer) emits the EXACT remaining
	//     pending count read from the store's OPTIONAL ports.OutboxDepthReporter
	//     capability — a dedicated COUNT primitive that does not saturate at the
	//     claim batch size. On a store that does NOT implement OutboxDepthReporter
	//     the drain path falls back to the claimed count, which saturates at the
	//     batch size (default 100, max 500) and is only a LOWER BOUND; implement
	//     OutboxDepthReporter on the store for a true, unbounded depth signal.
	// The default alarms read the Maximum statistic and treat missing data as
	// breaching — the gauge is emitted continuously while an outbox exists, so
	// silence means the drainer/bridge died. The per-cycle claim-batch liveness
	// signal is kept SEPARATE as MetricOutboxClaimBatchSize so a full batch can
	// never be mistaken for a shallow backlog (H-OBS).
	//
	// When a supported OutboxDepthReporter is present but its CountPending
	// returns a REAL error (DB/read failure — NOT ports.ErrOutboxDepthUnsupported),
	// the drainer SKIPS this gauge for that cycle rather than masking the failure
	// behind the saturating claimed-count fallback, so the breaching-on-missing
	// alarm catches a persistently broken depth query. The failure is recorded
	// on MetricOutboxDepthFailures + a structured error log.
	MetricOutboxDepth = "OutboxDepth"
	// MetricOutboxClaimBatchSize is the partition-keyed gauge of how many records
	// the drainer CLAIMED on its most recent poll cycle — a liveness/throughput
	// signal, NOT a backlog measure. It saturates at the current adaptive batch
	// size (the Claim ceiling) by design: a value at the ceiling means the drainer
	// is running flat-out and there may be more pending, so consult
	// MetricOutboxDepth for the true backlog. Split out from MetricOutboxDepth
	// (H-OBS) so a saturating claim size can never masquerade as a healthy shallow
	// depth. Tagged with the partition (TagKeyPartition), sharing the series shape
	// with MetricOutboxDepth.
	MetricOutboxClaimBatchSize = "OutboxClaimBatchSize"
	// MetricOutboxDepthFailures counts drain cycles where a SUPPORTED outbox
	// depth reporter's CountPending returned a REAL error (a DB/read failure,
	// not ports.ErrOutboxDepthUnsupported). On such a cycle the drainer skips the
	// MetricOutboxDepth emission (so the missing-data alarm can fire on a
	// persistently broken query instead of the failure being masked by the
	// saturating claimed-count fallback) and increments this counter, tagged with
	// the partition (TagKeyPartition), alongside a structured error log. A rising
	// value means the depth query itself is failing — investigate the store, not
	// the backlog (H-OBS).
	MetricOutboxDepthFailures     = "OutboxDepthFailures"
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
	// MetricOutboxDrainStalled counts drain batches whose in-flight sends did not
	// return within a generous grace past the batch deadline — the signature of a
	// Sender that ignores context cancellation and wedges the drain loop's
	// wg.Wait (Min2). A wedged batch is otherwise indistinguishable from an idle
	// drainer (both emit nothing), so a non-zero (and especially a rising) value
	// is the single signal that a sender is not honoring ctx and the partition is
	// stuck. The runtime does NOT kill the wedged goroutines — senders must honor
	// ctx — so this is a diagnostic, not a recovery, signal.
	MetricOutboxDrainStalled = "OutboxDrainStalled"
	// MetricOutboxStranded counts durable outbox records left with no drainer
	// after a live reload. The destructive-reload preflight proves an orphaned
	// shared_outbox partition empty at CHECK time and refuses otherwise, but a
	// record can still land in the swap window (late ingress) or a claimed send
	// can fail; the supervisor re-queries each orphaned partition on the NEW
	// runtime's store AFTER a successful swap and emits this counter (value = the
	// pending count) so the residual strand is observable instead of silent.
	// Tagged with the orphaned partition (TagKeyPartition). Non-zero means an
	// operator must drain that partition manually or restore a route/session for
	// it. Kept SEPARATE from MetricOutboxDepth (a steady-state gauge) and
	// MetricConfigReloads (a reload-outcome counter) so the strand alarm is
	// unambiguous.
	MetricOutboxStranded = "OutboxStranded"
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
	// MetricDLQDepth is a gauge of the CURRENT number of outstanding entries in
	// the dead-letter queue — the standing backlog — as opposed to
	// MetricDLQEntries, which is an INGRESS COUNTER that only ever counts writes
	// and never decreases. DLQEntries answers "how many were ever DLQ'd";
	// DLQDepth answers "how many are sitting in the DLQ right now", so a stale
	// backlog after a burst (writes have since stopped) is visible and alarmable
	// instead of requiring a manual storage scan (H-OBS DLQ-1). It is sampled
	// from a store's OPTIONAL ports.DLQDepthReporter capability via
	// runtime.ReportDLQDepth and emitted with NO route dimension (a fleet total),
	// so it matches the dimensionless default rollup alarm and stays low
	// cardinality. Alarm on DLQDepth > 0 sustained.
	MetricDLQDepth         = "DLQDepth"
	MetricDLQWriteFailures = "DLQWriteFailures"
	MetricDeliveryPanics   = "DeliveryPanics"
	// MetricDLQRedrives counts DLQ entries an admin redrive claimed and
	// re-injected successfully (route_id-tagged). MetricDLQRedriveFailures counts
	// redrive attempts that failed after (or during) the claim — inject failed,
	// claim failed, or a restore was attempted — so an operator can alert on
	// manual-recovery churn that the batch-level audit record does not surface.
	MetricDLQRedrives        = "DLQRedrives"
	MetricDLQRedriveFailures = "DLQRedriveFailures"
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
	// (shared.ErrMessageFiltered) under OnFiltered=drop — a POLICY discard, not a
	// fault. Split from MessagesDropped so an intentional filter rate never masks
	// (or is masked by) genuine loss.
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
	// MetricRouteRestarts counts per-route supervised restarts: a route runner
	// returned an error and was restarted in ISOLATION (jittered capped backoff)
	// instead of tearing down the whole runtime (per-route supervision). It
	// mirrors MetricSessionRestarts for the ingress/route side: a permanently
	// failing route (deleted queue, revoked permission) keeps restarting at the
	// backoff cap and stays visible here without crash-looping the pod or
	// killing healthy co-tenant routes. Alert on the rate (tag route_id), not on
	// liveness.
	MetricRouteRestarts = "RouteRestarts"
)

// Credential metric names. Emitted by the poll-based credential wrapper
// (runtime/credentials) around credential rotation polling.
const (
	// MetricCredentialRefreshFailures counts credential resolve failures during
	// rotation polling — both the initial seed resolve and every periodic poll
	// (runtime/credentials.PollBasedWrapper). A resolve failure is logged and
	// non-fatal (the loop retries next tick), so it is otherwise invisible; a
	// rising value flags a secrets backend (Secrets Manager / SSM) that is
	// unreachable or denying access, which would leave a session stuck on stale
	// credentials until the backend recovers. Tagged with nothing (the wrapper
	// is per-URI but the URI may carry secrets, so it is deliberately not a
	// dimension).
	MetricCredentialRefreshFailures = "CredentialRefreshFailures"
	// MetricCredentialRotationApplied counts credential rotations actually
	// APPLIED to a live transport target: the CredentialRefresher observed a
	// changed CredentialSet on a watched URI and a target's ApplyCredentials
	// returned without error (bridge.CredentialRefresher). Counted once per
	// target-apply, so a URI shared by N sessions counts N on one rotation. It
	// is the success counterpart to MetricCredentialRefreshFailures: together
	// they make credential rotation observable instead of log-only. The URI is
	// deliberately NOT a dimension (it may carry secrets).
	MetricCredentialRotationApplied = "CredentialRotationApplied"
	// MetricCredentialResolveFailure counts credential repository fetch failures
	// at the resolver choke point (runtime.CredentialResolver.fetch), tagged by
	// the failure's TagKeyCode (e.g. NOT_AUTHORIZED, UNAVAILABLE, NOT_FOUND) so a
	// permission denial is distinguishable from a backend outage. Covers every
	// resolve path — build-time synchronous resolve, uncached rotation polls, and
	// reactive re-resolves — because they all funnel through fetch. The URI is
	// deliberately NOT a dimension (it may carry secrets); only the bounded error
	// code is.
	MetricCredentialResolveFailure = "CredentialResolveFailure"
	// MetricCredentialStaleServed counts stale-while-error serves: the resolver
	// returned an EXPIRED but last-known-good cached CredentialSet because a
	// RETRYABLE fetch error (transient — throttled/timeout/unavailable) prevented
	// refreshing it (runtime.CredentialResolver). Serving stale keeps rebuilds
	// working through a bounded source outage instead of failing hard; a rising
	// value flags a secrets backend that has been unreachable longer than the
	// cache TTL. Never emitted for permanent errors (NOT_FOUND / INVALID_PAYLOAD),
	// which always propagate. The URI is deliberately NOT a dimension.
	MetricCredentialStaleServed = "CredentialStaleServed"
)

// Reconfiguration metric names. Emitted by the bridge Supervisor around live
// config reloads and the degraded state that follows a lost config-change
// stream. These make the previously-invisible degraded config-watch observable
// to operators.
//
// MetricConfigReloads counts live reconfiguration attempts, tagged with the
// outcome (TagKeyState = "success" | "failure"); a rising failure rate flags a
// config that keeps being rejected by the running runtime.
//
// MetricConfigDegraded is a 0/1 gauge that flips to 1 while the bridge's
// configuration machinery is in a degraded state and back to 0 when a reload
// next succeeds (or the degraded condition itself resolves). Two conditions
// raise it, distinguished by the reason surfaced in deep health
// (ConfigWatchHealth.Reason):
//
//   - live reconfiguration is no longer available (the config-change stream
//     closed and the bridge is running blind on its last good config);
//   - a reload was APPLIED but its transport sessions never CONVERGED within
//     the transport's declared activation budget (MQTT-R1: reload success
//     signals are green while the transport cannot reach its broker state —
//     e.g. an ACL-denied topic or rotated-away credentials committed as a
//     successful swap). This clears on its own when the sessions later
//     converge.
//
// It is the single series an operator alerts on to learn a bridge's config
// state needs attention; read /deephealth for which condition and why.
const (
	MetricConfigReloads  = "ConfigReloads"
	MetricConfigDegraded = "ConfigDegraded"
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
	// TagKeyProcessor dimensions a filter-drop counter by the processor that
	// discarded the message. Processor names are operator-defined and bounded,
	// so cardinality stays low.
	TagKeyProcessor = "processor"
	// TagKeyCode dimensions a failure counter by the BridgeError code that
	// classified it (e.g. NOT_AUTHORIZED, UNAVAILABLE, NOT_FOUND). Used by the
	// credential resolve-failure counter so a permission denial is separable
	// from a backend outage. ErrorCode is a bounded enum, so cardinality stays
	// low; never dimension by a free-form URI or message that may carry secrets.
	TagKeyCode = "code"
)
