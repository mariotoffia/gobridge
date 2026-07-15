package paho

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

func TestConfigPostAcquireActivationTimingUsesConservativeEffectiveDefaults(t *testing.T) {
	timing := (Config{}).PostAcquireActivationTiming(connectivity.SessionExclusive)
	want := 2*DefaultConnectTimeout + 4*DefaultReconcileTimeout + 2*DefaultUnmatchedGrace
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
	// (SUBSCRIBE, UNSUBSCRIBE, quiesce, final SUBSCRIBE), and two possible
	// replay-verification windows for crash residue plus newly removed filters.
	const want = 2*7*time.Second + 4*8*time.Second + 2*9*time.Second
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
