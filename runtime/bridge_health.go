package runtime

import (
	"context"
	"maps"
	"reflect"
	"slices"
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
	// hold a lease — ready but not serving as primary (finding 22).
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
func (rt *Runtime) roleUnlocked() string {
	if len(rt.sessionMgrs) == 0 {
		return roleStandalone
	}
	for _, mgr := range rt.sessionMgrs {
		if _, held := mgr.Token(); held {
			return roleActive
		}
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

// DeepHealth returns a comprehensive health snapshot including session
// subscription readiness and lease status. Use ReadyForTraffic to
// determine if all sessions are connected and subscribed before
// sending messages through the bridge.
func (rt *Runtime) DeepHealth(ctx context.Context) ports.DeepHealth {
	// A blocking plugin Session.Health can take arbitrarily long (a wedged
	// broker client). Holding rt.mu across it would stall every other lock user
	// — including Stop, Role, and the /live+/ready probes — for the whole
	// duration, turning one slow session into a runtime-wide health outage
	// (finding L-M). So snapshot the immutable references under the lock, RELEASE
	// it, and invoke the plugin calls (Health, runner reads) lock-free.
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
		// rate-based restart counter (F5). It is suppressed once the route's
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

	for _, snap := range sessSnaps {
		sh := snap.sess.Health(ctx)
		dh.Sessions = append(dh.Sessions, ports.SessionHealthDetail{
			SessionID:           snap.sid,
			Connected:           sh.Connected,
			HasLease:            snap.hasLease,
			ConnectAfterLease:   snap.connectAfterLease,
			SubscriptionsWanted: sh.SubscriptionsWanted,
			SubscriptionsActive: sh.SubscriptionsActive,
			ActiveTopics:        sh.ActiveTopics,
			Ready:               sh.Ready,
			ServiceLevel:        sh.ServiceLevel,
		})
		// A deferred-connect standby's source session intentionally stays
		// disconnected until this instance wins the lease; excluding it from the
		// ready aggregate keeps a healthy standby reportable rather than
		// permanently un-ready (finding C3-M readiness).
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
		// its Started signal already fired (finding L12).
		if routeErr[rs.id] {
			ready = false
		}
		inFlight := 0
		if rs.runner != nil {
			inFlight = int(rs.runner.InFlight())
		}
		dh.Routes = append(dh.Routes, ports.RouteHealth{
			ID:           rs.id,
			DeliveryMode: rs.deliveryMode,
			Ready:        ready,
			InFlight:     inFlight,
			RouteDead:    routeDead[rs.id],
		})
	}

	dh.ReadyForTraffic = allReady
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
	MinQuiet time.Duration // how long all routes must stay at zero in-flight
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
