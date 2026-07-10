package runtime

import (
	"context"
	stdruntime "runtime"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// TestDeepHealth_HungSessionHealthTimesOutNotReady proves the readiness-hang fix
// (runtime/bridge_health.go): a plugin Session.Health that blocks forever (a
// wedged broker client) must NOT wedge /ready or /deephealth. DeepHealth bounds
// the call with a clock-driven timeout and classifies the timeout as
// not-connected / not-ready / ServiceLevelNone, so readiness fails CLOSED.
//
// Deterministic: the timeout is driven by the fake clock (no real sleep). The
// blocking Health call is never released, modelling a truly-hung plugin; we
// advance the fake clock past the health timeout and assert DeepHealth returns
// with the session marked not-ready.
func TestDeepHealth_HungSessionHealthTimesOutNotReady(t *testing.T) {
	fake := clocktest.NewAt(time.Unix(0, 0))
	// release is never closed → Health blocks forever (wedged plugin).
	sess := &blockingHealthSession{release: make(chan struct{}), entered: make(chan struct{}, 1)}

	rt := &Runtime{
		clk:             fake,
		componentErrors: make(map[string]error),
		running:         true,
		healthy:         true,
		sessionSenders:  map[string]*sessionSenderEntry{},
		sessionMgrs:     map[string]*session.Manager{},
		entries: []*routeEntry{
			{
				config:  RouteConfig{ID: "r1"},
				session: sess,
				sessCfg: &session.Config{SessionID: "s1"},
			},
		},
	}

	type result struct{ dh ports.DeepHealth }
	done := make(chan result, 1)
	go func() {
		done <- result{dh: rt.DeepHealth(context.Background())}
	}()

	// Wait until DeepHealth has entered the (blocking) plugin Health call.
	select {
	case <-sess.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("DeepHealth never entered Session.Health")
	}

	// The timeout is registered via rt.clk.After inside probeSessionsHealth (one
	// shared deadline for the whole sweep). Spin (yielding, no logic-driving
	// sleep) until the fake clock sees the timer, then advance past it to fire the
	// timeout deterministically.
	deadline := time.Now().Add(2 * time.Second)
	for fake.TimerCount() < 1 && time.Now().Before(deadline) {
		stdruntime.Gosched()
	}
	if fake.TimerCount() < 1 {
		t.Fatal("probeSessionsHealth never registered its clock timeout")
	}
	fake.Advance(defaultSessionHealthTimeout + time.Second)

	select {
	case r := <-done:
		if len(r.dh.Sessions) != 1 {
			t.Fatalf("expected 1 session in DeepHealth, got %d", len(r.dh.Sessions))
		}
		sh := r.dh.Sessions[0]
		if sh.Ready {
			t.Fatalf("hung Session.Health must be classified NOT ready, got Ready=true")
		}
		if sh.Connected {
			t.Fatalf("hung Session.Health must be classified NOT connected, got Connected=true")
		}
		if sh.ServiceLevel != ports.ServiceLevelNone {
			t.Fatalf("hung Session.Health must floor ServiceLevel to none, got %q", sh.ServiceLevel)
		}
		if r.dh.ReadyForTraffic {
			t.Fatalf("a hung session must make ReadyForTraffic=false (readiness fails closed)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DeepHealth hung on a blocking Session.Health; per-session timeout did not fire")
	}
}
