package paho

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// Session edge-recovery benchmarks
//
// These paths run on autopaho's SINGLE connection-management goroutine or on
// the reconcile gate, so their cost is not amortised across messages: a
// reconnect storm executes them once per connection edge while that goroutine
// is also the owner of PINGRESP and error handling. The baselines exist so a
// later change that adds an allocation or a lock round-trip to a connection
// edge is visible.
// ═══════════════════════════════════════════════════════════════════════════

// BenchmarkSession_ConnectionUp measures one full connection-up edge for each
// mode: the subscription-state reset, the resume-expectation check, the router
// grace re-arm and the event push. The persistent case additionally takes the
// Session Present=false branch, so the delta between the two is the cost the
// durable-resume signal adds to a reconnect.
func BenchmarkSession_ConnectionUp(b *testing.B) {
	cases := []struct {
		name           string
		mode           connectivity.SessionMode
		sessionPresent bool
	}{
		{"ephemeral", connectivity.SessionEphemeral, false},
		{"persistent_resumed", connectivity.SessionPersistent, true},
		{"persistent_resume_lost", connectivity.SessionPersistent, false},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			s := NewSession(SessionOptions{
				BrokerURLs: []string{"tcp://192.0.2.1:1883"},
				ClientID:   "bench-" + tc.name,
			}, tc.mode, nil, &ports.NoopExporter{})
			s.mu.Lock()
			s.cm = &fakeLiveConn{}
			s.mu.Unlock()

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.handleConnectionUpGenerationWithSessionPresent(s.connectionGeneration, tc.sessionPresent)
			}
		})
	}
}

// BenchmarkSession_ConnectFailure measures the latch-plus-bounded-code-metric
// path a reconnect storm walks on every rejected CONNECT.
func BenchmarkSession_ConnectFailure(b *testing.B) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "bench-connect-failure",
	}, connectivity.SessionPersistent, nil, &ports.NoopExporter{})
	err := shared.ErrUnavailable.WithMessage("dial tcp: connection refused")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.noteConnectFailure(err)
	}
}

// BenchmarkSession_ReconcileQoSDowngrade measures the steady state of a
// subscription accepted below its requested QoS: a reconcile of the unchanged
// filter, which issues no SUBSCRIBE but re-aligns the downgrade record with the
// plan and re-arms its re-check.
func BenchmarkSession_ReconcileQoSDowngrade(b *testing.B) {
	s, fake, clk, _ := newDowngradeSession(b, "bench-downgrade", connectivity.SessionPersistent, 0x00, nil)
	plan := planAtQoS("sensors/x", 1)
	ctx := context.Background()
	if err := s.Reconcile(ctx, plan); err != nil {
		b.Fatal(err)
	}
	confirmDowngrade(b, s, clk, fake, "sensors/x")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Reconcile(ctx, plan)
		// Every reconcile re-arms the re-check; each replaced schedule stops
		// its timer from its own goroutine. The fake clock keeps a stopped
		// timer until an Advance retires it. Advance(0) retires those without
		// firing the re-check an hour away, so memory stays bounded however
		// large b.N grows.
		clk.Advance(0)
	}
}

// BenchmarkSession_QoSDowngradeGrant measures the per-SUBACK bookkeeping of a
// lower grant without timers: recording one fresh grant and syncing the gauge,
// under the session lock as reconcile and the probe do.
func BenchmarkSession_QoSDowngradeGrant(b *testing.B) {
	b.Run("recheck_still_lower", func(b *testing.B) {
		s, _, _, _ := newDowngradeSession(b, "bench-grant-still", connectivity.SessionPersistent, 0x00, nil)
		s.mu.Lock()
		for range qosDowngradeConfirmations {
			s.applyGrantLocked("sensors/x", 1, 0, DefaultQoSRecheckInterval)
		}
		s.mu.Unlock()

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			s.mu.Lock()
			s.applyGrantLocked("sensors/x", 1, 0, DefaultQoSRecheckInterval)
			s.syncQoSDowngradeGaugeLocked()
			s.mu.Unlock()
		}
	})
	b.Run("lower_then_recovered", func(b *testing.B) {
		s, _, _, _ := newDowngradeSession(b, "bench-grant-alternate", connectivity.SessionPersistent, 0x00, nil)

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			s.mu.Lock()
			s.applyGrantLocked("sensors/x", 1, byte(i%2), DefaultQoSRecheckInterval)
			s.syncQoSDowngradeGaugeLocked()
			s.mu.Unlock()
		}
	})
}

// BenchmarkSession_QoSDowngradeProbe measures one re-check round of an
// accepted downgrade: the gated SUBSCRIBE, recording the unchanged grant and
// re-arming the next re-check. The probe is called directly with its record
// due, so the timer wait itself is not part of the measurement.
func BenchmarkSession_QoSDowngradeProbe(b *testing.B) {
	s, fake, clk, _ := newDowngradeSession(b, "bench-probe", connectivity.SessionPersistent, 0x00, nil)
	ctx := context.Background()
	if err := s.Reconcile(ctx, planAtQoS("sensors/x", 1)); err != nil {
		b.Fatal(err)
	}
	confirmDowngrade(b, s, clk, fake, "sensors/x")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.mu.Lock()
		s.qosDowngrades["sensors/x"].due = clk.Now()
		s.mu.Unlock()
		s.probeQoSDowngrades(ctx)
		// Retire the timer the round's re-arm stopped (see
		// BenchmarkSession_ReconcileQoSDowngrade); the new one is an hour away.
		clk.Advance(0)
	}
	b.StopTimer()
	if got := fake.subscribeCallCount(); got != qosDowngradeConfirmations+b.N {
		b.Fatalf("probe rounds sent %d SUBSCRIBEs, want %d", got, qosDowngradeConfirmations+b.N)
	}
}
