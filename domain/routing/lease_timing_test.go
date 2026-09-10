package routing_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestLeaseTimingResolve_DerivedMatrix documents which derived cadences are
// serviceable across the TTL and failure-tolerance combinations an operator can
// write. It is the reference the session manager, the blueprint validator and
// the builder all resolve through, so a change to the derivation shows up here
// as a changed verdict rather than as a silent millisecond cadence in
// production.
func TestLeaseTimingResolve_DerivedMatrix(t *testing.T) {
	for _, tc := range []struct {
		ttl      time.Duration
		maxFails int
		valid    bool
	}{
		// At the production-minimum TTL the per-attempt budget runs out at four
		// tolerated failures: the clamp then cuts the interval and what runs is
		// no longer what was written.
		{ttl: 5 * time.Second, maxFails: 1, valid: true},
		{ttl: 5 * time.Second, maxFails: 2, valid: true},
		{ttl: 5 * time.Second, maxFails: 3, valid: true},
		{ttl: 5 * time.Second, maxFails: 4, valid: false},
		{ttl: 5 * time.Second, maxFails: 5, valid: false},
		// The HA TTL absorbs every tolerance an operator plausibly writes.
		{ttl: 45 * time.Second, maxFails: 1, valid: true},
		{ttl: 45 * time.Second, maxFails: 5, valid: true},
		{ttl: 45 * time.Second, maxFails: 50, valid: false},
		{ttl: routing.DefaultLeaseTTL, maxFails: routing.DefaultMaxRenewFails, valid: true},
	} {
		t.Run(tc.ttl.String()+"/"+time.Duration(tc.maxFails).String(), func(t *testing.T) {
			timing := routing.LeaseTimingRequest{LeaseTTL: tc.ttl, MaxRenewFails: tc.maxFails}.Resolve()
			err := timing.ValidateCadence("session")
			if tc.valid {
				require.NoError(t, err,
					"resolved renew=%s poll=%s", timing.RenewInterval, timing.AcquirePollInterval)
				assert.GreaterOrEqual(t, timing.RenewInterval, routing.MinimumLeaseCadence)
				assert.GreaterOrEqual(t, timing.AcquirePollInterval, routing.MinimumLeaseCadence)
				return
			}
			require.Error(t, err)
			assert.True(t, errors.Is(err, shared.ErrInvalidConfig), "got %v", err)
		})
	}
}

// TestLeaseTimingResolve_JitterOnlyClampStaysValid pins the distinction the
// clamp verdict rests on. ClampRenewTimings sheds jitter FIRST because jitter
// only spreads renewal load; a request that fits once jitter is trimmed runs a
// healthy cadence and must not be rejected, or an operator who merely asked for
// generous spread gets a hard build failure.
func TestLeaseTimingResolve_JitterOnlyClampStaysValid(t *testing.T) {
	req := routing.LeaseTimingRequest{LeaseTTL: 45 * time.Second, RenewJitter: 14 * time.Second}
	timing := req.Resolve()

	require.Less(t, timing.RenewJitter, req.RenewJitter, "precondition: the clamp must have trimmed jitter")
	assert.Equal(t, timing.RequestedRenewInterval, timing.RenewInterval, "the interval must be untouched")
	assert.False(t, timing.CadenceClamped)
	assert.NoError(t, timing.ValidateCadence("session"))
}

// TestLeaseTimingResolve_PinnedIntervalKeepsZeroJitter pins the asymmetry the
// session manager relies on: jitter is derived ONLY when the interval was also
// unset. A pinned interval is explicit enough that a zero jitter means "no
// jitter, deterministic cadence" rather than "derive one".
func TestLeaseTimingResolve_PinnedIntervalKeepsZeroJitter(t *testing.T) {
	pinned := routing.LeaseTimingRequest{LeaseTTL: 45 * time.Second, RenewInterval: 8 * time.Second}.Resolve()
	assert.Zero(t, pinned.RenewJitter, "a pinned interval with no jitter must stay deterministic")

	derived := routing.LeaseTimingRequest{LeaseTTL: 45 * time.Second}.Resolve()
	assert.Positive(t, derived.RenewJitter, "a fully derived cadence must spread renewals")
}

// TestBaselineLeaseTiming_HAOnlyWhenNothingPinned pins the baseline rule a route
// session inherits. A clustered deployment gets the low-latency HA cadence only
// when the operator pinned NEITHER lease_ttl NOR renew_interval: pinning either
// means they are tuning the cadence themselves, and silently overriding half of
// it with HA values would produce timings nobody wrote.
func TestBaselineLeaseTiming_HAOnlyWhenNothingPinned(t *testing.T) {
	none := routing.LeaseTimingRequest{}

	assert.Equal(t, routing.HALeaseTTL, routing.BaselineLeaseTiming(true, none).LeaseTTL)
	assert.Equal(t, routing.HARenewInterval, routing.BaselineLeaseTiming(true, none).RenewInterval)

	assert.Equal(t, routing.DefaultLeaseTTL, routing.BaselineLeaseTiming(false, none).LeaseTTL)
	assert.Zero(t, routing.BaselineLeaseTiming(false, none).RenewInterval,
		"the non-clustered baseline leaves the interval to derivation so it follows the TTL")

	for _, pinned := range []routing.LeaseTimingRequest{
		{LeaseTTL: 60 * time.Second},
		{RenewInterval: 9 * time.Second},
	} {
		base := routing.BaselineLeaseTiming(true, pinned)
		assert.Equal(t, routing.DefaultLeaseTTL, base.LeaseTTL,
			"pinning any cadence knob keeps the default baseline, not HA")
	}
}

// TestLeaseTimingApplyOverrides_OnlyPositiveValuesWin pins "empty means inherit"
// end to end: a blueprint that omits a field must take the baseline, never a
// zero that would be read downstream as "derive from a TTL it did not choose".
func TestLeaseTimingApplyOverrides_OnlyPositiveValuesWin(t *testing.T) {
	base := routing.BaselineLeaseTiming(true, routing.LeaseTimingRequest{})
	got := base.ApplyOverrides(routing.LeaseTimingRequest{RenewJitter: 2 * time.Second, MaxRenewFails: 4})

	assert.Equal(t, routing.HALeaseTTL, got.LeaseTTL)
	assert.Equal(t, routing.HARenewInterval, got.RenewInterval)
	assert.Equal(t, 2*time.Second, got.RenewJitter)
	assert.Equal(t, 4, got.MaxRenewFails)
}

// BenchmarkLeaseTimingResolve pins the cost of the one resolution three
// boundaries now share. It runs once per exclusive session per blueprint
// validation, per build and per manager construction, so it must stay
// allocation-free: the blueprint validator calls it on every route session block
// of every admin commit.
func BenchmarkLeaseTimingResolve(b *testing.B) {
	for _, tc := range []struct {
		name string
		req  routing.LeaseTimingRequest
	}{
		{name: "derived_from_ttl", req: routing.LeaseTimingRequest{LeaseTTL: 45 * time.Second}},
		{name: "ha_pinned", req: routing.BaselineLeaseTiming(true, routing.LeaseTimingRequest{})},
		{name: "clamped", req: routing.LeaseTimingRequest{LeaseTTL: 5 * time.Second, MaxRenewFails: 5}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = tc.req.Resolve()
			}
		})
	}
}
