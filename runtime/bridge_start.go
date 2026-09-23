package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/cluster"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// ValidateRoutes runs pre-start route validation and returns a
// [ValidationError] describing every problem found, or nil when all routes are
// valid. It is idempotent and side-effect-free (it never mutates route entries
// or runtime state), so it is safe to call repeatedly and BEFORE Start — the
// builder's complete() calls it at the end of construction. Start invokes
// the same validation internally, so a runtime that fails ValidateRoutes also
// fails Start.
func (rt *Runtime) ValidateRoutes() error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return validateRoutes(rt.entries, rt.outboxStore != nil, rt.leaseStore != nil, rt.dlqStore != nil)
}

// dlqDepthSampleInterval is the cadence at which the runtime samples the
// standing dead-letter-queue backlog (shared.MetricDLQDepth). Unlike OutboxDepth
// — sampled every drain cycle by the drainer loop — the DLQ has no loop of its
// own, so without this periodic sampler a stale post-burst backlog (writes
// stopped, nothing redriven) is invisible until a manual storage scan. 30s aligns with the 30–60s operational alarm cadence; it is a
// const, not a config knob, since no DLQ-sampling knob exists in the blueprint.
const dlqDepthSampleInterval = 30 * time.Second

// Start wires up all registered routes, session managers, and outbox
// drainers, then spawns background goroutines. It returns immediately;
// use Stop to shut down gracefully.
func (rt *Runtime) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.terminal || rt.stopped || rt.fenced {
		// Stop closes the outbox/DLQ/lease stores and cancels every
		// drainer/manager, but the drainers/managers/entries are never rebuilt.
		// A restart would append fresh drainers over CLOSED stores and duplicate
		// work. The runtime is single-use — for BOTH a clean deliberate Stop
		// (rt.stopped) and an unrecoverable component failure (rt.terminal):
		// reject a restart with a clear error. "Resume" after a deliberate stop
		// means the supervisor builds a NEW runtime.
		return errors.New("runtime: cannot start a stopped runtime (single-use lifecycle); build a new runtime")
	}

	if rt.running {
		return errors.New("runtime already running")
	}
	rt.running = true
	rt.healthy = true
	rt.terminal = false
	rt.componentErrors = make(map[string]error)
	rt.routeFlaps = make(map[string]int)
	rt.routeRunStart = make(map[string]time.Time)
	rt.sessionRuns = make(map[string]componentRun)
	rt.exclusiveSessions = make(map[string]bool)

	if err := validateRoutes(rt.entries, rt.outboxStore != nil, rt.leaseStore != nil, rt.dlqStore != nil); err != nil {
		rt.running = false
		return err
	}

	// Shared_outbox drainer config bleed. Exactly one drainer exists per
	// outbox session partition, so if two DIFFERENT routes resolve to the same
	// session with divergent sender or drain/replay/DLQ policy, one route's
	// records would silently drain under the other's configuration. Detect that
	// BEFORE any wiring/goroutine spawns and fail fast, rather than warning and
	// running with a data-integrity hazard.
	if err := rt.checkSharedOutboxDrainerConflicts(); err != nil {
		rt.running = false
		return err
	}

	if logging.DebugEnabled(rt.logger) {
		rt.logger.Log(ctx, logging.LevelDebug, "runtime starting",
			"instance_id", rt.instanceID,
			"route_count", len(rt.entries),
			"session_count", len(rt.sessionMgrs)+len(rt.sessionSenders)+len(rt.ingressSessions),
		)
	}

	// Routes, receivers, senders, session managers and drainers all derive their
	// context from the one built here. It is DETACHED from the caller's context
	// (values preserved, cancellation dropped) so Stop is the ONLY thing that can
	// cancel in-flight work.
	//
	// Both shipped binaries cancel the context they passed to Start when they
	// receive SIGTERM, and only then ask the supervisor/app to stop the runtime.
	// While the work context was derived from the caller's, that cancel reached
	// every in-flight send/persist/processor first, so Stop's "settle accepted
	// deliveries before cancelling" phase never ran: sends aborted, sources
	// redelivered, and every rolling restart produced duplicates (or, under a
	// drop policy, loss). Detaching moves teardown entirely onto Stop, which is
	// bounded by the configured drain budget.
	//
	// Cancelling the Start context is still a valid way to shut a runtime down —
	// the watcher below turns it into a Stop.
	stopSignal := ctx.Done()
	ctx, rt.cancel = context.WithCancel(context.WithoutCancel(ctx))
	rt.workCtx = ctx

	// Watch the caller-supplied Start context. If it is cancelled WITHOUT a Stop
	// (the caller cancels the ctx it passed to Start, rather than calling Stop),
	// nothing tears the runtime down on its own any more — the work context is
	// detached — so running/healthy would stay advertised on a runtime nobody is
	// going to stop. Drive Stop so resources are released and health flips. A
	// Stop already under way sets stopped/terminal, which this observes so there
	// is no double teardown. The watcher is deliberately NOT in rt.wg (Stop waits
	// on rt.wg, so enrolling it would deadlock); it always terminates, because
	// either the caller's context ends or rt.cancel closes the work context, and
	// whichever comes first releases the select.
	watchDone := ctx.Done()
	go func() {
		select {
		case <-stopSignal:
		case <-watchDone:
			// Stop (or a component-failure trip) cancelled the work context: the
			// teardown is already owned by that caller.
			return
		}
		rt.mu.Lock()
		stopping := rt.terminal || !rt.running
		rt.mu.Unlock()
		if stopping {
			return
		}
		// The budget the builder derives from bridge.drain_timeout — the same
		// ceiling the supervisor gives its own stopCurrent, so whichever of the
		// two wins the race performs an identically-bounded teardown. The 5s
		// fallback applies only to a hand-wired runtime that set no budget.
		stopBudget := rt.shutdownTimeout
		if stopBudget <= 0 {
			stopBudget = 5 * time.Second
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), stopBudget)
		defer cancel()
		_ = rt.Stop(stopCtx)
	}()

	// The DLQ write is on the settle-critical path: it runs inside the route
	// runner's per-delivery goroutine, which holds the global in-flight slot
	// until it returns. Keep the budget small and well under typical source
	// visibility so a degraded DLQ store cannot pin global slots for the full
	// 30s×3 default and starve healthy routes. Worst case here is ~10.5s.
	//
	// With the default transports this budget does not affect duplicates: SQS
	// AutoExtend and ASB lock renewal keep the source message invisible for the
	// whole blocking write, so a hung store yields a clean NACK-and-retry, not a
	// duplicate DLQ entry. Duplicate amplification only arises if a source runs
	// with visibility extension OFF and a visibility window shorter than the
	// send+DLQ budget — a deployment that must size visibility accordingly.
	//
	// ponytail: fixed 2×5s budget; make it per-route visibility-aware only if a
	// route both disables visibility extension and sets a visibility window below
	// the send+DLQ budget.
	m := rt.metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}

	rt.dlqRouter = dlq.NewFromConfig(dlq.Config{
		Store:            rt.dlqStore,
		Clock:            rt.clk,
		WriteTimeout:     dlq.RuntimeWriteTimeout,
		WriteMaxAttempts: dlq.RuntimeWriteMaxAttempts,
		// wire Metrics/Logger so MetricDLQWriteFailures reaches the real
		// exporter (not a NoopExporter) and router write errors are logged.
		// Without these the production DLQ router was blind.
		Metrics: m,
		Logger:  rt.logger,
	})

	if rt.globalMaxInFlight > 0 {
		rt.globalSem = make(chan struct{}, rt.globalMaxInFlight)
	}

	if rt.leaseStore != nil {
		locatorCfg := cluster.DefaultLocatorConfig()
		// Ownership-unknown decisions are advisory (no token is minted), so the
		// reason-tagged counter is the ONLY trace of fleet clock skew or a
		// cold-takeover window behind a 502/503.
		locatorCfg.Metrics = m
		rt.locator = cluster.NewLocator(rt.leaseOwnerID, rt.leaseStore, locatorCfg, rt.clk)
	}

	// Installed always, not only when this pass creates a manager: routes and
	// sessions added to the running runtime later write through the same
	// router, and dlqToken reads the live managers under rt.mu.
	rt.dlqRouter.SetTokenFn(rt.dlqToken)

	rt.startComponentsLocked(componentSet{
		entries:         rt.entries,
		sessionSenders:  rt.sessionSenders,
		ingressSessions: rt.ingressSessions,
	})

	// DLQ-depth sampler: periodically emit the standing DLQ backlog as
	// shared.MetricDLQDepth so operators can alarm on records sitting in the DLQ
	// after traffic stops. Unlike OutboxDepth (sampled every drain cycle by the
	// drainer loop) the DLQ has no loop of its own. The goroutine probes the
	// OPTIONAL ports.DLQDepthReporter capability ONCE: a store that does not
	// implement it (ok == false, err == nil) needs no ticker and the goroutine
	// returns immediately — no pointless loop; a transient probe error keeps
	// sampling, because the capability is present and only the backend was
	// briefly unavailable. The loop NEVER returns a non-ctx error, so a transient
	// sample failure cannot trip startBackground's terminal escalation — it logs
	// and retries next interval. It uses rt.clk so a fake clock drives it in
	// tests with no real sleep, and omits tags for the dimensionless fleet total
	// the metric documents. The probe runs inside the goroutine (not under
	// rt.mu) so a slow store never blocks Start under the runtime lock.
	if rt.dlqStore != nil {
		rt.startBackground(ctx, "dlq-depth-sampler", func(ctx context.Context) error {
			// Probe the OPTIONAL capability ONCE with a nil exporter: this is a
			// pure capability check, not an emission — the periodic loop below
			// owns every shared.MetricDLQDepth gauge so the series is driven
			// strictly by the sample cadence.
			if _, ok, probeErr := ReportDLQDepth(ctx, rt.dlqStore, nil); !ok && probeErr == nil {
				return nil
			}
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-rt.clk.After(dlqDepthSampleInterval):
					if _, _, err := ReportDLQDepth(ctx, rt.dlqStore, rt.metrics); err != nil && rt.logger != nil {
						rt.logger.Warn("dlq-depth sample failed; will retry next interval", "error", err)
					}
				}
			}
		})
	}

	return nil
}

// drainerFingerprint captures the drain-relevant inputs a shared_outbox drainer
// for one session partition is built from — the fields whose divergence between
// two routes sharing that partition would silently change how the SECOND route's
// records are drained. It is deliberately restricted to value-typed, directly
// comparable fields so two fingerprints compare with ==. The sender is compared
// separately (see sendersConflict) because a Sender is an interface identity, not
// a value. DrainStrategy is intentionally excluded: it is an interface (pointer)
// whose identity cannot distinguish "equivalent but separately constructed" from
// "genuinely different", so comparing it here would raise false conflicts; a
// diverging poll cadence is the accepted residual (records still drain, only the
// cadence follows the first route).
type drainerFingerprint struct {
	onPermanentFailure    routing.FailureAction
	onExpired             routing.ExpiredAction
	maxReplayAttempts     int
	replayBudget          time.Duration
	sendTimeout           time.Duration
	drainBatchSize        int
	drainMaxBatchSize     int
	drainMaxConcurrency   int
	perRecordDrainTimeout time.Duration
	maxDrainTimeout       time.Duration
}

// drainerBinding is a per-route claim on a session partition's single drainer.
type drainerBinding struct {
	routeID string
	sender  ports.Sender
	fp      drainerFingerprint
}

// drainFingerprint derives the fingerprint from a route's (defaulted) policy and
// the session config the drainer's tuning comes from. It mirrors the fields read
// off Policy/sessCfg at the two drainer-construction sites in
// wireRouteEntriesLocked.
//
// The drain-tuning fields are NORMALIZED to the values outbox.New would store,
// not compared raw: outbox.New defaults a zero DrainBatchSize/DrainMaxBatchSize/
// DrainMaxConcurrency/DrainTimeout to fixed values, so two effectively-identical
// session configs that merely differ in "left zero" vs "spelled out the default"
// would otherwise fingerprint differently and raise a false conflict that blocks
// a valid Start. Normalizing here mirrors runtime/outbox/drainer.go New()'s
// defaulting exactly (see normalizeDrain* below) so semantically-equal configs
// fingerprint equal.
//
// PerRecordDrainTimeout and MaxDrainTimeout are resolved via
// normalizeScaledDrainTimeouts, which mirrors outbox.ComputeBatchDeadline: when
// BOTH are zero the drainer is in legacy (fixed DrainTimeout) mode and the 0/0
// pair is kept RAW so a legacy route never fingerprints equal to a scaled one;
// once EITHER is set the drainer is in scaled mode, where a zero PerRecord/Max is
// defaulted (3s/10s) at compute time — so two scaled routes differing only
// zero-vs-spelled-out-default share an effective per-batch deadline and MUST
// fingerprint equal.
func drainFingerprint(p routing.RoutePolicy, sc session.Config) drainerFingerprint {
	batch := normalizeDrainBatchSize(sc.DrainBatchSize)
	per, maxT := normalizeScaledDrainTimeouts(sc.PerRecordDrainTimeout, sc.MaxDrainTimeout)
	return drainerFingerprint{
		onPermanentFailure:    p.OnPermanentFailure,
		onExpired:             p.OnExpired,
		maxReplayAttempts:     p.MaxReplayAttempts,
		replayBudget:          p.ReplayBudget,
		sendTimeout:           p.SendTimeout,
		drainBatchSize:        batch,
		drainMaxBatchSize:     normalizeDrainMaxBatchSize(sc.DrainMaxBatchSize, batch),
		drainMaxConcurrency:   normalizeDrainMaxConcurrency(sc.DrainMaxConcurrency),
		perRecordDrainTimeout: per,
		maxDrainTimeout:       maxT,
	}
}

// The following normalize* helpers mirror runtime/outbox/drainer.go New()'s
// inline defaulting for the drain-tuning fields. They are the source of truth's
// counterpart: if outbox.New changes a default, update these to match. (They are
// duplicated rather than reused because outbox.New performs the defaulting inline
// on its private Config and exposes no normalize/WithDefaults helper to call.)
const drainAbsoluteMaxBatchSize = 10000 // mirrors outbox absoluteMaxBatchSize

// Mirrors runtime/outbox drainer.go defaults (duplicated per the note above).
const (
	drainDefaultPerRecordTimeout = 3 * time.Second  // mirrors outbox defaultPerRecordDrainTimeout
	drainDefaultMaxTimeout       = 10 * time.Second // mirrors outbox defaultMaxDrainTimeout
)

// normalizeScaledDrainTimeouts mirrors outbox.ComputeBatchDeadline's resolution
// of the scaled drain-timeout pair. When BOTH are zero the drainer is in legacy
// (fixed DrainTimeout) mode, so the 0/0 pair is preserved verbatim to keep
// legacy and scaled configs distinct in the fingerprint. Once EITHER is set the
// drainer is in scaled mode, where ComputeBatchDeadline defaults a zero PerRecord
// to 3s and a zero Max to 10s at compute time — so two scaled routes differing
// only zero-vs-spelled-out-default share an effective deadline and MUST
// fingerprint equal.
func normalizeScaledDrainTimeouts(per, maxT time.Duration) (time.Duration, time.Duration) {
	if per == 0 && maxT == 0 {
		return 0, 0 // legacy mode: preserve raw so legacy != scaled
	}
	if per == 0 {
		per = drainDefaultPerRecordTimeout
	}
	if maxT == 0 {
		maxT = drainDefaultMaxTimeout
	}
	return per, maxT
}

func normalizeDrainBatchSize(v int) int {
	if v <= 0 {
		v = 100
	}
	return min(v, drainAbsoluteMaxBatchSize)
}

func normalizeDrainMaxBatchSize(v, batch int) int {
	if v <= 0 {
		v = 500
	}
	v = min(v, drainAbsoluteMaxBatchSize)
	return max(v, batch)
}

func normalizeDrainMaxConcurrency(v int) int {
	if v <= 0 {
		return 10
	}
	return v
}

// sendersConflict reports whether two senders are DIFFERENT instances. Sender
// identity is the correct check here: a partition drains through exactly one
// drainer wired with exactly one Sender, so two distinct Sender objects on one
// partition genuinely means one route's records take the other's send path —
// there is no "equivalent sender" escape hatch. A non-comparable dynamic type
// (which would panic on ==) is conservatively treated as a conflict so a config
// bleed fails fast rather than slipping through a recovered panic.
func sendersConflict(a, b ports.Sender) (conflict bool) {
	defer func() {
		if recover() != nil {
			conflict = true
		}
	}()
	return a != b
}

// checkSharedOutboxDrainerConflicts fails fast when two DIFFERENT shared_outbox
// routes resolve to the same outbox session partition with divergent sender or
// drain/replay/DLQ policy. Exactly one drainer is built per partition (keyed
// SESSION#<id> by OutboxPartitionKey), so such divergence would silently drain
// the second route's records under the first route's sender and policy — records
// sent via the wrong sender, or poisoned under the wrong replay budget/DLQ
// policy. It walks entries in the SAME order and resolves the SAME session→config
// mapping as the drainer-construction loop in wireRouteEntriesLocked, so what it
// validates is exactly what would be built. Called under rt.mu before any wiring; caller
// resets rt.running on error.
func (rt *Runtime) checkSharedOutboxDrainerConflicts() error {
	if rt.outboxStore == nil {
		return nil
	}

	owners := make(map[string]drainerBinding)
	claim := func(sid string, b drainerBinding) error {
		prev, exists := owners[sid]
		if !exists {
			owners[sid] = b
			return nil
		}
		if prev.routeID == b.routeID {
			// The same route re-referencing its own session partition (e.g. a
			// binding that inherited the route's primary session) shares that
			// route's single drainer — no cross-route bleed.
			return nil
		}
		if prev.fp == b.fp && !sendersConflict(prev.sender, b.sender) {
			// Identical drain config AND the same sender instance: the single
			// shared drainer is correct for both routes, no bleed.
			return nil
		}
		return fmt.Errorf("runtime: shared_outbox routes %q and %q both target outbox session partition %q but resolve to divergent sender or drain/replay/DLQ policy; a session partition drains through exactly one drainer, so the second route's records would silently drain under the first route's sender and policy — align their sender and drain policy, or give them distinct sessions",
			prev.routeID, b.routeID, sid)
	}

	for _, entry := range rt.entries {
		if entry.config.Policy.DeliveryMode != routing.DeliverySharedOutbox {
			continue
		}
		p := entry.config.Policy.WithDefaults()

		// Site 1: the route's primary session (matches the primary-session
		// drainer built in wireRouteEntriesLocked when entry.session != nil &&
		// entry.sessCfg != nil).
		if entry.session != nil && entry.sessCfg != nil {
			b := drainerBinding{routeID: entry.config.ID, sender: entry.sender, fp: drainFingerprint(p, *entry.sessCfg)}
			if err := claim(entry.sessCfg.SessionID, b); err != nil {
				return err
			}
		}

		// Site 2: fan-out target sessions referenced by bindings (matches the
		// per-binding drainer wireRouteEntriesLocked builds from the session
		// senders).
		for _, binding := range entry.config.Bindings {
			sid := binding.SessionID
			if sid == "" && entry.sessCfg != nil {
				// Mirror the wiring's binding-session inheritance: a shared_outbox
				// binding that omits its SessionID inherits the route's primary
				// session.
				sid = entry.sessCfg.SessionID
			}
			if sid == "" {
				continue
			}
			sse, ok := rt.sessionSenders[sid]
			if !ok {
				// No standalone sender for this sid: either it is the route's own
				// primary session (already claimed at site 1 for this route) or it
				// is not drainer-backed here. The wiring likewise skips it.
				continue
			}
			b := drainerBinding{routeID: entry.config.ID, sender: sse.sender, fp: drainFingerprint(p, sse.config)}
			if err := claim(sid, b); err != nil {
				return err
			}
		}
	}
	return nil
}
