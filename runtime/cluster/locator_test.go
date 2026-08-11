package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// Production contract: owner expiry is a WALL-CLOCK observation, and it is
// disclosed rather than repaired.
//
// The locator compares its LOCAL clock with the owner-written ExpiresAt. Fleet
// clock skew above the renew margin therefore makes a healthy owner look
// expired (or a dead owner look live) — an ADVISORY routing effect only: the
// locator never mints a token, so data-path fencing stays skew-immune. The same
// wall-clock reading is what makes a cold takeover (whole-fleet restart against
// an hours-expired row) surface as "owner unknown" until a successor
// actually acquires the lease — the locator never elects itself.
//
// The accepted tradeoff (keep skew-safe observation, do not invent clock logic)
// is closed by DISCLOSURE: every ownership-unknown decision raises
// shared.MetricRouteOwnerUnknown tagged with the reason, so skew and cold
// takeover are visible to an operator instead of appearing as unexplained
// 502/503 responses.
// ═══════════════════════════════════════════════════════════════════════════

// unknownReasons returns the reason tag of every MetricRouteOwnerUnknown
// emission, in order.
func unknownReasons(t *testing.T, rec *ports.RecordingExporter) []string {
	t.Helper()
	var out []string
	for _, e := range rec.FindEntries(shared.MetricRouteOwnerUnknown) {
		reason := ""
		for _, tag := range e.Tags {
			if tag.Key == shared.TagKeyReason {
				reason = tag.Value
			}
		}
		require.NotEmpty(t, reason, "every ownership-unknown emission must name its reason")
		out = append(out, reason)
	}
	return out
}

// TestLocator_OwnerClockSkewIsDisclosed_ProductionContract proves the accepted
// behavior: a lease the OWNER still considers live reads as expired
// once the local clock passes the owner-written ExpiresAt, the locator refuses
// the routing decision (advisory fail-closed, no self-election, no token), and
// the decision is metered so the skew is diagnosable.
func TestLocator_OwnerClockSkewIsDisclosed_ProductionContract(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	rec := &ports.RecordingExporter{}
	store := &stubLeaseStore{}
	// The remote owner renewed against ITS clock and considers the lease live for
	// another 30s. This node's clock runs 31s ahead — skew above the renew
	// margin — so the same row reads as expired here.
	store.setInfo(persistence.LeaseInfo{
		Owner:     "instance-remote",
		Version:   7,
		ExpiresAt: fake.Now().Add(-31 * time.Second),
		Endpoints: map[string]string{"http": "http://remote:8080"},
	})

	rl := NewLocator("instance-local", store, LocatorConfig{
		CacheTTL:       time.Second,
		MaxFailures:    100,
		CooldownPeriod: time.Hour,
		Metrics:        rec,
	}, fake)
	rl.RegisterRoute("route-1", "sess-1")

	peer, local, err := rl.Locate(context.Background(), "route-1")
	require.Error(t, err, "a skew-expired owner must not be served as authoritative")
	assert.ErrorIs(t, err, shared.ErrNoRouteOwner)
	assert.False(t, local, "the locator must never promote itself to owner on a skewed observation")
	assert.Nil(t, peer)

	assert.Equal(t, []string{reasonLeaseExpired}, unknownReasons(t, rec),
		"the skewed observation must be disclosed as an expired-lease decision")
}

// TestLocator_ColdTakeoverWindowIsDisclosed_ProductionContract pins the accepted
// behavior: after a whole-fleet restart the surviving row names an
// owner that died hours ago, and the locator reports ownership-unknown for the
// WHOLE observation window — it does not shortcut the wait by trusting the old
// wall-clock expiry. Every call in that window is metered, and routing resumes
// the moment a successor actually holds the lease.
func TestLocator_ColdTakeoverWindowIsDisclosed_ProductionContract(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	rec := &ports.RecordingExporter{}
	store := &stubLeaseStore{}
	// Hours-expired row from the pre-restart generation.
	store.setInfo(persistence.LeaseInfo{
		Owner:     "instance-dead",
		Version:   3,
		ExpiresAt: fake.Now().Add(-4 * time.Hour),
		Endpoints: map[string]string{"http": "http://dead:8080"},
	})

	rl := NewLocator("instance-local", store, LocatorConfig{
		CacheTTL:       2 * time.Second,
		MaxFailures:    100,
		CooldownPeriod: time.Hour,
		Metrics:        rec,
	}, fake)
	rl.RegisterRoute("route-1", "sess-1")
	ctx := context.Background()

	// The full lease-observation window elapses before any successor acquires.
	for i := range 3 {
		_, local, err := rl.Locate(ctx, "route-1")
		require.Error(t, err, "call %d: an hours-expired row must not name an owner", i)
		assert.False(t, local, "call %d: no self-election during the cold-takeover window", i)
		fake.Advance(15 * time.Second)
	}
	assert.Equal(t, []string{reasonLeaseExpired, reasonLeaseExpired, reasonLeaseExpired},
		unknownReasons(t, rec), "every cold-takeover call must be disclosed")

	// A successor finally acquires: the locator routes again with no operator
	// action — the window is bounded by acquisition, not by the stale row.
	store.setInfo(persistence.LeaseInfo{
		Owner:     "instance-local",
		Version:   4,
		ExpiresAt: fake.Now().Add(45 * time.Second),
	})
	_, local, err := rl.Locate(ctx, "route-1")
	require.NoError(t, err)
	assert.True(t, local, "once this node owns the lease the route is handled locally")
	assert.Len(t, unknownReasons(t, rec), 3, "a resolved ownership must not emit an unknown decision")
}

// TestLocator_UnknownOwnerReasonsAreDistinct_ProductionContract proves the
// disclosure discriminates the three operator-actionable causes: an unowned
// lease (normal transfer), a store outage with no cached owner, and the breaker
// holding the route closed. Without distinct reasons an operator cannot tell a
// skewed clock from a dead store.
func TestLocator_UnknownOwnerReasonsAreDistinct_ProductionContract(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	rec := &ports.RecordingExporter{}
	store := &stubLeaseStore{}
	rl := NewLocator("instance-local", store, LocatorConfig{
		CacheTTL:       time.Second,
		MaxFailures:    2,
		CooldownPeriod: time.Hour,
		Metrics:        rec,
	}, fake)
	rl.RegisterRoute("route-1", "sess-1")
	ctx := context.Background()

	// 1. Unowned lease: normal mid-transfer state, must NOT read as a store fault.
	store.setErrForNCalls(shared.ErrNotFound, 1)
	_, _, err := rl.Locate(ctx, "route-1")
	require.ErrorIs(t, err, shared.ErrNoRouteOwner)

	// 2. Store failures with no cached owner, up to the breaker threshold.
	store.setErrForNCalls(shared.ErrUnavailable, 2)
	for range 2 {
		_, _, err = rl.Locate(ctx, "route-1")
		require.Error(t, err)
	}

	// 3. Breaker now open: the decision is refused without touching the store.
	before := store.callCount()
	_, _, err = rl.Locate(ctx, "route-1")
	require.ErrorIs(t, err, shared.ErrUnavailable)
	assert.Equal(t, before, store.callCount(), "an open breaker must not call the store")

	assert.Equal(t, []string{
		reasonLeaseUnowned,
		reasonStoreUnavailable,
		reasonStoreUnavailable,
		reasonStoreBreakerOpen,
	}, unknownReasons(t, rec))
}

// TestLocator_FailOpenPostureStillDiscloses_ProductionContract guards the
// dangerous half of the tradeoff: with FailOpen the locator processes locally
// during an unverifiable window (accepting transient duplicate processing), so
// the disclosure matters MORE, not less — the decision must still be metered.
func TestLocator_FailOpenPostureStillDiscloses_ProductionContract(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	rec := &ports.RecordingExporter{}
	store := &stubLeaseStore{}
	store.setInfo(persistence.LeaseInfo{
		Owner:     "instance-remote",
		Version:   1,
		ExpiresAt: fake.Now().Add(-time.Second),
	})
	rl := NewLocator("instance-local", store, LocatorConfig{
		CacheTTL:       time.Second,
		MaxFailures:    100,
		CooldownPeriod: time.Hour,
		FailOpen:       true,
		Metrics:        rec,
	}, fake)
	rl.RegisterRoute("route-1", "sess-1")

	_, local, err := rl.Locate(context.Background(), "route-1")
	require.NoError(t, err)
	assert.True(t, local, "fail-open trades exclusivity for availability")
	assert.Equal(t, []string{reasonLeaseExpired}, unknownReasons(t, rec),
		"optimistic local processing during an unverifiable window must still be disclosed")
}
