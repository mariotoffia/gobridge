package session

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
)

// TestCluster2_BrokerHealthStepDownDue pins the CLUSTER-2 broker-health
// step-down logic: an active owner whose broker path stays non-converged past the
// configured threshold becomes "due" for a voluntary step-down, while a converged
// or recovered owner never is. It is opt-in (threshold 0 disables it) and never
// fires before the first convergence (pre-connect activation is bounded
// separately).
func TestCluster2_BrokerHealthStepDownDue(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := &Manager{brokerHealthStepDown: 90 * time.Second, clk: clock.Clock(fake)}

	// Before the first convergence a disconnect does not arm the clock.
	m.markNonConverged()
	if m.brokerHealthStepDownDue() {
		t.Fatal("must not be due before the first convergence (activation, not an outage)")
	}

	// First connect + reconcile: converged. markConverged is the ONE place that
	// arms the gate, whether it was reached from a SessionConnected event or
	// from a completed post-acquire activation.
	m.markConverged()
	if m.brokerHealthStepDownDue() {
		t.Fatal("converged owner must never be due")
	}

	// Broker path drops.
	m.markNonConverged()
	if m.brokerHealthStepDownDue() {
		t.Fatal("just-dropped owner must not be due before the threshold")
	}
	fake.Advance(89 * time.Second)
	if m.brokerHealthStepDownDue() {
		t.Fatal("below threshold: not yet due")
	}
	fake.Advance(2 * time.Second) // 91s total > 90s
	if !m.brokerHealthStepDownDue() {
		t.Fatal("past threshold: the owner must be due to step down (CLUSTER-2)")
	}

	// Repeated disconnect events keep the earliest timestamp (elapsed measured
	// from the outage start), so it stays due.
	m.markNonConverged()
	if !m.brokerHealthStepDownDue() {
		t.Fatal("a later disconnect event must not reset the outage clock")
	}

	// Reconverging clears the clock.
	m.markConverged()
	if m.brokerHealthStepDownDue() {
		t.Fatal("reconverged owner must clear the step-down clock")
	}
}

// TestCluster2_DisabledByDefault proves broker-path failover is opt-in: with the
// threshold unset (zero), an owner disconnected for an arbitrarily long time is
// never due for a broker-health step-down (preserving the historical behaviour).
func TestCluster2_DisabledByDefault(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := &Manager{brokerHealthStepDown: 0, clk: clock.Clock(fake)}
	m.markConverged()
	m.markNonConverged()
	fake.Advance(time.Hour)
	if m.brokerHealthStepDownDue() {
		t.Fatal("with broker_health_step_down disabled, an owner must never step down on broker health")
	}
}

// TestCluster2_ConfigFlowsToManager proves the config field reaches the manager.
func TestCluster2_ConfigFlowsToManager(t *testing.T) {
	cfg := Config{
		SessionID:            "sess-c2",
		Exclusive:            true,
		LeaseTTL:             30 * time.Second,
		BrokerHealthStepDown: 45 * time.Second,
	}
	mgr := NewFromConfig(cfg, newCountingSession(), &partitionRenewStore{}, "owner-1", nil)
	if mgr.brokerHealthStepDown != 45*time.Second {
		t.Fatalf("brokerHealthStepDown = %s, want 45s (config must flow to the manager)", mgr.brokerHealthStepDown)
	}
}
