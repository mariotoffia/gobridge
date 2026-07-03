package session_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/runtime/session"
)

// TestHAConfig_FailoverInvariants pins the high-availability preset's
// concrete lease-timing values and the interrelationship invariants that
// keep cluster failover safe. The meaningful guard is the invariant block:
// if a later edit breaks StepDownGrace < LeaseTTL, or pushes
// RenewInterval*MaxRenewFails past LeaseTTL, this test must fail.
func TestHAConfig_FailoverInvariants(t *testing.T) {
	cfg := session.HAConfig("telemetry", true)

	// Concrete preset values (~45s failover band).
	assert.Equal(t, 45*time.Second, cfg.LeaseTTL, "LeaseTTL")
	assert.Equal(t, 5*time.Second, cfg.StepDownGrace, "StepDownGrace")
	assert.Equal(t, 1*time.Second, cfg.RenewJitter, "RenewJitter")
	assert.Equal(t, 3, cfg.MaxRenewFails, "MaxRenewFails")
	assert.Equal(t, 14*time.Second, cfg.RenewInterval,
		"RenewInterval pinned to 14s so RenewInterval*MaxRenewFails < LeaseTTL (A8-R1)")

	// Invariant 1: a graceful step-down must finish before the lease would
	// expire, so the old owner stops sending before a new owner takes over.
	assert.Less(t, cfg.StepDownGrace, cfg.LeaseTTL, "StepDownGrace must be < LeaseTTL")

	// Invariant 2: MaxRenewFails renew attempts fit STRICTLY inside one TTL,
	// so the third (recovering) attempt lands before expiry instead of exactly
	// on the boundary (A8-R1-leasettl-margin). RenewInterval is pinned (not the
	// derived LeaseTTL/MaxRenewFails, which would sit on the boundary).
	assert.Less(t, cfg.RenewInterval*time.Duration(cfg.MaxRenewFails), cfg.LeaseTTL,
		"RenewInterval * MaxRenewFails must be < LeaseTTL")

	// Invariant 3: the JITTERED worst-case renew span must stay STRICTLY under
	// the TTL. Each of the MaxRenewFails attempts can be delayed by up to
	// RenewInterval + RenewJitter/2 (max positive jitter), so the owner must
	// detect loss and step down before its own lease expires (A9-J5). At 1s
	// jitter: 3 × (14 + 0.5) = 43.5s < 45s. At the old 2s: 3 × 15 = 45s = TTL,
	// which left no margin.
	jitteredWorstCase := time.Duration(cfg.MaxRenewFails) * (cfg.RenewInterval + cfg.RenewJitter/2)
	assert.Less(t, jitteredWorstCase, cfg.LeaseTTL,
		"jittered worst-case renew span (%s) must be < LeaseTTL (%s)", jitteredWorstCase, cfg.LeaseTTL)

	// Invariant 4: jitter stays small relative to the renew interval (well
	// under half) so a jittered renewal cannot drift toward the TTL.
	assert.Less(t, cfg.RenewJitter, cfg.RenewInterval/2, "RenewJitter must be << RenewInterval")

	// Sanity: HAConfig is a strictly tighter preset than DefaultConfig where
	// the timing knobs are overridden.
	def := session.DefaultConfig("telemetry", true)
	assert.Less(t, cfg.LeaseTTL, def.LeaseTTL, "HA LeaseTTL must be shorter than default")
	assert.Less(t, cfg.StepDownGrace, def.StepDownGrace, "HA StepDownGrace must be shorter than default")
	assert.Less(t, cfg.RenewJitter, def.RenewJitter, "HA RenewJitter must be smaller than default")

	// Sanity: the inherited (non-timing) defaults are preserved.
	assert.Equal(t, def.DrainBatchSize, cfg.DrainBatchSize, "DrainBatchSize inherited from default")
	assert.Equal(t, def.DrainMaxConcurrency, cfg.DrainMaxConcurrency,
		"DrainMaxConcurrency inherited from default")
	assert.NotNil(t, cfg.DrainStrategy, "DrainStrategy inherited from default")
}

// TestLeaseRenewMargin_BothConfigs pins the A8-R1-leasettl-margin invariant
// for BOTH presets: RenewInterval*MaxRenewFails must be STRICTLY less than
// LeaseTTL, so the final (MaxRenewFails-th) renew attempt lands before the
// lease-expiry boundary rather than on it. That margin makes the documented
// "tolerate two transient renew failures then recover on the third" guarantee
// literally true for the default and HA presets alike. The presets pin
// RenewInterval explicitly precisely to create this margin; a zero here would
// collapse to the derived LeaseTTL/MaxRenewFails and sit exactly on the
// boundary (120*3=360=LeaseTTL for the default, 15*3=45=LeaseTTL for HA).
func TestLeaseRenewMargin_BothConfigs(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  session.Config
	}{
		{"default", session.DefaultConfig("telemetry", true)},
		{"ha", session.HAConfig("telemetry", true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.NotZero(t, tc.cfg.RenewInterval, "RenewInterval must be pinned (non-zero)")
			assert.Greater(t, tc.cfg.MaxRenewFails, 0, "MaxRenewFails must be positive")
			span := tc.cfg.RenewInterval * time.Duration(tc.cfg.MaxRenewFails)
			assert.Less(t, span, tc.cfg.LeaseTTL,
				"RenewInterval*MaxRenewFails (%s) must be < LeaseTTL (%s) so the "+
					"final renew lands before the expiry boundary", span, tc.cfg.LeaseTTL)
		})
	}
}
