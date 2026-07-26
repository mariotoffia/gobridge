package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/store/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
)

const testCoordLeaseTTL = 30 * time.Second

// coordFixture is a caller-driven coordinator over a real memory rollout store
// and the election lease fake, on a fake clock. Every test drives tick()
// explicitly and advances the clock explicitly, so no assertion depends on
// wall-clock timing.
type coordFixture struct {
	store *memoryrollout.Store
	lease *electionLeaseStore
	clk   *clocktest.Fake
	coord *rolloutCoordinator
}

func newCoordFixture(t *testing.T, memberID string, members ...string) *coordFixture {
	t.Helper()
	// The store stamps rollout deadlines from ITS clock and the coordinator
	// compares against ITS clock, so both must be the same fake — otherwise a
	// deadline lands decades away from "now" and F1 is untestable.
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	store := memoryrollout.NewStore(memoryrollout.WithClock(clk))
	lease := newElectionLeaseStore()
	return &coordFixture{
		store: store,
		lease: lease,
		clk:   clk,
		coord: newRolloutCoordinator(rolloutCoordinatorConfig{
			Store:      store,
			Lease:      lease,
			Membership: func() []string { return members },
			Clock:      clk,
			MemberID:   memberID,
			LeaseTTL:   testCoordLeaseTTL,
		}),
	}
}

// propose opens a rollout with the given epoch and a generous deadline.
func (f *coordFixture) propose(t *testing.T, members ...string) persistence.Rollout {
	t.Helper()
	r, err := f.store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: "digest-1", ConfigVersion: 42,
		Members: members, TTL: time.Hour,
	})
	require.NoError(t, err)
	return r
}

// elected runs the election tick and the lock-delay wait, leaving the
// coordinator able to take a side effect on its next tick.
func (f *coordFixture) elected(t *testing.T) {
	t.Helper()
	require.NoError(t, f.coord.tick(context.Background()), "election tick")
	require.True(t, f.coord.tok.Valid(), "the coordinator must hold a fencing token after electing")
	f.clk.Advance(testCoordLeaseTTL)
}

// TestRolloutCoordinatorTick_LosingTheElectionIsNotAnError proves the normal
// state for all but one member. Every node runs the drive, so a lost election
// must be silent — reporting it as an error would make an N-member cohort log
// N-1 errors on every single tick.
func TestRolloutCoordinatorTick_LosingTheElectionIsNotAnError(t *testing.T) {
	f := newCoordFixture(t, "node-a", "node-a", "node-b")
	_, err := f.lease.Acquire(context.Background(), coordLeaseID, "node-b", testCoordLeaseTTL, nil)
	require.NoError(t, err)

	require.NoError(t, f.coord.tick(context.Background()))
	assert.False(t, f.coord.tok.Valid(), "a member that lost the election holds no fencing token")
}

// TestRolloutCoordinatorTick_LockDelayDefersTheFirstDecision proves the
// Chubby-style lock delay (§6): a freshly elected coordinator takes NO side
// effect until one full previous-lease duration has passed, so a predecessor
// that has not yet noticed its lease lapsed cannot be racing it.
func TestRolloutCoordinatorTick_LockDelayDefersTheFirstDecision(t *testing.T) {
	f := newCoordFixture(t, "node-a", "node-a")
	r := f.propose(t, "node-a")
	require.NoError(t, f.store.Ack(context.Background(), r.Generation(), "node-a", "build"))

	require.NoError(t, f.coord.tick(context.Background())) // elect
	require.NoError(t, f.coord.tick(context.Background())) // still inside the lock delay

	got, err := f.store.Current(context.Background())
	require.NoError(t, err)
	assert.Equal(t, persistence.RolloutStaging, got.State(),
		"a fully-acked rollout must NOT be committed inside the lock delay")

	f.clk.Advance(testCoordLeaseTTL)
	require.NoError(t, f.coord.tick(context.Background()))

	got, err = f.store.Current(context.Background())
	require.NoError(t, err)
	assert.Equal(t, persistence.RolloutCommitted, got.State(),
		"once the lock delay elapses the coordinator decides")
}

// TestRolloutCoordinatorTick_CommitsWhenTheEpochIsAcked proves the barrier's
// central invariant end to end through the loop (I2): the coordinator commits
// exactly when acks cover the frozen epoch, and not before.
func TestRolloutCoordinatorTick_CommitsWhenTheEpochIsAcked(t *testing.T) {
	f := newCoordFixture(t, "node-a", "node-a", "node-b")
	r := f.propose(t, "node-a", "node-b")
	f.elected(t)

	require.NoError(t, f.store.Ack(context.Background(), r.Generation(), "node-a", "build"))
	require.NoError(t, f.coord.tick(context.Background()))
	got, err := f.store.Current(context.Background())
	require.NoError(t, err)
	require.Equal(t, persistence.RolloutStaging, got.State(),
		"a partially-acked epoch must never commit — that is the whole barrier")

	require.NoError(t, f.store.Ack(context.Background(), r.Generation(), "node-b", "build"))
	require.NoError(t, f.coord.tick(context.Background()))

	got, err = f.store.Current(context.Background())
	require.NoError(t, err)
	assert.Equal(t, persistence.RolloutCommitted, got.State())
}

// TestRolloutCoordinatorTick_AbortsOnNack proves F2 through the loop: a single
// Nack aborts the rollout immediately rather than waiting out the deadline, so
// an operator learns a member rejected the change in seconds, not minutes.
func TestRolloutCoordinatorTick_AbortsOnNack(t *testing.T) {
	f := newCoordFixture(t, "node-a", "node-a", "node-b")
	r := f.propose(t, "node-a", "node-b")
	f.elected(t)

	require.NoError(t, f.store.Ack(context.Background(), r.Generation(), "node-a", "build"))
	require.NoError(t, f.store.Nack(context.Background(), r.Generation(), "node-b", "cannot build"))
	require.NoError(t, f.coord.tick(context.Background()))

	got, err := f.store.Current(context.Background())
	require.NoError(t, err)
	assert.Equal(t, persistence.RolloutAborted, got.State())
	assert.Contains(t, got.Reason(), "cannot build", "the abort reason must carry the member's nack")
}

// TestRolloutCoordinatorTick_AbortsOnDeadline proves F1 through the loop: a
// rollout whose members never all ack is aborted at its deadline, so a crashed
// member cannot block the cohort's config indefinitely (invariant I1 permits
// only one active rollout, so a wedged one would block every future change).
func TestRolloutCoordinatorTick_AbortsOnDeadline(t *testing.T) {
	f := newCoordFixture(t, "node-a", "node-a", "node-b")
	r, err := f.store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: "digest-1", ConfigVersion: 42,
		Members: []string{"node-a", "node-b"}, TTL: time.Minute,
	})
	require.NoError(t, err)
	require.NoError(t, f.store.Ack(context.Background(), r.Generation(), "node-a", "build"))
	f.elected(t)

	require.NoError(t, f.coord.tick(context.Background()))
	got, err := f.store.Current(context.Background())
	require.NoError(t, err)
	require.Equal(t, persistence.RolloutStaging, got.State(), "not yet past the deadline")

	f.clk.Advance(2 * time.Minute)
	require.NoError(t, f.coord.tick(context.Background()))

	got, err = f.store.Current(context.Background())
	require.NoError(t, err)
	assert.Equal(t, persistence.RolloutAborted, got.State())
	assert.Contains(t, got.Reason(), "deadline")
}

// TestRolloutCoordinatorTick_StepsDownWhenDeposed proves F5 through the loop. A
// coordinator whose lease was taken over must stop acting: its renewal is
// rejected under the stale token, it drops the token, and it takes no further
// side effect. Continuing would mean two coordinators deciding one rollout.
func TestRolloutCoordinatorTick_StepsDownWhenDeposed(t *testing.T) {
	f := newCoordFixture(t, "node-a", "node-a", "node-b")
	r := f.propose(t, "node-a", "node-b")
	f.elected(t)
	require.NoError(t, f.store.Ack(context.Background(), r.Generation(), "node-a", "build"))
	require.NoError(t, f.store.Ack(context.Background(), r.Generation(), "node-b", "build"))

	f.lease.depose("node-b")

	require.NoError(t, f.coord.tick(context.Background()),
		"a deposed coordinator steps down quietly; it does not fight the live one")
	assert.False(t, f.coord.tok.Valid(), "the stale fencing token is dropped")

	got, err := f.store.Current(context.Background())
	require.NoError(t, err)
	assert.Equal(t, persistence.RolloutStaging, got.State(),
		"the deposed coordinator must not have decided; node-b now owns the decision")
}

// TestRolloutCoordinatorTick_RenewsBeforeDeciding pins the mitigation that makes
// the F5b residual acceptable (see coordinatorStep's F5b note). The store fence
// cannot reject a deposed coordinator's FIRST decision, so the loop must not
// give it the chance: every tick renews the lease BEFORE observing, and a
// rejected renewal steps down without any decision write.
//
// Without this ordering a deposed coordinator would decide a full poll interval
// after losing the lease, concurrently with the successor.
func TestRolloutCoordinatorTick_RenewsBeforeDeciding(t *testing.T) {
	f := newCoordFixture(t, "node-a", "node-a")
	r := f.propose(t, "node-a")
	f.elected(t)
	// A fully-acked rollout: the ONLY thing standing between this coordinator
	// and a commit is the renewal check.
	require.NoError(t, f.store.Ack(context.Background(), r.Generation(), "node-a", "build"))
	f.lease.depose("node-b")

	require.NoError(t, f.coord.tick(context.Background()))

	got, err := f.store.Current(context.Background())
	require.NoError(t, err)
	assert.Equal(t, persistence.RolloutStaging, got.State(),
		"a coordinator that lost its lease must not decide, even with the barrier satisfied")
	assert.False(t, f.coord.tok.Valid())
}

// TestRolloutCoordinatorTick_ReElectsAfterStepDown proves the successor path
// (F3): once the lease is free again the same member can win it back and resume
// from durable store state — recovery is resumption, not repair.
func TestRolloutCoordinatorTick_ReElectsAfterStepDown(t *testing.T) {
	f := newCoordFixture(t, "node-a", "node-a")
	r := f.propose(t, "node-a")
	f.elected(t)
	f.lease.depose("node-b")
	require.NoError(t, f.coord.tick(context.Background()))
	require.False(t, f.coord.tok.Valid())

	// The usurper's lease lapses; node-a wins the next election and, after the
	// lock delay, resumes the rollout it never finished.
	require.NoError(t, f.lease.Release(context.Background(), coordLeaseID,
		persistence.LeaseToken{Owner: "node-b", Version: f.lease.version}))
	require.NoError(t, f.store.Ack(context.Background(), r.Generation(), "node-a", "build"))

	f.elected(t)
	require.NoError(t, f.coord.tick(context.Background()))

	got, err := f.store.Current(context.Background())
	require.NoError(t, err)
	assert.Equal(t, persistence.RolloutCommitted, got.State(),
		"a re-elected coordinator resumes from store state and decides")
}

// TestRolloutCoordinator_ResignReleasesTheLease proves an orderly shutdown hands
// the coordinator role over immediately instead of making the cohort wait out
// the full lease TTL. F3 (crash) is what the TTL is for; a clean stop should not
// pay it.
func TestRolloutCoordinator_ResignReleasesTheLease(t *testing.T) {
	f := newCoordFixture(t, "node-a", "node-a")
	f.elected(t)
	require.Equal(t, "node-a", f.lease.held())

	f.coord.resign(context.Background())

	assert.Empty(t, f.lease.held(), "an orderly stop releases the coordinator lease")
	assert.False(t, f.coord.tok.Valid())
}

// TestRolloutCoordinator_ResignIsSafeWithoutALease proves resign on a member
// that never won the election is a no-op. Every member's drive calls it on
// shutdown, so all but one are in exactly this state.
func TestRolloutCoordinator_ResignIsSafeWithoutALease(t *testing.T) {
	f := newCoordFixture(t, "node-a", "node-a")
	f.coord.resign(context.Background())
	assert.Empty(t, f.lease.held())
}

// TestRolloutCoordinatorTick_MembershipChangeAborts proves F6 through the loop:
// a live roster that no longer matches the frozen epoch makes strict all-ack
// unachievable, so the rollout aborts cheaply (nothing has swapped) rather than
// committing under a roster the cohort no longer has.
func TestRolloutCoordinatorTick_MembershipChangeAborts(t *testing.T) {
	roster := []string{"node-a", "node-b"}
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	store := memoryrollout.NewStore(memoryrollout.WithClock(clk))
	lease := newElectionLeaseStore()
	coord := newRolloutCoordinator(rolloutCoordinatorConfig{
		Store: store, Lease: lease,
		Membership: func() []string { return roster },
		Clock:      clk, MemberID: "node-a", LeaseTTL: testCoordLeaseTTL,
	})
	_, err := store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: "d", ConfigVersion: 1,
		Members: roster, TTL: time.Hour,
	})
	require.NoError(t, err)

	require.NoError(t, coord.tick(context.Background()))
	clk.Advance(testCoordLeaseTTL)
	roster = []string{"node-a", "node-b", "node-c"} // a joiner appears mid-rollout
	require.NoError(t, coord.tick(context.Background()))

	got, err := store.Current(context.Background())
	require.NoError(t, err)
	assert.Equal(t, persistence.RolloutAborted, got.State())
	assert.Contains(t, got.Reason(), "membership changed")
}

// TestStartRolloutDrive_NotStartedWithoutTheBarrier proves the drive is opt-in
// on BOTH axes: no rollout store wired, or a deployment that did not ask for
// coordinated rollout, starts nothing. A non-clustered bridge must not grow a
// background goroutine polling a store it does not have.
func TestStartRolloutDrive_NotStartedWithoutTheBarrier(t *testing.T) {
	t.Run("no barrier wired", func(t *testing.T) {
		s := newTestSupervisor()
		s.cfg = coordinatedClusteredCfg("r1")
		assert.Nil(t, s.startRolloutDrive(context.Background()))
	})
	t.Run("barrier wired but deployment is not coordinated", func(t *testing.T) {
		s := newTestSupervisor(WithClusterRollout(testRolloutConfig(memoryrollout.NewStore(), "node-a")))
		s.cfg = quickCfg("r1")
		assert.Nil(t, s.startRolloutDrive(context.Background()))
	})
}

// TestNewRolloutBarrier_RequiresCompleteWiring proves a half-wired barrier
// degrades to the ADR 0012 refusal instead of silently proposing into nothing.
// A cohort with no coordinator lease store would never DECIDE any rollout, so
// every change would sit until its TTL and expire.
func TestNewRolloutBarrier_RequiresCompleteWiring(t *testing.T) {
	store := memoryrollout.NewStore()
	lease := newElectionLeaseStore()

	assert.Nil(t, newRolloutBarrier(ClusterRolloutConfig{Lease: lease, MemberID: "node-a"}),
		"no rollout store")
	assert.Nil(t, newRolloutBarrier(ClusterRolloutConfig{Store: store, MemberID: "node-a"}),
		"no coordinator lease store: nothing would ever decide a rollout")
	assert.Nil(t, newRolloutBarrier(ClusterRolloutConfig{Store: store, Lease: lease}),
		"no member id")

	b := newRolloutBarrier(ClusterRolloutConfig{Store: store, Lease: lease, MemberID: "node-a"})
	require.NotNil(t, b)
	assert.Equal(t, defaultRolloutTTL, b.ttl)
	assert.Equal(t, defaultCoordLeaseTTL, b.leaseTTL)
	assert.Equal(t, defaultRolloutPollInterval, b.pollInterval)
}

var _ = ports.ClusterRolloutStore(nil)
