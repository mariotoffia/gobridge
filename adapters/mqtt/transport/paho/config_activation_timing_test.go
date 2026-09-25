package paho

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

func TestConfigPostAcquireActivationTimingUsesConservativeEffectiveDefaults(t *testing.T) {
	timing := (Config{}).PostAcquireActivationTiming(connectivity.SessionExclusive)
	want := 2*DefaultConnectTimeout + 6*DefaultReconcileTimeout + 2*DefaultUnmatchedGrace
	if timing.WorstCaseDuration != want {
		t.Fatalf("default durable worst-case activation = %s, want %s", timing.WorstCaseDuration, want)
	}
	if ephemeral := (Config{}).PostAcquireActivationTiming(connectivity.SessionEphemeral); ephemeral.WorstCaseDuration != 0 {
		t.Fatalf("ephemeral worst-case activation = %s, want 0", ephemeral.WorstCaseDuration)
	}
	decodedDefaults := DefaultConfig().PostAcquireActivationTiming(connectivity.SessionExclusive)
	if decodedDefaults.WorstCaseDuration != want {
		t.Fatalf("decoded default worst-case activation = %s, want %s", decodedDefaults.WorstCaseDuration, want)
	}
}

func TestConfigPostAcquireActivationTimingSumsSequentialManagedMigrationPhases(t *testing.T) {
	cfg := Config{Session: SessionOptions{
		ConnectTimeout: 7 * time.Second, ReconnectTimeout: 6 * time.Second,
		ReconcileTimeout: 8 * time.Second, UnmatchedGrace: 9 * time.Second,
	}}
	timing := cfg.PostAcquireActivationTiming(connectivity.SessionPersistent)
	// Initial + recycle connection, four sequential reconcile-owned waits
	// (SUBSCRIBE, UNSUBSCRIBE, quiesce, final SUBSCRIBE), two possible
	// replay-verification windows for crash residue plus newly removed filters,
	// and one dead-letter budget for each of those two replay windows.
	const want = 2*7*time.Second + 6*8*time.Second + 2*9*time.Second
	if timing.WorstCaseDuration != want {
		t.Fatalf("configured worst-case activation = %s, want %s", timing.WorstCaseDuration, want)
	}
}

func TestConfigPostAcquireActivationTimingSaturatesDurationOverflow(t *testing.T) {
	cfg := Config{Session: SessionOptions{
		ConnectTimeout:   time.Duration(1<<63 - 1),
		ReconcileTimeout: time.Second,
		UnmatchedGrace:   time.Second,
	}}
	if got := cfg.PostAcquireActivationTiming(connectivity.SessionExclusive).WorstCaseDuration; got != time.Duration(1<<63-1) {
		t.Fatalf("overflowing activation bound = %s, want saturated max duration", got)
	}
}

func TestConfigTransportFailoverTimingUsesCompleteDefaultActivation(t *testing.T) {
	got := (Config{}).TransportFailoverTiming(connectivity.SessionExclusive)
	want := (Config{}).PostAcquireActivationTiming(connectivity.SessionExclusive).WorstCaseDuration
	if got.PostTakeoverActivation != want {
		t.Fatalf("default failover activation = %s, want complete activation %s", got.PostTakeoverActivation, want)
	}
}

func TestConfigTransportFailoverTimingIncludesManagedMigrationRecycleAndReplay(t *testing.T) {
	cfg := Config{Session: SessionOptions{
		ConnectTimeout: 7 * time.Second, ReconnectTimeout: 6 * time.Second,
		ReconcileTimeout: 8 * time.Second, UnmatchedGrace: 9 * time.Second,
	}}
	got := cfg.TransportFailoverTiming(connectivity.SessionExclusive)
	// Initial connect + cleanup recycle connect, initial SUBSCRIBE, exact
	// UNSUBSCRIBE, bounded ingress quiescence, replacement SUBSCRIBE, two
	// replay-verification grace windows, and the dead-letter budget of each
	// replay window. ReconnectTimeout is nested and not added.
	const want = 2*7*time.Second + 6*8*time.Second + 2*9*time.Second
	if got.PostTakeoverActivation != want {
		t.Fatalf("migration/recycle failover activation = %s, want %s", got.PostTakeoverActivation, want)
	}
	if got.PostTakeoverActivation <= cfg.Session.ConnectTimeout+cfg.Session.ReconcileTimeout {
		t.Fatalf("failover activation undercounted cleanup/recycle/replay phases: %s", got.PostTakeoverActivation)
	}
}

// TestConfigSettlementRecoveryWaitIsTheActivationWorstCase pins the wait a
// settlement-recovery recycle gives the deliveries the runtime already accepted:
// the recycle runs the same sequential phases an activation does, so it is the
// same bound. The route validator reads it through the port to keep an
// in-process send retry inside it, and a mode that never recycles reports zero.
func TestConfigSettlementRecoveryWaitIsTheActivationWorstCase(t *testing.T) {
	want := 2*DefaultConnectTimeout + 6*DefaultReconcileTimeout + 2*DefaultUnmatchedGrace
	if got := (Config{}).SettlementRecoveryWait(connectivity.SessionPersistent); got != want {
		t.Fatalf("default persistent settlement-recovery wait = %s, want %s", got, want)
	}
	if got := (Config{}).SettlementRecoveryWait(connectivity.SessionEphemeral); got != 0 {
		t.Fatalf("ephemeral settlement-recovery wait = %s, want 0", got)
	}

	cfg := Config{Session: SessionOptions{
		ConnectTimeout: 7 * time.Second, ReconcileTimeout: 8 * time.Second, UnmatchedGrace: 9 * time.Second,
	}}
	for _, mode := range []connectivity.SessionMode{
		connectivity.SessionPersistent, connectivity.SessionExclusive, connectivity.SessionEphemeral, "",
	} {
		activation := cfg.PostAcquireActivationTiming(mode).WorstCaseDuration
		if got := cfg.SettlementRecoveryWait(mode); got != activation {
			t.Fatalf("settlement-recovery wait for mode %q = %s, want the activation worst case %s",
				mode, got, activation)
		}
	}
}

func TestConfigTransportFailoverTimingSaturatesCompleteActivationOverflow(t *testing.T) {
	cfg := Config{Session: SessionOptions{
		ConnectTimeout: time.Duration(1<<63 - 1), ReconcileTimeout: time.Second,
		UnmatchedGrace: time.Second,
	}}
	if got := cfg.TransportFailoverTiming(connectivity.SessionExclusive).PostTakeoverActivation; got != time.Duration(1<<63-1) {
		t.Fatalf("overflowing failover activation = %s, want saturated maximum", got)
	}
}
