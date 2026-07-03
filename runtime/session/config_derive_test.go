package session

import (
	"testing"
	"time"
)

// TestDeriveRenewTimings_SatisfyInvariant pins finding 1 / contract C3: when an
// operator supplies ONLY LeaseTTL (bridge/convert.go no longer seeds
// DefaultConfig, so RenewInterval/RenewJitter arrive zero), the derived renew
// interval and jitter must satisfy the expiry-margin invariant with margin:
//
//	MaxRenewFails × (RenewInterval + RenewJitter/2) < LeaseTTL
//
// for a spread of realistic TTLs including the documented 45s HA value.
func TestDeriveRenewTimings_SatisfyInvariant(t *testing.T) {
	ttls := []time.Duration{
		30 * time.Second,
		45 * time.Second, // documented HA band value
		60 * time.Second,
		360 * time.Second, // default
	}
	for _, maxFails := range []int{1, 3, 5} {
		for _, ttl := range ttls {
			interval := deriveRenewInterval(ttl, maxFails)
			jitter := deriveRenewJitter(interval)
			worst := renewWorstCaseSpan(interval, jitter, maxFails)
			if worst >= ttl {
				t.Errorf("ttl=%s maxFails=%d: derived worst-case span %s (interval=%s jitter=%s) must be < ttl",
					ttl, maxFails, worst, interval, jitter)
			}
			// Require real margin (>=10%% of the TTL), not a hair under.
			if margin := ttl - worst; margin < ttl/10 {
				t.Errorf("ttl=%s maxFails=%d: margin %s is under 10%% of ttl", ttl, maxFails, margin)
			}
		}
	}
}

// TestNewManager_DerivesFromTTLOnly verifies the full construction-time
// derivation path (findings 1, 4, 6, C3): a Config carrying only LeaseTTL
// derives a safe renew interval, jitter, acquire-poll cadence, and per-call
// timeout, and the standby acquire-poll is no slower than the owner's renew
// cadence (finding 6).
func TestNewManager_DerivesFromTTLOnly(t *testing.T) {
	cfg := Config{SessionID: "s-derive", Exclusive: true, LeaseTTL: 45 * time.Second}

	m := newManager(cfg, nil, nil, "owner-1", nil)

	if m.renewInterval <= 0 {
		t.Fatalf("renewInterval not derived: %s", m.renewInterval)
	}
	if m.renewJitter < 0 {
		t.Fatalf("renewJitter negative: %s", m.renewJitter)
	}
	worst := renewWorstCaseSpan(m.renewInterval, m.renewJitter, m.maxRenewFails)
	if worst >= m.leaseTTL {
		t.Fatalf("derived worst-case span %s must be < leaseTTL %s", worst, m.leaseTTL)
	}
	// Finding 6: standbys must poll for acquisition at least as fast as the
	// owner renews, otherwise the poll cadence adds to failover time.
	if m.acquirePoll > m.renewInterval {
		t.Fatalf("acquirePoll %s must be <= renewInterval %s (finding 6)", m.acquirePoll, m.renewInterval)
	}
	if m.acquirePoll <= 0 {
		t.Fatalf("acquirePoll not derived: %s", m.acquirePoll)
	}
	// Finding 3: a per-call timeout must be set and bounded.
	if m.renewCallTimeout <= 0 || m.renewCallTimeout > 5*time.Second {
		t.Fatalf("renewCallTimeout out of bounds: %s", m.renewCallTimeout)
	}
}

// TestNewManager_HonorsPinnedIntervalWithZeroJitter verifies the deliberate
// asymmetry documented in newManager: a pinned RenewInterval with a zero
// RenewJitter is honored as "no jitter" (deterministic cadence) rather than
// reinterpreted as "derive". This keeps timing tests deterministic; the C3
// production path (both zero) still derives both.
func TestNewManager_HonorsPinnedIntervalWithZeroJitter(t *testing.T) {
	cfg := Config{
		SessionID:     "s-pinned",
		Exclusive:     true,
		LeaseTTL:      45 * time.Second,
		RenewInterval: 10 * time.Second,
		MaxRenewFails: 3,
		// RenewJitter deliberately left zero.
	}

	m := newManager(cfg, nil, nil, "owner-1", nil)

	if m.renewInterval != 10*time.Second {
		t.Fatalf("pinned renewInterval changed: got %s want 10s", m.renewInterval)
	}
	if m.renewJitter != 0 {
		t.Fatalf("pinned interval with zero jitter must stay zero, got %s", m.renewJitter)
	}
}

// TestConfigValidate pins finding 1's rejection requirement: an explicit renew
// combination whose worst-case jittered span reaches the TTL is rejected, while
// safe explicit and derive (zero) configs pass.
func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "explicit bad combo rejected",
			cfg: Config{
				SessionID:     "bad",
				LeaseTTL:      45 * time.Second,
				RenewInterval: 20 * time.Second, // 3 × 20 = 60s >= 45s
				MaxRenewFails: 3,
			},
			wantErr: true,
		},
		{
			name: "explicit bad combo via jitter rejected",
			cfg: Config{
				SessionID:     "bad-jitter",
				LeaseTTL:      45 * time.Second,
				RenewInterval: 14 * time.Second,
				RenewJitter:   3 * time.Second, // 3 × (14 + 1.5) = 46.5s >= 45s
				MaxRenewFails: 3,
			},
			wantErr: true,
		},
		{
			name: "safe explicit combo accepted",
			cfg: Config{
				SessionID:     "good",
				LeaseTTL:      45 * time.Second,
				RenewInterval: 14 * time.Second,
				RenewJitter:   1 * time.Second, // 3 × (14 + 0.5) = 43.5s < 45s
				MaxRenewFails: 3,
			},
			wantErr: false,
		},
		{
			name:    "zero (derive) config accepted",
			cfg:     Config{SessionID: "derive", LeaseTTL: 45 * time.Second},
			wantErr: false,
		},
		{
			name:    "negative duration rejected",
			cfg:     Config{SessionID: "neg", LeaseTTL: 45 * time.Second, RenewInterval: -1},
			wantErr: true,
		},
		{
			name: "stepdown grace exceeding ttl rejected",
			cfg: Config{
				SessionID:     "grace",
				LeaseTTL:      45 * time.Second,
				StepDownGrace: 50 * time.Second,
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}

	// The shipped presets must always validate.
	if err := DefaultConfig("d", true).Validate(); err != nil {
		t.Fatalf("DefaultConfig must validate: %v", err)
	}
	if err := HAConfig("h", true).Validate(); err != nil {
		t.Fatalf("HAConfig must validate: %v", err)
	}
}

// TestClampRenewTimings pins the defensive construction-time clamp: an unsafe
// explicit combination is shrunk until the worst-case span fits under the TTL,
// while a safe combination is left untouched.
func TestClampRenewTimings(t *testing.T) {
	ttl := 45 * time.Second

	// Unsafe: 3 × 20 = 60s >= 45s.
	interval, jitter, clamped := clampRenewTimings(ttl, 20*time.Second, 0, 3)
	if !clamped {
		t.Fatal("expected an unsafe combo to be clamped")
	}
	if worst := renewWorstCaseSpan(interval, jitter, 3); worst >= ttl {
		t.Fatalf("clamped worst-case span %s must be < ttl %s", worst, ttl)
	}

	// Safe: 3 × (14 + 0.5) = 43.5s < 45s.
	interval, jitter, clamped = clampRenewTimings(ttl, 14*time.Second, 1*time.Second, 3)
	if clamped {
		t.Fatal("safe combo must not be clamped")
	}
	if interval != 14*time.Second || jitter != 1*time.Second {
		t.Fatalf("safe combo altered: interval=%s jitter=%s", interval, jitter)
	}
}
