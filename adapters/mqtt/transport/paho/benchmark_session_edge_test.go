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

// BenchmarkSession_ReconcileQoSDowngrade measures a reconcile whose broker
// grant sits below the requested QoS, including the permanence confirmation
// bookkeeping. It is the reconnect-storm shape of a broker QoS cap: one
// reconcile per connection edge, each concluding the same downgrade.
func BenchmarkSession_ReconcileQoSDowngrade(b *testing.B) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "bench-downgrade",
	}, connectivity.SessionPersistent, nil, &ports.NoopExporter{})
	s.mu.Lock()
	s.cm = &fakeReconcileConn{reasons: []byte{0x00}}
	s.connected = true
	empty := connectivity.SessionPlan{}
	s.appliedPlan = &empty
	s.mu.Unlock()

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "sensors/x", QoS: 1}},
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Reconcile(ctx, plan)
	}
}
