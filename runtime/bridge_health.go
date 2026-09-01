package runtime

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// InstanceID returns the bridge instance identifier.
func (rt *Runtime) InstanceID() string {
	return rt.instanceID
}

// IsRunning reports whether the runtime is currently running and healthy.
// Returns false if any critical background component has failed.
func (rt *Runtime) IsRunning() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.running && rt.healthy
}

// Healthy reports whether all background components are healthy.
func (rt *Runtime) Healthy() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.healthy
}

// Terminal reports whether the runtime has suffered an unrecoverable
// failure: a background component returned a fatal error or panicked,
// causing the runtime to cancel itself. Unlike a clean Stop (which leaves
// healthy true), a terminal runtime never recovers on its own — its
// supervisor/orchestrator must restart the process. The liveness probe
// uses this to fail closed so Kubernetes restarts a dead-but-running pod.
func (rt *Runtime) Terminal() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.terminal
}

// ComponentErrors returns a copy of the failed component error map.
func (rt *Runtime) ComponentErrors() map[string]error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return maps.Clone(rt.componentErrors)
}

// DLQReader returns the DLQ read port if a DLQ store is configured, or
// nil. It satisfies the read-side ports.RuntimeQuery driving port.
func (rt *Runtime) DLQReader() ports.DLQReader {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.dlqStore == nil {
		return nil
	}
	return rt.dlqStore
}

// DLQAdmin returns the DLQ admin (write/destructive) port if a DLQ
// store is configured, or nil. It satisfies the write-side
// ports.RuntimeCommand driving port. The runtime holds a single
// DLQStore that satisfies both halves; the read/admin split is a
// port-boundary contract, not two physical stores.
func (rt *Runtime) DLQAdmin() ports.DLQAdmin {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.dlqStore == nil {
		return nil
	}
	return rt.dlqStore
}

// OutboxPending reports the number of PENDING outbox records currently held for
// partitionKey, using the OPTIONAL ports.OutboxDepthReporter capability the
// workstream added (forwarded through runtime.InstrumentedOutboxStore).
//
// ok is true ONLY when the depth was actually proven: the store is configured
// AND implements the depth-report capability AND the query succeeded. It is
// false — with err nil — when no outbox store is configured or the store does
// not implement the capability (ErrOutboxDepthUnsupported). A real backend
// failure returns ok=false with a non-nil err.
//
// Callers that must decide whether a durable partition is safe to strand (the
// supervisor's destructive-reload preflight) treat "not ok" as UNPROVEN and
// fail closed: an unprovable depth is never assumed empty.
func (rt *Runtime) OutboxPending(ctx context.Context, partitionKey string) (n int, ok bool, err error) {
	rt.mu.Lock()
	store := rt.outboxStore
	rt.mu.Unlock()
	if store == nil {
		return 0, false, nil
	}
	reporter, isReporter := store.(ports.OutboxDepthReporter)
	if !isReporter {
		return 0, false, nil
	}
	count, err := reporter.CountPending(ctx, partitionKey)
	if err != nil {
		if errors.Is(err, ports.ErrOutboxDepthUnsupported) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return count, true, nil
}

// LeaseStatus returns the lease ownership status for each exclusive
// session. The map keys are session IDs; values are true when the
// session holds the lease (active) and false otherwise (standby).
// Non-exclusive sessions are not included.
func (rt *Runtime) LeaseStatus() map[string]bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	status := make(map[string]bool)
	for id, mgr := range rt.sessionMgrs {
		_, held := mgr.Token()
		status[id] = held
	}
	return status
}

// Role classification strings returned by Role / DeepHealth.Role.
const (
	// roleActive: at least one exclusive session holds a lease — this
	// instance is the primary dispatcher for its exclusive routes.
	roleActive = "active"
	// roleStandby: exclusive sessions are configured but none currently
	// hold a lease — ready but not serving as primary.
	roleStandby = "standby"
	// roleStandalone: no exclusive sessions configured — no failover role.
	roleStandalone = "standalone"
)

// Role returns the operational role of this instance based on lease
// ownership: "active" if at least one exclusive session holds a lease,
// "standby" if exclusive sessions exist but none hold a lease,
// "standalone" if no exclusive sessions are configured.
// The result is a point-in-time snapshot computed under a single lock.
func (rt *Runtime) Role() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.roleUnlocked()
}

// roleUnlocked returns the role without acquiring the mutex (caller must hold it).
// It classifies by EXCLUSIVE sessions only, matching the documented contract:
// a non-exclusive session never acquires a lease and takes no part in failover,
// so it must NOT make the instance look like a standby. Keying off all
// sessionMgrs instead misclassified a plain single-session (non-exclusive)
// bridge as "standby", which the readiness cap then pinned below LevelFull —
// turning the canonical standalone deployment permanently not-ready on the
// legacy /ready probe.
func (rt *Runtime) roleUnlocked() string {
	hasExclusive := false
	for _, mgr := range rt.sessionMgrs {
		if !mgr.Exclusive() {
			continue
		}
		hasExclusive = true
		if _, held := mgr.Token(); held {
			return roleActive
		}
	}
	if !hasExclusive {
		return roleStandalone
	}
	return roleStandby
}

// DeepHealth, SessionHealthDetail and RouteHealth are read-side
// projections used by driving adapters (HTTP monitor / admin, CLI,
// future gRPC). They are defined in the ports package so adapters
// depend on the inner-ring contract, not on the runtime package.
// The aliases below preserve the runtime.X spelling at existing call
// sites without forcing an import-site rename.
type (
	DeepHealth          = ports.DeepHealth
	SessionHealthDetail = ports.SessionHealthDetail
	RouteHealth         = ports.RouteHealth
)

// defaultSessionHealthTimeout bounds a single plugin Session.Health call made
// from a health probe (DeepHealth → /ready, /deephealth). A plugin Health that
// blocks — a wedged broker client whose SDK call never returns — would otherwise
// hang the probe indefinitely, and it does so at the WORST possible moment: an
// outage, which is exactly when Kubernetes/ECS readiness is what sheds traffic
// from the sick instance. Bounding the call and classifying a timeout as
// NOT-ready (see healthUnderDeadline) makes the probe fail closed instead of
// hanging. 5s is comfortably longer than a healthy broker's Health latency yet
// well under a typical probe period, so it never false-trips a live session.
// it is now the SHARED deadline for the whole concurrent probe sweep
// (probeSessionsHealth), so the sweep costs ~one ceiling regardless of how many
// sessions are wedged — not O(N × ceiling).
const defaultSessionHealthTimeout = 5 * time.Second

// deepHealthProbeConcurrency caps the number of session Health calls in flight
// at once during a DeepHealth sweep. A fixed, small worker pool bounds the
// goroutine fan-out on a large fleet (never one goroutine per session
// unconditionally) while still collapsing the sweep to ~one ceiling under the
// shared deadline. 8 is comfortably above a typical bridge's session count so a
// healthy fleet is probed in a single wave.
const deepHealthProbeConcurrency = 8

// probeSessionsHealth probes every session's Health CONCURRENTLY under ONE
// shared, clock-driven deadline, returning results indexed to sessions.
//
// probing sessions SEQUENTIALLY cost O(N × ceiling) — a fleet of wedged
// sessions (12 × 5s = 60s) blows the 30–60s failover objective and piles
// concurrent probes on every scrape. A single deadline shared by every probe
// caps the WHOLE sweep at ~one ceiling regardless of session count; any session
// whose Health has not returned by the deadline is left not-ready/ServiceLevelNone
// (fail closed), so a wedged plugin still cannot advertise a green /ready.
//
// The deadline is clock-driven (rt.clk — the production timing gate forbids raw
// time.After) and also honours the caller's ctx (whichever fires first). A
// single watcher fans the deadline out to every worker via a close-once channel
// because a shared rt.clk.After value can be received by only one goroutine.
//
// Race-clean: each worker writes only its own results[i] slot and every worker
// has returned before the caller reads the slice (workers stop blocking on
// Health the moment the deadline fires — only the inner Health goroutine may
// linger, writing to its own buffered channel, never the shared slice).
//
// ponytail: a genuinely-hung plugin still leaks the single inner
// Health goroutine per probed session until it unblocks — Go cannot cancel a
// non-cooperative call. The bounded pool caps how many are spawned per sweep;
// fully eliminating the leak needs a plugin-side cancellable Health contract,
// out of scope here (CRITICAL+HIGH only).
func (rt *Runtime) probeSessionsHealth(ctx context.Context, sessions []ports.Session) []ports.SessionHealth {
	n := len(sessions)
	results := make([]ports.SessionHealth, n)
	// Fail closed by default: any session not resolved before the shared deadline
	// keeps this not-ready/ServiceLevelNone value.
	for i := range results {
		results[i] = ports.SessionHealth{ServiceLevel: ports.ServiceLevelNone}
	}
	if n == 0 {
		return results
	}

	// One shared deadline for the whole sweep, fanned out via a close-once
	// channel. The watcher owns deadlineFired (the sole closer, so no double
	// close); the caller signals completion via stop so the watcher never
	// outlives the sweep.
	deadlineFired := make(chan struct{})
	stop := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		defer close(deadlineFired)
		select {
		case <-rt.clk.After(defaultSessionHealthTimeout):
		case <-ctx.Done():
		case <-stop:
		}
	}()

	jobs := make(chan int)
	var wg sync.WaitGroup
	workers := deepHealthProbeConcurrency
	if workers > n {
		workers = n
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = rt.healthUnderDeadline(ctx, sessions[i], deadlineFired)
			}
		}()
	}

	// Feed indices to the pool, abandoning dispatch once the deadline fires so we
	// do not queue work that would only be marked not-ready anyway.
feed:
	for i := 0; i < n; i++ {
		select {
		case jobs <- i:
		case <-deadlineFired:
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	// Tear the watcher down (it may still be blocked on its rt.clk timer) and
	// wait for it to exit so no goroutine outlives the sweep.
	close(stop)
	<-watcherDone
	return results
}

// healthUnderDeadline invokes sess.Health, racing it against a SHARED deadline
// channel and the caller ctx. Session.Health is a plugin call that MAY ignore
// its context and block forever (a wedged SDK), so it runs in its own goroutine
// and the deadline is raced against it rather than trusting the plugin to honour
// ctx. On the deadline (or ctx cancellation) it returns a not-connected,
// not-ready, ServiceLevelNone health so the readiness aggregate fails CLOSED.
//
// ponytail: a genuinely-hung plugin leaks the single Health goroutine spawned
// here until (if ever) it unblocks. It only blocks on a buffered-channel send,
// so it holds no lock and is reclaimed the moment Health finally returns.
func (rt *Runtime) healthUnderDeadline(ctx context.Context, sess ports.Session, deadline <-chan struct{}) ports.SessionHealth {
	resCh := make(chan ports.SessionHealth, 1)
	go func() { resCh <- sess.Health(ctx) }()

	select {
	case sh := <-resCh:
		return sh
	case <-deadline:
		return ports.SessionHealth{ServiceLevel: ports.ServiceLevelNone}
	case <-ctx.Done():
		return ports.SessionHealth{ServiceLevel: ports.ServiceLevelNone}
	}
}

// DeepHealth returns a comprehensive health snapshot including session
// subscription readiness and lease status. Use ReadyForTraffic to
// determine if all sessions are connected and subscribed before
// sending messages through the bridge.
func (rt *Runtime) DeepHealth(ctx context.Context) ports.DeepHealth {
	// A blocking plugin Session.Health can take arbitrarily long (a wedged
	// broker client). Holding rt.mu across it would stall every other lock user
	// — including Stop, Role, and the /live+/ready probes — for the whole
	// duration, turning one slow session into a runtime-wide health outage
	// So snapshot the immutable references under the lock, RELEASE it, and invoke
	// the plugin calls (Health, runner reads) lock-free.
	type sessionSnap struct {
		sess              ports.Session
		sid               string
		hasLease          bool
		connectAfterLease bool
	}
	type routeSnap struct {
		id           string
		deliveryMode string
		runner       *route.RouteRunner
	}

	rt.mu.Lock()
	running := rt.running
	healthy := rt.healthy
	instanceID := rt.instanceID
	role := rt.roleUnlocked()

	sessSnaps := make([]sessionSnap, 0, len(rt.entries)+len(rt.sessionSenders))
	seen := make(map[string]bool)
	for _, e := range rt.entries {
		// Routes whose AddRoute caller passed a nil *session.Config
		// (e.g. SQS->SQS routes that have no MQTT session at all)
		// are intentionally excluded from session-health aggregation.
		if e.session == nil || e.sessCfg == nil {
			continue
		}
		sid := e.sessCfg.SessionID
		if seen[sid] {
			continue
		}
		seen[sid] = true
		snap := sessionSnap{sess: e.session, sid: sid, connectAfterLease: e.sessCfg.ConnectAfterLease}
		if mgr, ok := rt.sessionMgrs[sid]; ok {
			_, snap.hasLease = mgr.Token()
		}
		sessSnaps = append(sessSnaps, snap)
	}
	for sid, sse := range rt.sessionSenders {
		if seen[sid] {
			continue
		}
		seen[sid] = true
		snap := sessionSnap{sess: sse.session, sid: sid, connectAfterLease: sse.config.ConnectAfterLease}
		if mgr, ok := rt.sessionMgrs[sid]; ok {
			_, snap.hasLease = mgr.Token()
		}
		sessSnaps = append(sessSnaps, snap)
	}

	routeSnaps := make([]routeSnap, 0, len(rt.entries))
	for _, e := range rt.entries {
		routeSnaps = append(routeSnaps, routeSnap{
			id:           e.config.ID,
			deliveryMode: string(e.config.Policy.DeliveryMode),
			runner:       e.runner,
		})
	}
	// componentErrors snapshot for per-route supervision readiness (finding
	// L12): a route whose supervisor has recorded a fault is NOT ready even if
	// its close-once Started channel fired on the first (now-failed) run.
	routeErr := make(map[string]bool, len(routeSnaps))
	routeDead := make(map[string]bool, len(routeSnaps))
	for _, rs := range routeSnaps {
		if rt.componentErrors["route:"+rs.id] != nil {
			routeErr[rs.id] = true
		}
		// route_dead latches once a route has flapped routeDeadRestartThreshold
		// times without a stable run — a steady-state signal distinct from the
		// rate-based restart counter. It is suppressed once the route's
		// CURRENT run has outlived the stability window: a route that flapped to
		// the threshold then recovered and kept running never re-enters
		// superviseRoute to reset the counter, so reading liveness here keeps a
		// healthy long-running route from advertising route_dead forever.
		if rt.routeFlaps["route:"+rs.id] >= routeDeadRestartThreshold {
			key := "route:" + rs.id
			recovered := false
			if start, ok := rt.routeRunStart[key]; ok && rt.clk.Since(start) >= routeStabilityWindow {
				recovered = true
			}
			if !recovered {
				routeDead[rs.id] = true
			}
		}
	}
	rt.mu.Unlock()

	dh := ports.DeepHealth{
		Running:    running,
		Healthy:    healthy,
		InstanceID: instanceID,
		Role:       role,
	}
	allReady := running && healthy
	aggSL := ports.ServiceLevelFull

	// probe every session's Health CONCURRENTLY under one shared deadline
	// so the sweep costs ~one ceiling instead of O(N × ceiling) when sessions are
	// wedged. Results are indexed to sessSnaps; an un-returned probe is left
	// not-ready/ServiceLevelNone (fail closed) by probeSessionsHealth.
	sessions := make([]ports.Session, len(sessSnaps))
	for i, snap := range sessSnaps {
		sessions[i] = snap.sess
	}
	sessHealth := rt.probeSessionsHealth(ctx, sessions)

	for i, snap := range sessSnaps {
		sh := sessHealth[i]
		dh.Sessions = append(dh.Sessions, ports.SessionHealthDetail{
			SessionID:                snap.sid,
			Connected:                sh.Connected,
			HasLease:                 snap.hasLease,
			ConnectAfterLease:        snap.connectAfterLease,
			SubscriptionsWanted:      sh.SubscriptionsWanted,
			SubscriptionsActive:      sh.SubscriptionsActive,
			SubscriptionsSatisfied:   sh.SubscriptionsSatisfied,
			ActiveTopics:             sh.ActiveTopics,
			Ready:                    sh.Ready,
			ServiceLevel:             sh.ServiceLevel,
			UnsettledCount:           sh.UnsettledCount,
			OldestUnsettledAge:       sh.OldestUnsettledAge,
			ReceiveWindowUtilization: sh.ReceiveWindowUtilization,
			RecoveryRecycleCount:     sh.RecoveryRecycleCount,
		})
		// A deferred-connect standby's source session intentionally stays
		// disconnected until this instance wins the lease; excluding it from the
		// ready aggregate keeps a healthy standby reportable rather than
		// permanently un-ready.
		deferredStandby := snap.connectAfterLease && !snap.hasLease
		if !sh.Ready && !deferredStandby {
			allReady = false
		}
		aggSL = minServiceLevel(aggSL, sh.ServiceLevel)
	}

	for _, rs := range routeSnaps {
		ready := false
		if rs.runner != nil {
			select {
			case <-rs.runner.Started():
				ready = true
			default:
			}
			if rss, ok := rs.runner.Receiver().(ports.ReceiverStartedSignaler); ok {
				select {
				case <-rss.Started():
				default:
					ready = false
				}
			}
		}
		// A supervised route that has recorded a fault is not ready even though
		// its Started signal already fired.
		if routeErr[rs.id] {
			ready = false
		}
		inFlight := 0
		if rs.runner != nil {
			inFlight = int(rs.runner.InFlight())
		}
		dead := routeDead[rs.id]
		dh.Routes = append(dh.Routes, ports.RouteHealth{
			ID:           rs.id,
			DeliveryMode: rs.deliveryMode,
			Ready:        ready,
			InFlight:     inFlight,
			RouteDead:    dead,
		})
		// a route that is not ready OR latched dead means this instance
		// cannot dispatch that route, so it MUST NOT advertise traffic-ready.
		// Previously only the session loop narrowed allReady, so a down route left
		// ReadyForTraffic=true and an LB kept steering traffic at a bridge that
		// could not deliver. Floor the reported ServiceLevel too so dh.ServiceLevel
		// stays honest with ReadyForTraffic; the pure ReadinessLevelFromDeepHealth
		// independently caps such a snapshot below LevelFull. Per-route reporting
		// (Ready / RouteDead above) is unchanged.
		if !ready || dead {
			allReady = false
			aggSL = minServiceLevel(aggSL, ports.ServiceLevelDegraded)
		}
	}

	// An instance with no routes and no sessions bridges nothing: every
	// "all X are ready" aggregate above is vacuously true over the empty set, so
	// a bridge started from a missing or route-less config would otherwise
	// advertise itself as ready for traffic and reach LevelFull. Report the
	// state explicitly and refuse the traffic claim; the process stays alive and
	// running so a probe can still tell it apart from a dead one.
	dh.Empty = len(routeSnaps) == 0 && len(sessSnaps) == 0
	dh.ReadyForTraffic = allReady && !dh.Empty
	dh.ServiceLevel = aggSL
	return dh
}

// WaitRouteReady blocks until the named route reports Ready in DeepHealth,
// or the context is cancelled. Ready is defined as the RouteRunner's
// Started() channel being closed AND, if the receiver implements
// ReceiverStartedSignaler, its Started() channel being closed too.
// Both are close-once signals, so this is a pure event select with a
// 1s sanity fallback that handles the case where the route has not
// yet been registered (entries list does not contain it).
func (rt *Runtime) WaitRouteReady(ctx context.Context, routeID string) error {
	for {
		rt.mu.Lock()
		var runnerStarted, recvStarted <-chan struct{}
		var found bool
		for _, e := range rt.entries {
			if e.config.ID != routeID {
				continue
			}
			found = true
			if e.runner != nil {
				runnerStarted = e.runner.Started()
			}
			if rss, ok := e.receiver.(ports.ReceiverStartedSignaler); ok {
				recvStarted = rss.Started()
			}
			break
		}
		rt.mu.Unlock()

		if found {
			runnerReady := runnerStarted == nil || isClosed(runnerStarted)
			recvReady := recvStarted == nil || isClosed(recvStarted)
			if runnerReady && recvReady {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-runnerStarted:
		case <-recvStarted:
		case <-rt.clk.After(1 * time.Second):
		}
	}
}

// isClosed reports whether a close-once signal channel has been
// closed. Safe to call with a nil channel (returns false — callers
// treat nil as "no signal required" separately).
func isClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// QuiescenceOptions controls WaitQuiescent behaviour.
type QuiescenceOptions struct {
	Routes   []string      // only watch these routes (empty = all)
	MinQuiet time.Duration // zero defaults to 50ms; negative returns on the first all-zero snapshot
	Timeout  time.Duration // overall deadline (0 = rely on ctx)
}

// WaitQuiescent blocks until every watched route has zero InFlight
// deliveries for at least MinQuiet, or the context / Timeout expires.
// Event-driven: each watched RouteRunner's IdleChanged() channel fires
// on the InFlight → 0 transition. The quiet-window timer (clk.After)
// provides both the MinQuiet deadline and a sanity fallback.
func (rt *Runtime) WaitQuiescent(ctx context.Context, opts QuiescenceOptions) error {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	noQuietWindow := opts.MinQuiet < 0
	if opts.MinQuiet == 0 {
		opts.MinQuiet = 50 * time.Millisecond
	}

	quietSince := time.Time{}
	for {
		// Capture idle channels AND inFlight snapshot under rt.mu.
		// Channels must be captured before reading InFlight to avoid
		// a lost-wakeup race (see OutboxDrainer.WaitIdle).
		rt.mu.Lock()
		idleChs := make([]<-chan struct{}, 0, len(rt.entries))
		allZero := true
		for _, e := range rt.entries {
			if e.runner == nil {
				continue
			}
			if !routeWatched(opts.Routes, e.config.ID) {
				continue
			}
			idleChs = append(idleChs, e.runner.IdleChanged())
			if e.runner.InFlight() > 0 {
				allZero = false
			}
		}
		rt.mu.Unlock()

		if allZero {
			if noQuietWindow {
				return nil
			}
			if quietSince.IsZero() {
				quietSince = rt.clk.Now()
			}
			if rt.clk.Since(quietSince) >= opts.MinQuiet {
				return nil
			}
		} else {
			quietSince = time.Time{}
		}

		// Sanity/quiet-window timer: when we are currently quiet, sleep
		// the remaining window; otherwise fall back to MinQuiet so we
		// re-check on routes that never fire an idle transition.
		sanity := opts.MinQuiet
		if noQuietWindow {
			sanity = 50 * time.Millisecond
		}
		if !quietSince.IsZero() {
			sanity = opts.MinQuiet - rt.clk.Since(quietSince)
			if sanity <= 0 {
				continue
			}
		}

		cases := make([]reflect.SelectCase, 0, len(idleChs)+2)
		cases = append(cases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(ctx.Done()),
		})
		cases = append(cases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(rt.clk.After(sanity)),
		})
		for _, ch := range idleChs {
			cases = append(cases, reflect.SelectCase{
				Dir:  reflect.SelectRecv,
				Chan: reflect.ValueOf(ch),
			})
		}
		chosen, _, _ := reflect.Select(cases)
		if chosen == 0 {
			return ctx.Err()
		}
	}
}

// routeWatched reports whether routeID is in the watched set. An
// empty watched slice means "all routes".
func routeWatched(watched []string, routeID string) bool {
	if len(watched) == 0 {
		return true
	}
	return slices.Contains(watched, routeID)
}

// minServiceLevel returns the lower of two service levels.
// Order: None < Degraded < Full. Empty string is treated as None.
func serviceLevelOrd(s ports.ServiceLevel) int {
	switch s {
	case ports.ServiceLevelFull:
		return 2
	case ports.ServiceLevelDegraded:
		return 1
	default:
		return 0
	}
}

func minServiceLevel(a, b ports.ServiceLevel) ports.ServiceLevel {
	if serviceLevelOrd(a) < serviceLevelOrd(b) {
		return a
	}
	return b
}
