package paho

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

func TestConfigPostAcquireActivationTimingUsesEffectiveDefaults(t *testing.T) {
	timing := (Config{}).PostAcquireActivationTiming(connectivity.SessionExclusive)
	if timing.ConnectTimeout != DefaultConnectTimeout ||
		timing.ReconnectTimeout != DefaultReconnectAttemptTimeout ||
		timing.ReconcileTimeout != DefaultReconcileTimeout ||
		timing.ReplayGrace != DefaultUnmatchedGrace {
		t.Fatalf("default durable activation timing = %+v, want connect=%s reconnect=%s reconcile=%s replay=%s",
			timing, DefaultConnectTimeout, DefaultReconnectAttemptTimeout, DefaultReconcileTimeout, DefaultUnmatchedGrace)
	}
	if ephemeral := (Config{}).PostAcquireActivationTiming(connectivity.SessionEphemeral); ephemeral.ReplayGrace != 0 {
		t.Fatalf("ephemeral replay grace = %s, want 0", ephemeral.ReplayGrace)
	}
	decodedDefaults := DefaultConfig().PostAcquireActivationTiming(connectivity.SessionExclusive)
	if decodedDefaults.ReconnectTimeout != DefaultSessionOptions().ReconnectTimeout {
		t.Fatalf("decoded default reconnect timing = %s, want %s", decodedDefaults.ReconnectTimeout, DefaultSessionOptions().ReconnectTimeout)
	}
}

func TestConfigPostAcquireActivationTimingPreservesConfiguredLimits(t *testing.T) {
	cfg := Config{Session: SessionOptions{
		ConnectTimeout: 7 * time.Second, ReconnectTimeout: 6 * time.Second,
		ReconcileTimeout: 8 * time.Second, UnmatchedGrace: 9 * time.Second,
	}}
	timing := cfg.PostAcquireActivationTiming(connectivity.SessionPersistent)
	if timing.ConnectTimeout != 7*time.Second || timing.ReconnectTimeout != 6*time.Second ||
		timing.ReconcileTimeout != 8*time.Second || timing.ReplayGrace != 9*time.Second {
		t.Fatalf("configured activation timing = %+v", timing)
	}
}
