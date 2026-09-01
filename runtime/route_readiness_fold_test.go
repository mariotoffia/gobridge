package runtime

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/route"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ════════════════════════════════════════════════════════════════════════════
// Chunk-1 — DeepHealth route-readiness aggregation and the
// concurrent, deadline-bounded session-health probe.
//
// Deterministic: timeouts are driven by the injected fake clock; no
// time.Sleep sequences logic (the real-time selects are failure guards only).
// ════════════════════════════════════════════════════════════════════════════

// blockingReceiver keeps a RouteRunner alive by blocking Run until ctx is
// cancelled. It does NOT implement ReceiverStartedSignaler, so once the runner's
// Started() channel is closed the DeepHealth route projection reports the route
// Ready (no receiver-started gate to fail).
type blockingReceiver struct{}

func (blockingReceiver) Run(ctx context.Context, _ func(context.Context, ports.Delivery) error) error {
	<-ctx.Done()
	return ctx.Err()
}

type nopRouteSender struct{}

func (nopRouteSender) Send(context.Context, ports.OutboundMessage) error { return nil }

// startedReadyRunner returns a RouteRunner whose Started() channel is already
// closed (Run closes it as its first action) and whose receiver is not a
// ReceiverStartedSignaler — so DeepHealth reports the route Ready.
func startedReadyRunner(t *testing.T) *route.RouteRunner {
	t.Helper()
	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:  "r1",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Receiver: blockingReceiver{},
		Sender:   nopRouteSender{},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = runner.Run(ctx) }()
	select {
	case <-runner.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("route runner never signalled Started()")
	}
	return runner
}

// TestRouteReadinessFoldsIntoReadyForTraffic proves DeepHealth's
// ReadyForTraffic (and the derived ReadinessLevel) reflect ROUTE readiness, not
// just session readiness. A route that is not-ready OR latched dead must drive
// ReadyForTraffic=false and cap ReadinessLevelFromDeepHealth below LevelFull, so
// a load balancer does not keep steering traffic at a bridge that cannot
// dispatch that route.
//
// Mutation check (bridge_health.go): delete the `if !ready || dead { allReady =
// false; ... }` fold and the "faulted"/"dead" cases fail — ReadyForTraffic stays
// true while a route is down. Mutation check (ports/runtime.go): delete
// `|| rh.RouteDead` and the "dead" case's level assertion fails (level stays Full).
func TestRouteReadinessFoldsIntoReadyForTraffic(t *testing.T) {
	runner := startedReadyRunner(t)

	newRuntime := func(componentErr bool, flaps int) *Runtime {
		rt := &Runtime{
			clk:             clocktest.NewAt(time.Unix(0, 0)),
			componentErrors: make(map[string]error),
			routeFlaps:      make(map[string]int),
			routeRunStart:   make(map[string]time.Time),
			running:         true,
			healthy:         true,
			sessionSenders:  map[string]*sessionSenderEntry{},
			sessionMgrs:     map[string]*session.Manager{},
			entries: []*routeEntry{
				{config: RouteConfig{ID: "r1"}, runner: runner},
			},
		}
		if componentErr {
			rt.componentErrors["route:r1"] = fmt.Errorf("supervised route fault")
		}
		if flaps > 0 {
			// No routeRunStart entry → not "recovered" → route_dead latches.
			rt.routeFlaps["route:r1"] = flaps
		}
		return rt
	}

	t.Run("healthy route is traffic-ready (control)", func(t *testing.T) {
		dh := newRuntime(false, 0).DeepHealth(context.Background())
		if len(dh.Routes) != 1 || !dh.Routes[0].Ready || dh.Routes[0].RouteDead {
			t.Fatalf("control route must be Ready and not dead, got %+v", dh.Routes)
		}
		if !dh.ReadyForTraffic {
			t.Fatal("a fully-ready route must yield ReadyForTraffic=true (control)")
		}
		if lvl := ports.ReadinessLevelFromDeepHealth(dh); lvl != ports.LevelFull {
			t.Fatalf("a fully-ready route must reach LevelFull, got %v", lvl)
		}
	})

	t.Run("faulted route sheds traffic", func(t *testing.T) {
		dh := newRuntime(true, 0).DeepHealth(context.Background())
		if dh.Routes[0].Ready {
			t.Fatal("a supervised-faulted route must report Ready=false")
		}
		if dh.ReadyForTraffic {
			t.Fatal("a not-ready route must drive ReadyForTraffic=false")
		}
		if lvl := ports.ReadinessLevelFromDeepHealth(dh); lvl >= ports.LevelFull {
			t.Fatalf("a not-ready route must cap level below Full, got %v", lvl)
		}
	})

	t.Run("route-dead sheds traffic even while Ready", func(t *testing.T) {
		dh := newRuntime(false, routeDeadRestartThreshold).DeepHealth(context.Background())
		if !dh.Routes[0].Ready {
			t.Fatal("test setup: a latched-dead route is still Started (Ready=true)")
		}
		if !dh.Routes[0].RouteDead {
			t.Fatal("test setup: route_dead did not latch at the restart threshold")
		}
		if dh.ReadyForTraffic {
			t.Fatal("a route_dead route must drive ReadyForTraffic=false even while Ready")
		}
		if lvl := ports.ReadinessLevelFromDeepHealth(dh); lvl >= ports.LevelFull {
			t.Fatalf("a route_dead route must cap level below Full, got %v", lvl)
		}
	})
}

// hungCountingSession is a ports.Session whose Health increments a SHARED
// counter on entry and then blocks forever (a wedged broker client). The shared
// counter lets a test prove how many probes run CONCURRENTLY.
type hungCountingSession struct {
	entered *atomic.Int32
	release chan struct{} // never closed → Health blocks forever
}

func (s *hungCountingSession) Start(context.Context) error { return nil }
func (s *hungCountingSession) Reconcile(context.Context, connectivity.SessionPlan) error {
	return nil
}
func (s *hungCountingSession) Health(context.Context) ports.SessionHealth {
	s.entered.Add(1)
	<-s.release
	return ports.SessionHealth{Connected: true, Ready: true, ServiceLevel: ports.ServiceLevelFull}
}
func (s *hungCountingSession) Events() <-chan ports.SessionEvent { return nil }
func (s *hungCountingSession) Close(context.Context) error       { return nil }

// TestSessionProbesRunConcurrentlyUnderOneDeadline proves the DeepHealth
// session-health sweep probes sessions CONCURRENTLY under ONE shared deadline
// (bounded by deepHealthProbeConcurrency workers), not sequentially. With N
// wedged sessions the serial design cost O(N × ceiling) (12 × 5s = 60s), blowing
// the 30–60s failover objective. The concurrent design collapses the whole sweep
// to ~one ceiling: a SINGLE fake-clock advance past the ceiling completes it, and
// every un-returned session is marked not-ready (fail closed).
//
// Mutation check: serialize the probes (probe each session under its OWN
// per-session rt.clk.After, as the old healthWithTimeout loop did). Then only ONE
// session's Health runs before the first deadline, so `entered` never reaches the
// worker cap and this fails at the "expected N concurrent probes" assertion; a
// single clock advance also cannot complete the sweep, tripping the return guard.
func TestSessionProbesRunConcurrentlyUnderOneDeadline(t *testing.T) {
	fake := clocktest.NewAt(time.Unix(0, 0))
	const n = 12
	var entered atomic.Int32
	release := make(chan struct{}) // never closed → every Health hangs

	entries := make([]*routeEntry, n)
	for i := 0; i < n; i++ {
		entries[i] = &routeEntry{
			config:  RouteConfig{ID: fmt.Sprintf("r%d", i)},
			session: &hungCountingSession{entered: &entered, release: release},
			sessCfg: &session.Config{SessionID: fmt.Sprintf("s%d", i)},
		}
	}
	rt := &Runtime{
		clk:             fake,
		componentErrors: make(map[string]error),
		running:         true,
		healthy:         true,
		sessionSenders:  map[string]*sessionSenderEntry{},
		sessionMgrs:     map[string]*session.Manager{},
		entries:         entries,
	}

	done := make(chan ports.DeepHealth, 1)
	go func() { done <- rt.DeepHealth(context.Background()) }()

	wantConcurrent := deepHealthProbeConcurrency
	if wantConcurrent > n {
		wantConcurrent = n
	}

	// Wait until the bounded pool has ALL its workers engaged in Health (proving
	// concurrency) and the single shared deadline timer is registered.
	// wait.Poll, not wait.Until: on timeout the checks below name exactly which
	// half failed (serial probes vs. a missing shared deadline), which is the
	// whole diagnostic value of this test.
	wait.Poll(3*time.Second, func() bool {
		return int(entered.Load()) >= wantConcurrent && fake.TimerCount() >= 1
	})
	if got := int(entered.Load()); got < wantConcurrent {
		t.Fatalf("only %d concurrent session probes; expected %d (probes are serial, not a bounded pool)", got, wantConcurrent)
	}
	if got := int(entered.Load()); got > wantConcurrent {
		t.Fatalf("%d concurrent session probes exceed the worker cap %d (unbounded fan-out)", got, wantConcurrent)
	}
	if fake.TimerCount() != 1 {
		t.Fatalf("expected exactly ONE shared probe deadline for %d sessions, got %d (per-session timers = O(N × ceiling))", n, fake.TimerCount())
	}

	// A SINGLE advance past the ceiling must complete the WHOLE sweep.
	fake.Advance(defaultSessionHealthTimeout + time.Second)

	select {
	case dh := <-done:
		if len(dh.Sessions) != n {
			t.Fatalf("expected %d sessions, got %d", n, len(dh.Sessions))
		}
		for _, sh := range dh.Sessions {
			if sh.Ready {
				t.Fatalf("hung session %q must be classified NOT ready (fail closed)", sh.SessionID)
			}
			if sh.ServiceLevel != ports.ServiceLevelNone {
				t.Fatalf("hung session %q must floor ServiceLevel to none, got %q", sh.SessionID, sh.ServiceLevel)
			}
		}
		if dh.ReadyForTraffic {
			t.Fatal("all sessions hung → ReadyForTraffic must be false")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("DeepHealth did not complete within one ceiling after a single clock advance — probes are serial (O(N × ceiling)), not concurrent under one shared deadline")
	}
}
