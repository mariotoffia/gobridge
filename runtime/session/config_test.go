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
	assert.Equal(t, 2*time.Second, cfg.RenewJitter, "RenewJitter")
	assert.Equal(t, 3, cfg.MaxRenewFails, "MaxRenewFails")
	assert.Zero(t, cfg.RenewInterval,
		"RenewInterval kept zero so the manager derives LeaseTTL/MaxRenewFails")

	// Invariant 1: a graceful step-down must finish before the lease would
	// expire, so the old owner stops sending before a new owner takes over.
	assert.Less(t, cfg.StepDownGrace, cfg.LeaseTTL, "StepDownGrace must be < LeaseTTL")

	// Invariant 2: MaxRenewFails renew attempts fit inside one TTL. Derive
	// the renew interval the same way runtime/session/manager.go does for a
	// zero RenewInterval, then check it sums to <= LeaseTTL.
	derived := cfg.LeaseTTL / time.Duration(cfg.MaxRenewFails)
	assert.Equal(t, 15*time.Second, derived, "derived RenewInterval = LeaseTTL / MaxRenewFails")
	assert.LessOrEqual(t, derived*time.Duration(cfg.MaxRenewFails), cfg.LeaseTTL,
		"RenewInterval * MaxRenewFails must be <= LeaseTTL")

	// Invariant 3: jitter stays small relative to the renew interval (well
	// under half) so a jittered renewal cannot drift toward the TTL.
	assert.Less(t, cfg.RenewJitter, derived/2, "RenewJitter must be << derived RenewInterval")

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
