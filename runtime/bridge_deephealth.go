package runtime

import (
	"context"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// The DeepHealth read-side projection: the full snapshot driving adapters
// render /deephealth and readiness gates from. Split out of bridge_health.go,
// which holds the cheap single-field accessors and the session probe pool.
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

	sessSnaps := make([]sessionSnap, 0, len(rt.entries)+len(rt.sessionSenders)+len(rt.ingressSessions))
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
	// An ingress session never defers its connect and never holds a lease, so
	// it is counted the way a plain single session is.
	for sid, ise := range rt.ingressSessions {
		if seen[sid] {
			continue
		}
		seen[sid] = true
		sessSnaps = append(sessSnaps, sessionSnap{sess: ise.session, sid: sid})
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
