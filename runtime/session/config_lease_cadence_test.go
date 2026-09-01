package session_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// TestConfigValidate_RejectsCollapsedDerivedLeaseCadence pins that validation
// resolves the lease cadence exactly as the manager does and REJECTS a
// combination the constructor would silently clamp.
//
// A TTL with a large MaxRenewFails leaves no per-attempt budget: the derived
// renew interval and the standby acquire poll both collapse to the 1 ms floor.
// The owner then issues lease writes back-to-back and every standby issues a
// claim round per millisecond, which throttles the store and feeds the very
// transient-failure counter that decides ownership. Validation used to look only
// at an EXPLICIT RenewInterval, so the derived path — the production path once an
// operator supplies only lease_ttl and max_renew_fails — passed unchecked.
func TestConfigValidate_RejectsCollapsedDerivedLeaseCadence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		leaseTTL time.Duration
		maxFails int
	}{
		{name: "production_minimum_ttl", leaseTTL: session.MinimumProductionLeaseTTL, maxFails: 5},
		{name: "ha_ttl", leaseTTL: 45 * time.Second, maxFails: 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := session.Config{
				SessionID:     "collapsed",
				Exclusive:     true,
				LeaseTTL:      tc.leaseTTL,
				MaxRenewFails: tc.maxFails,
			}

			// The resolution the manager actually applies must be visible here:
			// both cadences have collapsed far below anything a lease store can
			// serve, yet nothing rejected the configuration.
			_, acquirePoll, _ := cfg.EffectiveFailoverLeaseTiming()
			require.Less(t, acquirePoll, session.MinimumLeaseCadence,
				"precondition: this combination must resolve below the cadence floor")

			err := cfg.Validate()
			require.Error(t, err, "a lease cadence the constructor would clamp must fail validation")
			assert.True(t, errors.Is(err, shared.ErrInvalidConfig),
				"a rejected cadence is an invalid configuration, got %v", err)
		})
	}
}

// TestConfigValidate_RejectsClampedRenewCadence pins the CLAMP branch on its own.
// The collapsed profiles above also resolve to a sub-floor acquire poll, so the
// floor check alone would satisfy them; this configuration does not. `lease_ttl:
// 6s` with `max_renew_fails: 4` resolves to a 350 ms renew interval and a 350 ms
// poll — both comfortably above the floor — but only because the clamp cut the
// interval and the per-call timeout to make four attempts fit. The failure
// tolerance the operator declared is not the one that would run, so it is
// rejected.
func TestConfigValidate_RejectsClampedRenewCadence(t *testing.T) {
	cfg := session.Config{
		SessionID:     "clamped",
		Exclusive:     true,
		LeaseTTL:      6 * time.Second,
		MaxRenewFails: 4,
	}

	_, acquirePoll, _ := cfg.EffectiveFailoverLeaseTiming()
	require.GreaterOrEqual(t, acquirePoll, session.MinimumLeaseCadence,
		"precondition: this case must be caught by the clamp rule, not the cadence floor")

	err := cfg.Validate()
	require.Error(t, err, "a cadence that only fits because the clamp cut it must fail validation")
	assert.True(t, errors.Is(err, shared.ErrInvalidConfig), "got %v", err)
}

// TestConfigValidate_AcceptsJitterOnlyClamp is the counterpart: the clamp sheds
// JITTER first, by design, because jitter only spreads renewal load. A config
// that fits once jitter is trimmed runs a perfectly healthy cadence, so it must
// keep validating — rejecting it would turn a blueprint the manager has always
// run into a hard build failure over a knob that is advisory.
func TestConfigValidate_AcceptsJitterOnlyClamp(t *testing.T) {
	cfg := session.Config{
		SessionID:   "jitter-heavy",
		Exclusive:   true,
		LeaseTTL:    45 * time.Second,
		RenewJitter: 14 * time.Second,
	}

	require.NoError(t, cfg.Validate())

	_, acquirePoll, _ := cfg.EffectiveFailoverLeaseTiming()
	assert.GreaterOrEqual(t, acquirePoll, session.MinimumLeaseCadence)
}

// TestConfigValidate_RejectsPinnedRenewIntervalBelowCadenceFloor pins the renew
// floor on its own. A generous TTL and a healthy standby poll keep both the
// clamp rule and the acquire-poll rule quiet; only the owner's own renew cadence
// is unserveable.
func TestConfigValidate_RejectsPinnedRenewIntervalBelowCadenceFloor(t *testing.T) {
	cfg := session.Config{
		SessionID:           "fast-renew",
		Exclusive:           true,
		LeaseTTL:            360 * time.Second,
		RenewInterval:       100 * time.Millisecond,
		AcquirePollInterval: 5 * time.Second,
		MaxRenewFails:       3,
	}

	err := cfg.Validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrInvalidConfig), "got %v", err)
}

// TestConfigValidate_RejectsAcquirePollBelowCadenceFloor pins the standby side:
// an explicit poll interval below the floor makes every standby hammer the lease
// store, so it is rejected even when the owner's renew cadence is sane.
func TestConfigValidate_RejectsAcquirePollBelowCadenceFloor(t *testing.T) {
	cfg := session.HAConfig("fast-poll", true)
	cfg.AcquirePollInterval = session.MinimumLeaseCadence - time.Millisecond

	err := cfg.Validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrInvalidConfig), "got %v", err)
}

// TestConfigValidate_AcceptsCadenceAtFloor is the negative control: the shipped
// presets, the derived path at the production-minimum TTL, and a cadence exactly
// ON the floor must all keep validating, so the new rule rejects only the
// collapsed configurations it targets.
func TestConfigValidate_AcceptsCadenceAtFloor(t *testing.T) {
	require.NoError(t, session.DefaultConfig("d", true).Validate())
	require.NoError(t, session.HAConfig("h", true).Validate())

	// Only lease_ttl supplied: the derived cadence must stay above the floor at
	// the lowest TTL production accepts.
	derived := session.Config{SessionID: "derived", Exclusive: true, LeaseTTL: session.MinimumProductionLeaseTTL}
	require.NoError(t, derived.Validate())

	atFloor := session.HAConfig("at-floor", true)
	atFloor.AcquirePollInterval = session.MinimumLeaseCadence
	require.NoError(t, atFloor.Validate())
}
