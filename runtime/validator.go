package runtime

import (
	"fmt"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
)

// ValidationError collects one or more route validation failures.
type ValidationError struct {
	errors []string
}

func (e *ValidationError) Error() string {
	return strings.Join(e.errors, "; ")
}

func (e *ValidationError) add(msg string) {
	e.errors = append(e.errors, msg)
}

func (e *ValidationError) err() error {
	if len(e.errors) == 0 {
		return nil
	}
	return e
}

// Errors returns a copy of the collected error messages.
func (e *ValidationError) Errors() []string {
	cp := make([]string, len(e.errors))
	copy(cp, e.errors)
	return cp
}

// validateRoutes checks all registered route entries for configuration
// correctness before the runtime starts. It returns a ValidationError
// containing all detected problems, or nil when all routes are valid.
func validateRoutes(entries []*routeEntry, hasOutboxStore, hasLeaseStore, hasDLQStore bool) error {
	ve := &ValidationError{}

	for _, entry := range entries {
		validateRoute(ve, entry, hasOutboxStore, hasLeaseStore, hasDLQStore)
	}

	validateSharedOutboxPartitions(ve, entries)

	return ve.err()
}

// drainRelevantPolicy is the subset of RoutePolicy a shared_outbox drainer
// applies to EVERY record in its partition, independent of which route produced
// the record. These are exactly the fields the drainer send/terminal path reads
// from d.policy:
//
//   - SendTimeout          — bounds each send + the Complete window; also derives
//     the poison min-age fallback (max(5×SendTimeout, 2m)).
//   - MaxReplayAttempts    — the COUNT half of the poison gate.
//   - ReplayBudget         — the wall-clock AGE half of the poison gate
//     (replayBudgetExhausted); a record is only poisoned once BOTH halves hold.
//   - OnExpired            — DLQ vs drop for an expired record.
//   - OnPermanentFailure   — DLQ vs DROP for a permanent send failure OR a
//     poisoned record. THIS is the field: a single per-partition
//     drainer bakes in one OnPermanentFailure, so a record persisted by a
//     dlq-policy route but drained under a drop-policy route is DROPPED with no
//     DLQ evidence after the source was already ACKed — silent message loss.
//
// All five are drain-branching fields; leaving any one out lets two routes with
// divergent behaviour share one partition's single drainer, so the second
// route's records are silently settled under the first route's policy. Ingress-
// only fields (MaxInFlight, DepthCacheTTL, ...) are excluded so routes that
// legitimately differ only on ingress behaviour are not rejected for sharing a
// session. Compare over the WithDefaults-normalized policy so two routes that
// both leave a field unset (same default) are NOT falsely rejected.
type drainRelevantPolicy struct {
	sendTimeout        time.Duration
	maxReplayAttempts  int
	replayBudget       time.Duration
	onExpired          routing.ExpiredAction
	onPermanentFailure routing.FailureAction
}

func drainRelevant(p routing.RoutePolicy) drainRelevantPolicy {
	return drainRelevantPolicy{
		sendTimeout:        p.SendTimeout,
		maxReplayAttempts:  p.MaxReplayAttempts,
		replayBudget:       p.ReplayBudget,
		onExpired:          p.OnExpired,
		onPermanentFailure: p.OnPermanentFailure,
	}
}

// validateSharedOutboxPartitions rejects a configuration where two or more
// shared_outbox routes drain the SAME session partition with divergent
// drain-relevant policy (finding 17 +). A session partition has
// exactly one drainer — the first route to claim it wins (bridge_start
// drainerSessions guard) — so the other routes' records would be silently
// drained under the first route's SendTimeout / MaxReplayAttempts / ReplayBudget
// / OnExpired / OnPermanentFailure. The OnPermanentFailure case is the
// message-loss hazard: a record persisted by a dlq-policy route,
// source-ACKed after Persist, then drained (and permanently-failed or poisoned)
// under a drop-policy route is Completed with NO DLQ entry — the source delivery
// is gone and the DLQ evidence the operator configured is lost. Rather than let
// that config bleed happen invisibly, fail fast at validation and tell the
// operator to either align the policies or give the routes separate sessions.
// Start's checkSharedOutboxDrainerConflicts enforces the same (plus sender
// identity and drain tuning); this earlier, side-effect-free check keeps the
// operator-facing ValidateRoutes consistent with Start.
func validateSharedOutboxPartitions(ve *ValidationError, entries []*routeEntry) {
	type claim struct {
		routeID string
		drain   drainRelevantPolicy
	}
	// Effective drain session -> the first route that claimed it.
	claimed := make(map[string]claim)

	for _, entry := range entries {
		policy := entry.config.Policy.WithDefaults()
		if policy.DeliveryMode != routing.DeliverySharedOutbox {
			continue
		}
		dr := drainRelevant(policy)

		routeSession := ""
		if entry.sessCfg != nil {
			routeSession = entry.sessCfg.SessionID
		}

		// Distinct effective sessions this route persists records under. An
		// empty binding session inherits the route session (mirrors the
		// inheritance bridge_start applies before creating drainers).
		seen := make(map[string]bool)
		for _, b := range entry.config.Bindings {
			eff := b.SessionID
			if eff == "" {
				eff = routeSession
			}
			if eff == "" || seen[eff] {
				continue
			}
			seen[eff] = true
			prev, ok := claimed[eff]
			if !ok {
				claimed[eff] = claim{routeID: entry.config.ID, drain: dr}
				continue
			}
			if prev.drain != dr {
				ve.add(fmt.Sprintf(
					"shared_outbox partition conflict: routes %q and %q both drain session %q "+
						"but have divergent drain policy "+
						"(SendTimeout/MaxReplayAttempts/ReplayBudget/OnExpired/OnPermanentFailure); "+
						"the partition has a single drainer, so one route's records would be drained "+
						"under the other's policy — give them separate sessions or align their policy",
					prev.routeID, entry.config.ID, eff))
			}
		}
	}
}

func validateRoute(ve *ValidationError, entry *routeEntry, hasOutboxStore, hasLeaseStore, hasDLQStore bool) {
	cfg := entry.config
	policy := cfg.Policy.WithDefaults()
	prefix := fmt.Sprintf("route %q: ", cfg.ID)

	switch policy.DeliveryMode {
	case routing.DeliveryDirectHold:
		validateDirectHold(ve, prefix, entry, policy)
	case routing.DeliverySharedOutbox:
		validateSharedOutbox(ve, prefix, entry, policy, hasOutboxStore, hasLeaseStore)
	}

	validateZeroPlanResolver(ve, prefix, entry)
	validateRetryFallback(ve, prefix, entry, hasDLQStore)
	validateTerminalFailureSink(ve, prefix, policy, hasDLQStore)
	validateTimeouts(ve, prefix, entry, hasDLQStore)
	validateBackoff(ve, prefix, policy)
}

// validateZeroPlanResolver rejects a StaticResolver whose cardinality is fixed
// at ZERO, for ANY delivery mode. Such a resolver is a statically-knowable DEAD
// config: it can never yield a dispatch plan, so every message either strands
// (shared_outbox: zero outbox records persisted then the source is ACKed —
// silent loss) or retry-poisons forever (direct_hold: the resolvePlans
// choke-point guard refuses to settle with zero delivery). Fail fast at
// registration rather than per-message at runtime, mirroring the direct_hold
// PlanCount()>1 rejection. The mode-specific PlanCount()>1 rules stay where they
// are (>1 is legal fan-out for shared_outbox, illegal for direct_hold's single leg).
func validateZeroPlanResolver(ve *ValidationError, prefix string, entry *routeEntry) {
	if sr, ok := entry.config.Resolver.(*StaticResolver); ok && sr.PlanCount() == 0 {
		ve.add(prefix + "invalid: static resolver yields 0 dispatch plans; the route can never " +
			"produce a delivery (every message would strand or retry-poison); configure a resolver " +
			"that yields at least one plan")
	}
}

func validateDirectHold(ve *ValidationError, prefix string, entry *routeEntry, policy routing.RoutePolicy) {
	if policy.DispatchMode == routing.DispatchFanOut {
		ve.add(prefix + "direct_hold invalid: resolver fan-out is enabled")
	}

	if entry.sessCfg != nil && entry.sessCfg.Exclusive {
		ve.add(prefix + "direct_hold invalid: target session requires lease handoff")
	}

	if !hasCapability(entry.config.SourceCapabilities, ports.CapVisibilityExtension) &&
		!hasCapability(entry.config.SourceCapabilities, ports.CapHTTPEndpoint) {
		ve.add(prefix + "direct_hold invalid: source does not support visibility extension")
	}

	// Multiple bindings are allowed when a resolver is configured for
	// content-based single dispatch. Without a resolver, multiple
	// bindings are ambiguous.
	if len(entry.config.Bindings) > 1 && entry.config.Resolver == nil {
		ve.add(prefix + "direct_hold invalid: multiple bindings require a resolver for content-based dispatch")
	}

	if !policy.AllowUnfenced && hasCapability(entry.config.SourceCapabilities, ports.CapSharedConsumer) {
		ve.add(prefix + "direct_hold invalid: shared consumer source requires fencing (use shared_outbox) or set AllowUnfenced")
	}

	// Finding 4: direct_hold dispatches a SINGLE leg (DispatchSingle). A
	// resolver that yields more than one plan has its extra plans silently
	// discarded at runtime (only plans[0] is sent). We cannot in general know
	// how many plans an arbitrary resolver returns before it runs, but a
	// StaticResolver's cardinality is fixed and knowable now — reject it
	// statically so the misconfiguration surfaces before Start rather than as a
	// runtime permanent-failure per message.
	if sr, ok := entry.config.Resolver.(*StaticResolver); ok && sr.PlanCount() > 1 {
		ve.add(prefix + fmt.Sprintf(
			"direct_hold invalid: static resolver yields %d plans but direct_hold dispatches a "+
				"single leg; the extra plans would be discarded (use shared_outbox for fan-out or "+
				"configure a single-plan resolver)",
			sr.PlanCount()))
	}
}

// outboxTransactionLimit is the maximum number of records atomically
// persisted in a single OutboxStore.Persist call (DynamoDB BatchWriteItem).
const outboxTransactionLimit = 100

func validateSharedOutbox(ve *ValidationError, prefix string, entry *routeEntry, policy routing.RoutePolicy, hasOutboxStore, hasLeaseStore bool) {
	if !hasOutboxStore {
		ve.add(prefix + "shared_outbox invalid: no OutboxStore configured")
	}

	if entry.sessCfg != nil && entry.sessCfg.Exclusive && !hasLeaseStore {
		ve.add(prefix + "shared_outbox invalid: no LeaseStore configured for exclusive session")
	}

	// Finding 11: a non-exclusive session never acquires a lease (only
	// runExclusive sets hasLease), so its outbox drainer's TokenFn reports
	// "not held" on every cycle and the partition NEVER drains — the source is
	// ACKed after persist and the records silently strand. shared_outbox
	// therefore requires an exclusive session (which in turn requires a lease
	// store, checked above). Reject the combo statically; the drainer also
	// surfaces it at runtime via MetricDrainSkippedNoLease + a rate-limited warn.
	if entry.sessCfg != nil && !entry.sessCfg.Exclusive {
		ve.add(prefix + "shared_outbox invalid: session is non-exclusive; a non-exclusive " +
			"session never acquires a lease, so its outbox drainer skips every cycle and " +
			"persisted records never drain (make the session exclusive)")
	}

	if policy.DispatchMode == routing.DispatchFanOut && len(entry.config.Bindings) > outboxTransactionLimit {
		ve.add(prefix + fmt.Sprintf(
			"shared_outbox invalid: fan-out cardinality (%d) exceeds OutboxStore transaction limit (%d)",
			len(entry.config.Bindings), outboxTransactionLimit))
	}

	// Outbox records are keyed by OutboxPartitionKey: SESSION#<sid> when the
	// record has a session, else BINDING#<bid>. Drainers only ever poll SESSION#
	// partitions, in ANY instance — no drainer anywhere reads a BINDING#
	// partition. A record that lands under BINDING# is therefore orphaned in
	// every topology: the source is ACKed after persist and the record is
	// silently lost. The two static, topology-independent ways to produce a
	// BINDING# record are checked below.
	//
	// We deliberately do NOT reject a binding whose (non-empty) session simply
	// has no drainer in THIS runtime: that is the normal cross-instance handoff
	// where one instance ingests and persists while a different instance
	// owns the session lease and drains. Local drainer absence is therefore not
	// proof of orphaning, so it cannot be a Start-time error without breaking
	// cross-instance topologies.
	//
	// ponytail: two residual orphan vectors are not statically decidable here
	// and are left to operator discipline / deployment validation: (1) a resolver
	// that emits a BindingID absent from entry.config.Bindings resolves to an
	// empty session -> BINDING# orphan; (2) a binding session that no instance in
	// the deployment ever drains. Both require whole-deployment knowledge the
	// per-instance validator does not have.

	// (1) A route with no bindings and no resolver dispatches the fallback plan
	// {BindingID: routeID}; routeID matches no binding, so its session resolves
	// to "" -> BINDING#<routeID>, never drained. Reject it.
	if len(entry.config.Bindings) == 0 && entry.config.Resolver == nil {
		ve.add(prefix + "shared_outbox invalid: route has no bindings and no resolver; its " +
			"fallback outbox partition (BINDING#<route>) is never drained and records would " +
			"be silently lost")
	}

	// (2) A binding whose effective session is empty persists under BINDING#<id>.
	// The effective session mirrors the inheritance applied at Start (an empty
	// binding session inherits the route session), which runs after this
	// validation, so replicate it here.
	routeSession := ""
	if entry.sessCfg != nil {
		routeSession = entry.sessCfg.SessionID
	}
	for _, b := range entry.config.Bindings {
		eff := b.SessionID
		if eff == "" {
			eff = routeSession
		}
		if eff == "" {
			ve.add(prefix + fmt.Sprintf(
				"shared_outbox invalid: binding %q has no session_id and the route has no "+
					"session to inherit; its outbox records would persist under an undrained "+
					"partition (BINDING#) and be silently lost (set the binding session_id or "+
					"add a route session)",
				b.ID))
		}
	}

	// ack_after=target_accept is not honored by shared_outbox: the source is
	// ACKed once the outbox record is durably persisted, never after the
	// downstream target accepts. Reading the RAW (pre-WithDefaults) value lets
	// us distinguish an explicit operator choice from the unset default, which
	// WithDefaults coerces to target_accept. Reject the explicit choice rather
	// than silently delivering weaker semantics than requested.
	if entry.config.Policy.AckAfter == routing.AckAfterTargetAccept {
		ve.add(prefix + "shared_outbox invalid: ack_after=target_accept is not honored; " +
			"outbox persist is the durability boundary (use ack_after=outbox_persist or omit it)")
	}
}

// validateTerminalFailureSink ensures a DLQ store exists whenever the effective
// policy routes terminal outcomes (permanent failure or expiry) to the DLQ.
// Without a store, Router.Route no-ops and the source/outbox is settled as if
// the evidence were recorded, silently dropping failed messages. Operators who
// genuinely want to discard such messages must say so explicitly via the drop
// actions.
func validateTerminalFailureSink(ve *ValidationError, prefix string, policy routing.RoutePolicy, hasDLQStore bool) {
	if hasDLQStore {
		return
	}
	if policy.OnPermanentFailure == routing.FailureDLQ {
		ve.add(prefix + "on_permanent_failure=dlq but no DLQ store configured; " +
			"permanent failures would be silently dropped (configure a DLQ store or set on_permanent_failure=drop)")
	}
	if policy.OnExpired == routing.ExpiredDLQ {
		ve.add(prefix + "on_expired=dlq but no DLQ store configured; " +
			"expired messages would be silently dropped (configure a DLQ store or set on_expired=drop)")
	}
	if policy.OnFiltered == routing.FilteredDLQ {
		ve.add(prefix + "on_filtered=dlq but no DLQ store configured; " +
			"filtered messages would be silently dropped (configure a DLQ store or set on_filtered=drop)")
	}
}

// validateRetryFallback checks that routes whose source cannot retry
// (e.g. MQTT) have a DLQ store configured. Without one, failed messages
// are silently dropped. Set AllowRetryDrop to acknowledge this risk.
func validateRetryFallback(ve *ValidationError, prefix string, entry *routeEntry, hasDLQStore bool) {
	policy := entry.config.Policy.WithDefaults()
	if policy.AllowRetryDrop {
		return
	}
	caps := entry.config.SourceCapabilities
	if !hasCapability(caps, ports.CapVisibilityExtension) &&
		!hasCapability(caps, ports.CapSourceRedelivery) &&
		!hasCapability(caps, ports.CapHTTPEndpoint) &&
		!hasDLQStore {
		ve.add(prefix + "source does not support retry/redelivery and no DLQ store configured; " +
			"messages will be silently dropped on failure (set AllowRetryDrop to suppress)")
	}
}

// validateTimeouts checks that SendTimeout does not exceed half the
// source visibility timeout. When send takes longer than the visibility
// window, the source transport redelivers the message while send is
// still in progress, causing duplicates.
//
// The check is skipped when the source auto-extends the window
// (SourceAutoExtend): a background renewal keeps the message invisible
// for the duration of processing, so a deliberately short window is safe
// and must not be rejected. It only guards routes running with a fixed,
// non-renewed window.
func validateTimeouts(ve *ValidationError, prefix string, entry *routeEntry, hasDLQStore bool) {
	policy := entry.config.Policy.WithDefaults()
	vis := entry.config.SourceVisibilityTimeout
	if entry.config.SourceAutoExtend || vis <= 0 {
		// Auto-extend renews the window in the background, and a zero/unknown
		// window is not checkable; either way the fixed-window bound does not apply.
		return
	}

	if policy.SendTimeout >= vis/2 {
		ve.add(prefix + fmt.Sprintf(
			"SendTimeout (%s) >= VisibilityTimeout/2 (%s); "+
				"source may redeliver before send completes",
			policy.SendTimeout, vis/2))
	}

	// Total worst-case time the message can hold the source before it is settled,
	// checked against the fixed visibility window. Per-processor budgets are
	// own-time-only and disarm during next() (route/chain.go), so N compliant
	// processors can legally consume N×ProcessorTimeout before the send even
	// starts; add the send budget and the bounded DLQ-write budget the failure
	// path may spend. When this total exceeds the window the source redelivers
	// mid-pipeline and the message is processed concurrently — the very duplicate
	// this validator exists to prevent — even though every individual timeout
	// passes its own check.
	//
	// dlqWriteBudget is the DLQ-write time the failure path may spend before it
	// settles the source (see the package const). It is counted into the worst
	// case only when a DLQ write is actually reachable: a DLQ store exists AND the
	// terminal policy is not drop. A drop-policy route, or any route in a
	// deployment with no DLQ store, settles the source in-memory on failure, so
	// counting the budget there would over-reject a safe config at startup.
	dlqBudget := time.Duration(0)
	if hasDLQStore && policy.OnPermanentFailure != routing.FailureDrop {
		dlqBudget = dlqWriteBudget
	}
	nProc := len(entry.config.Processors)
	total := time.Duration(nProc)*policy.ProcessorTimeout + policy.SendTimeout + dlqBudget
	if total > vis {
		ve.add(prefix + fmt.Sprintf(
			"worst-case pipeline time (%s = %d processors × ProcessorTimeout %s + SendTimeout %s + DLQ budget %s) "+
				"exceeds source VisibilityTimeout (%s); source may redeliver mid-pipeline causing duplicate processing",
			total, nProc, policy.ProcessorTimeout, policy.SendTimeout, dlqBudget, vis))
	}
}

// dlqWriteBudget is the bounded wall-clock time the inline failure path may spend
// writing a poisoned/permanently-failed message to the DLQ before it settles the
// source. It mirrors the DLQ router wiring in bridge_start.go (WriteTimeout 5s ×
// WriteMaxAttempts 2 plus the router's 500ms default backoff between attempts =
// 10.5s). Kept a local constant because the runtime does not model it as a shared
// value; TestDLQWriteBudget_MatchesRouterWiring pins it to that wiring so either
// side drifting is caught.
const dlqWriteBudget = 10500 * time.Millisecond

// validateBackoff rejects a Backoff block that is not a backoff. WithDefaults
// fills only ZERO fields, so a negative interval or a below-one multiplier
// survives to here. A negative MaxInterval is the dangerous one: route.retryDelay
// only clamps exponential growth behind a `> 0` MaxInterval guard, so a negative
// cap never fires and float64 growth reaches time.Duration(+Inf)
// (implementation-defined, negative on amd64/arm64), feeding Retry a
// negative/near-infinite delay. A multiplier in (0,1) is the inverse defect: it
// makes each retry fire sooner than the last, hammering a failing target at an
// accelerating rate. The checks mirror domain/routing.RoutePolicy.Validate,
// which the runtime start path does not call.
func validateBackoff(ve *ValidationError, prefix string, policy routing.RoutePolicy) {
	if policy.Backoff.InitialInterval < 0 {
		ve.add(prefix + fmt.Sprintf("Backoff.InitialInterval (%s) must not be negative", policy.Backoff.InitialInterval))
	}
	if policy.Backoff.MaxInterval < 0 {
		ve.add(prefix + fmt.Sprintf("Backoff.MaxInterval (%s) must not be negative", policy.Backoff.MaxInterval))
	}
	if policy.Backoff.Multiplier != 0 && policy.Backoff.Multiplier < 1 {
		ve.add(prefix + fmt.Sprintf(
			"Backoff.Multiplier (%g) must be >= 1; below 1 accelerates retries instead of backing off",
			policy.Backoff.Multiplier))
	}
	if policy.Backoff.JitterFactor != routing.JitterDisabled &&
		(policy.Backoff.JitterFactor < 0 || policy.Backoff.JitterFactor > 1) {
		ve.add(prefix + fmt.Sprintf(
			"Backoff.JitterFactor (%g) must be a fraction in [0,1] or routing.JitterDisabled",
			policy.Backoff.JitterFactor))
	}
}

func hasCapability(caps []ports.Capability, target ports.Capability) bool {
	for _, c := range caps {
		if c == target {
			return true
		}
	}
	return false
}
