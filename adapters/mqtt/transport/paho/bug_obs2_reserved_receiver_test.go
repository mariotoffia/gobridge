package paho

import (
	"context"
	"log/slog"
	"testing"

	"github.com/eclipse/paho.golang/autopaho"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// TestBug_MQTTOBS2_ReservedReceiverNotFullBeforeReconcile pins MQTT-OBS-2: a
// connected session that has RESERVED an ingress receiver but has not yet
// declared its first plan (Reconcile pending) must NOT report ServiceLevelFull —
// it is a receiver still converging, not a sender-only session. A sender-only
// session (no reservation, no plan) is still Full when connected.
//
// Mutation check: delete the `ingressReserved && !planDeclared` case in Health
// and this fails — the reserved receiver falls into the sender-only Full branch.
func TestBug_MQTTOBS2_ReservedReceiverNotFullBeforeReconcile(t *testing.T) {
	connect := func(s *Session) {
		s.mu.Lock()
		s.cm = &pahoConn{cm: &autopaho.ConnectionManager{}}
		s.connected = true
		s.mu.Unlock()
	}

	t.Run("reserved receiver pending first reconcile is capped below Full", func(t *testing.T) {
		s := NewSession(SessionOptions{}, connectivity.SessionEphemeral, slog.Default())
		connect(s)
		s.mu.Lock()
		s.ingressReceiverReserved = true
		s.ingressReceiverID = "rx-1"
		s.mu.Unlock()

		if sl := s.Health(context.Background()).ServiceLevel; sl == ports.ServiceLevelFull {
			t.Fatalf("reserved receiver before first Reconcile reported Full; want capped below Full (MQTT-OBS-2)")
		}
	})

	t.Run("genuine sender-only session is Full when connected", func(t *testing.T) {
		s := NewSession(SessionOptions{}, connectivity.SessionEphemeral, slog.Default())
		connect(s)
		if sl := s.Health(context.Background()).ServiceLevel; sl != ports.ServiceLevelFull {
			t.Fatalf("connected sender-only session ServiceLevel = %v, want Full", sl)
		}
	})
}
