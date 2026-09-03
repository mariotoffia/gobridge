package runtime

import (
	"context"
	"errors"
	"maps"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/ports"
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
			return ports.RoleActive
		}
	}
	if !hasExclusive {
		return ports.RoleStandalone
	}
	return ports.RoleStandby
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
