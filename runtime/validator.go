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
// the record. These are exactly the fields the drainer send path reads from
// d.policy (SendTimeout, MaxReplayAttempts, OnExpired; poisonMinAge derives from
// SendTimeout). Ingress-only fields (MaxInFlight, DepthCacheTTL, ...) are
// excluded so routes that legitimately differ only on ingress behaviour are not
// rejected for sharing a session.
type drainRelevantPolicy struct {
	sendTimeout       time.Duration
	maxReplayAttempts int
	onExpired         routing.ExpiredAction
}

func drainRelevant(p routing.RoutePolicy) drainRelevantPolicy {
	return drainRelevantPolicy{
		sendTimeout:       p.SendTimeout,
		maxReplayAttempts: p.MaxReplayAttempts,
		onExpired:         p.OnExpired,
	}
}

// validateSharedOutboxPartitions rejects a configuration where two or more
// shared_outbox routes drain the SAME session partition with divergent
// drain-relevant policy (finding 17). A session partition has exactly one
// drainer — the first route to claim it wins (bridge_start drainerSessions
// guard) — so the other routes' records would be silently drained under the
// first route's SendTimeout / MaxReplayAttempts / OnExpired. Rather than let
// that config bleed happen invisibly, fail fast at validation and tell the
// operator to either align the policies or give the routes separate sessions.
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
						"but have divergent drain policy (SendTimeout/MaxReplayAttempts/OnExpired); "+
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

	validateRetryFallback(ve, prefix, entry, hasDLQStore)
	validateTerminalFailureSink(ve, prefix, policy, hasDLQStore)
	validateTimeouts(ve, prefix, entry)
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
	// (T11) where one instance ingests and persists while a different instance
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
func validateTimeouts(ve *ValidationError, prefix string, entry *routeEntry) {
	policy := entry.config.Policy.WithDefaults()
	vis := entry.config.SourceVisibilityTimeout
	if !entry.config.SourceAutoExtend && vis > 0 && policy.SendTimeout >= vis/2 {
		ve.add(prefix + fmt.Sprintf(
			"SendTimeout (%s) >= VisibilityTimeout/2 (%s); "+
				"source may redeliver before send completes",
			policy.SendTimeout, vis/2))
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
