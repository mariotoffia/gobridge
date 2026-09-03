package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

// A member with nothing to prove does not get to go first.
//
// Every session on a lease-based cohort defers its connect until this instance
// wins the lease, and immediately after a provisional swap no member holds one —
// not even the member that is about to. The readiness fold skips such a session,
// so every member folds to "ready" over an empty set. If that counted as
// convergence the whole cohort would record it seconds after the swap, the
// coordinator would confirm, and the broker's verdict would arrive afterwards
// against a generation that is by then permanent — which is the one outcome the
// confirm window exists to prevent.
//
// So a member whose readiness rests on nothing waits for a member whose readiness
// rests on something to go first. If none ever does, nobody converges and the
// window expires, which is the right answer for a change nothing could verify.

// windowedApplier builds a member sitting on an open confirm window for version,
// with the candidate already staged.
func windowedApplier(t *testing.T, version int) (*rolloutApplier, *fakeRolloutHost, *memoryrollout.Store) {
	t.Helper()
	fake := clocktest.NewAt(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	store := memoryrollout.NewStore(memoryrollout.WithClock(fake))

	host := newFakeRolloutHost(soloWindowConfig(0, time.Minute))
	candidate := soloWindowConfig(version, time.Minute)
	digest, ok := configCanonicalBytesDigest(candidate)
	require.True(t, ok)

	barrier := &rolloutBarrier{store: store, memberID: "node-a", pollInterval: time.Second, ops: newRolloutOps(0)}
	barrier.stage(digest, candidate, candidate)
	applier := &rolloutApplier{
		host: host, barrier: barrier, store: store, memberID: "node-a",
		clk: fake, obs: newRolloutObserver(nil, fake, time.Second, "node-a"),
	}
	seedCohortWindowedCommit(t, store, digest, version)
	return applier, host, store
}

// seedCohortWindowedCommit puts a TWO-member cohort into an open confirm window.
// The second member matters: a solo epoch cannot express "somebody else proved
// it", which is the whole rule under test.
func seedCohortWindowedCommit(t *testing.T, store *memoryrollout.Store, digest string, version int) {
	t.Helper()
	ctx := context.Background()
	r, err := store.Propose(ctx, persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: digest, ConfigVersion: version,
		Members: []string{"node-a", "node-b"}, TTL: time.Hour, ConfirmWindow: time.Minute,
	})
	require.NoError(t, err)
	require.NoError(t, store.Ack(ctx, r.Generation(), "node-a", digest))
	require.NoError(t, store.Ack(ctx, r.Generation(), "node-b", digest))
	require.NoError(t, store.Commit(ctx, r.Generation(),
		persistence.LeaseToken{Owner: "coord", Version: 1}))
}

func convergedMembers(t *testing.T, store *memoryrollout.Store) []string {
	t.Helper()
	r, err := store.Current(context.Background())
	require.NoError(t, err)
	return sortedKeys(r.Converged())
}

// TestRecordConvergence_ReadyOverNothingDoesNotConvergeFirst is the case that
// made the confirm window unable to bite on the profile it ships with.
func TestRecordConvergence_ReadyOverNothingDoesNotConvergeFirst(t *testing.T) {
	applier, host, store := windowedApplier(t, 7)
	host.unprovable = true

	require.NoError(t, applier.tick(context.Background()))
	require.Equal(t, 7, host.Config().Version, "precondition: the provisional swap landed")

	require.Empty(t, convergedMembers(t, store),
		"a member that observed nothing recorded convergence anyway, so the coordinator would "+
			"confirm a config nobody has tried")
}

// TestRecordConvergence_AProvableMemberGoesFirst keeps the ordinary path intact:
// a member whose readiness rests on a session it observed records convergence
// without waiting for anybody.
func TestRecordConvergence_AProvableMemberGoesFirst(t *testing.T) {
	applier, _, store := windowedApplier(t, 7)

	require.NoError(t, applier.tick(context.Background()))

	require.Equal(t, []string{"node-a"}, convergedMembers(t, store))
}

// TestRecordConvergence_FollowsAMemberThatProvedIt is the other half. Once a peer
// has demonstrated the config can serve, a member that can never demonstrate it
// itself — a genuine warm standby — is free to agree, so a healthy change still
// confirms instead of always reverting.
func TestRecordConvergence_FollowsAMemberThatProvedIt(t *testing.T) {
	applier, host, store := windowedApplier(t, 7)
	host.unprovable = true
	ctx := context.Background()

	require.NoError(t, applier.tick(ctx))
	require.Empty(t, convergedMembers(t, store), "precondition: it waited")

	current, err := store.Current(ctx)
	require.NoError(t, err)
	require.NoError(t, store.Converge(ctx, current.Generation(), "node-b"))

	require.NoError(t, applier.tick(ctx))
	require.Equal(t, []string{"node-a", "node-b"}, convergedMembers(t, store),
		"a member with nothing to prove must be able to follow the peer that proved it")
}
