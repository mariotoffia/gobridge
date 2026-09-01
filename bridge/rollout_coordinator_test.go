package bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memorylease"
	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// errStoreUnavailable simulates a transient store outage.
var errStoreUnavailable = errors.New("rollout store unavailable")

// flakyRolloutStore wraps a real memoryrollout.Store and can inject a Current
// outage or a Commit rejection, so coordinator-step tests exercise how
// the coordinator HANDLES a failing/fencing store — the store's own enforcement
// is covered by the domain + conformance suites.
type flakyRolloutStore struct {
	inner     *memoryrollout.Store
	failReads bool  // while true, Current returns errStoreUnavailable
	commitErr error // while non-nil, Commit returns this instead of deciding
}

func (f *flakyRolloutStore) Propose(ctx context.Context, p persistence.RolloutProposal) (persistence.Rollout, error) {
	return f.inner.Propose(ctx, p)
}
func (f *flakyRolloutStore) Ack(ctx context.Context, gen uint64, m, d string) error {
	return f.inner.Ack(ctx, gen, m, d)
}
func (f *flakyRolloutStore) Nack(ctx context.Context, gen uint64, m, r string) error {
	return f.inner.Nack(ctx, gen, m, r)
}
func (f *flakyRolloutStore) Commit(ctx context.Context, gen uint64, tok persistence.LeaseToken) error {
	if f.commitErr != nil {
		return f.commitErr
	}
	return f.inner.Commit(ctx, gen, tok)
}
func (f *flakyRolloutStore) Abort(ctx context.Context, gen uint64, tok persistence.LeaseToken, r string) error {
	return f.inner.Abort(ctx, gen, tok, r)
}
func (f *flakyRolloutStore) Current(ctx context.Context) (persistence.Rollout, error) {
	if f.failReads {
		return persistence.Rollout{}, errStoreUnavailable
	}
	return f.inner.Current(ctx)
}
func (f *flakyRolloutStore) PutCommittedConfig(ctx context.Context, cfg persistence.CommittedRolloutConfig) error {
	return f.inner.PutCommittedConfig(ctx, cfg)
}
func (f *flakyRolloutStore) CommittedConfig(ctx context.Context) (persistence.CommittedRolloutConfig, error) {
	if f.failReads {
		return persistence.CommittedRolloutConfig{}, errStoreUnavailable
	}
	return f.inner.CommittedConfig(ctx)
}
func (f *flakyRolloutStore) Converge(ctx context.Context, gen uint64, m string) error {
	return f.inner.Converge(ctx, gen, m)
}
func (f *flakyRolloutStore) Confirm(ctx context.Context, gen uint64, tok persistence.LeaseToken) error {
	return f.inner.Confirm(ctx, gen, tok)
}
func (f *flakyRolloutStore) Revert(ctx context.Context, gen uint64, tok persistence.LeaseToken, r string) error {
	return f.inner.Revert(ctx, gen, tok, r)
}

// proposeAndAck opens a rollout over epoch and records acks/nacks against a real
// memory store, returning the store ready for a coordinator observation.
func proposeAndAck(t *testing.T, epoch, ackers []string, nacks map[string]string) *memoryrollout.Store {
	t.Helper()
	st := memoryrollout.NewStore(memoryrollout.WithClock(clocktest.NewAt(rolloutBase)))
	ctx := context.Background()
	r, err := st.Propose(ctx, persistence.RolloutProposal{
		ProposerID:   "proposer",
		ConfigDigest: "digest",
		Members:      epoch,
		TTL:          5 * time.Minute,
	})
	require.NoError(t, err)
	for _, m := range ackers {
		require.NoError(t, st.Ack(ctx, r.Generation(), m, "build-"+m))
	}
	for m, reason := range nacks {
		require.NoError(t, st.Nack(ctx, r.Generation(), m, reason))
	}
	return st
}

// coordTok is a valid coordinator fencing token for the step tests.
var coordTok = persistence.LeaseToken{Owner: "coordinator", Version: 1}

// TestCoordinatorStep_CommitsFullyAckedRollout validates: a coordinator
// (here a successor observing state a predecessor left) drives a fully-acked
// rollout to Committed through the real store.
func TestCoordinatorStep_CommitsFullyAckedRollout(t *testing.T) {
	st := proposeAndAck(t, []string{"a", "b"}, []string{"a", "b"}, nil)
	ctx := context.Background()

	terminal, err := coordinatorStep(ctx, nil, st, []string{"a", "b"}, coordTok, rolloutBase.Add(time.Minute))

	require.NoError(t, err)
	require.True(t, terminal)
	cur, err := st.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, persistence.RolloutCommitted, cur.State())
}

// TestCoordinatorStep_AbortsOnNack validates driven through the store: a
// nacked rollout is aborted, carrying the nack reason.
func TestCoordinatorStep_AbortsOnNack(t *testing.T) {
	st := proposeAndAck(t, []string{"a", "b"}, []string{"a"}, map[string]string{"b": "bad plugin"})
	ctx := context.Background()

	terminal, err := coordinatorStep(ctx, nil, st, []string{"a", "b"}, coordTok, rolloutBase.Add(time.Minute))

	require.NoError(t, err)
	require.True(t, terminal)
	cur, err := st.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, persistence.RolloutAborted, cur.State())
	require.Contains(t, cur.Reason(), "bad plugin")
}

// TestCoordinatorStep_WaitsWhenNoRollout validates that an empty store (no
// rollout ever proposed) is a no-op, not an error.
func TestCoordinatorStep_WaitsWhenNoRollout(t *testing.T) {
	st := memoryrollout.NewStore(memoryrollout.WithClock(clocktest.NewAt(rolloutBase)))

	terminal, err := coordinatorStep(context.Background(), nil, st, []string{"a"}, coordTok, rolloutBase)

	require.NoError(t, err)
	require.False(t, terminal)
}

// TestCoordinatorStep_SurfacesStaleFencingToken validates handling: when the
// store rejects the Commit because this coordinator was deposed, the step does
// NOT report the rollout terminal and surfaces the stale-token error so the loop
// can step down.
func TestCoordinatorStep_SurfacesStaleFencingToken(t *testing.T) {
	inner := proposeAndAck(t, []string{"a"}, []string{"a"}, nil)
	st := &flakyRolloutStore{inner: inner, commitErr: shared.ErrStaleFencingToken}

	terminal, err := coordinatorStep(context.Background(), nil, st, []string{"a"}, coordTok, rolloutBase.Add(time.Minute))

	require.False(t, terminal)
	require.ErrorIs(t, err, shared.ErrStaleFencingToken)
}

// TestCoordinatorStep_StoreUnavailableThenResolves validates: while the store
// is unavailable the coordinator makes no decision and surfaces the outage; once
// the store returns, the same observation commits.
func TestCoordinatorStep_StoreUnavailableThenResolves(t *testing.T) {
	inner := proposeAndAck(t, []string{"a"}, []string{"a"}, nil)
	st := &flakyRolloutStore{inner: inner, failReads: true}
	ctx := context.Background()

	terminal, err := coordinatorStep(ctx, nil, st, []string{"a"}, coordTok, rolloutBase.Add(time.Minute))
	require.False(t, terminal)
	require.ErrorIs(t, err, errStoreUnavailable)

	st.failReads = false // store recovers
	terminal, err = coordinatorStep(ctx, nil, st, []string{"a"}, coordTok, rolloutBase.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, terminal)
	cur, err := st.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, persistence.RolloutCommitted, cur.State())
}

// TestFirstSideEffectAllowed pins the lock-delay gate: a coordinator elected at
// electedAt may take a side effect only once one lock-delay has elapsed.
func TestFirstSideEffectAllowed(t *testing.T) {
	electedAt := rolloutBase
	lockDelay := 30 * time.Second

	require.False(t, firstSideEffectAllowed(electedAt, lockDelay, electedAt))
	require.False(t, firstSideEffectAllowed(electedAt, lockDelay, electedAt.Add(29*time.Second)))
	require.True(t, firstSideEffectAllowed(electedAt, lockDelay, electedAt.Add(30*time.Second)))
	require.True(t, firstSideEffectAllowed(electedAt, lockDelay, electedAt.Add(time.Minute)))
}

// TestRolloutCoordinator_LockDelayDefersFirstSideEffect validates the Chubby-
// style lock-delay (design §6): a freshly elected coordinator does not commit a
// committable rollout until it has waited out one previous-lease duration, even
// though the barrier is already satisfied. Deterministic via clocktest — the
// coordinator's observe() is caller-driven, no goroutine.
func TestRolloutCoordinator_LockDelayDefersFirstSideEffect(t *testing.T) {
	fake := clocktest.NewAt(rolloutBase)
	rolloutSt := proposeAndAck(t, []string{"a"}, []string{"a"}, nil) // ready to commit
	leaseSt := memorylease.NewStore(
		memorylease.WithClock(fake),
		memorylease.WithAcknowledgeSingleReplica(true), // single-process unit test
	)
	ctx := context.Background()

	c := newRolloutCoordinator(rolloutCoordinatorConfig{
		Store:      rolloutSt,
		Lease:      leaseSt,
		Membership: func() []string { return []string{"a"} },
		Clock:      fake,
		MemberID:   "node-1",
		LeaseTTL:   30 * time.Second,
	})
	require.NoError(t, c.elect(ctx))

	// Within the lock-delay window: no side effect, rollout stays Staging.
	fake.Advance(10 * time.Second)
	terminal, err := c.observe(ctx)
	require.NoError(t, err)
	require.False(t, terminal)
	cur, err := rolloutSt.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, persistence.RolloutStaging, cur.State(), "must not commit during lock-delay")

	// Past the lock-delay: the coordinator commits.
	fake.Advance(21 * time.Second) // now electedAt+31s > 30s TTL
	terminal, err = c.observe(ctx)
	require.NoError(t, err)
	require.True(t, terminal)
	cur, err = rolloutSt.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, persistence.RolloutCommitted, cur.State())
}

// TestRolloutCoordinator_ObserveBeforeElectIsNoOp validates the caller contract:
// a coordinator that has not (yet) won the election holds no valid fencing token,
// so observe() takes no side effect — it neither bypasses the lock-delay nor
// attempts a store write against a committable rollout.
func TestRolloutCoordinator_ObserveBeforeElectIsNoOp(t *testing.T) {
	fake := clocktest.NewAt(rolloutBase)
	rolloutSt := proposeAndAck(t, []string{"a"}, []string{"a"}, nil) // committable
	c := newRolloutCoordinator(rolloutCoordinatorConfig{
		Store:      rolloutSt,
		Membership: func() []string { return []string{"a"} },
		Clock:      fake,
		MemberID:   "node-1",
		LeaseTTL:   30 * time.Second,
	})

	terminal, err := c.observe(context.Background()) // no elect() first
	require.NoError(t, err)
	require.False(t, terminal)
	cur, err := rolloutSt.Current(context.Background())
	require.NoError(t, err)
	require.Equal(t, persistence.RolloutStaging, cur.State(), "an unelected coordinator must not commit")
}

// TestRolloutCoordinator_SuccessorResumesAfterCrash validates: the first
// coordinator elects and then crashes before deciding; after its lease expires a
// successor elects and, past its own lock-delay, drives the in-progress rollout
// left in the store to Committed. All state is durable, so resume is election +
// observation — no repair, no coordination handoff.
func TestRolloutCoordinator_SuccessorResumesAfterCrash(t *testing.T) {
	fake := clocktest.NewAt(rolloutBase)
	rolloutSt := proposeAndAck(t, []string{"a", "b"}, []string{"a", "b"}, nil) // fully acked
	leaseSt := memorylease.NewStore(
		memorylease.WithClock(fake),
		memorylease.WithAcknowledgeSingleReplica(true),
	)
	ctx := context.Background()
	membership := func() []string { return []string{"a", "b"} }
	const ttl = 30 * time.Second

	// Coordinator 1 elects, then crashes before observing.
	c1 := newRolloutCoordinator(rolloutCoordinatorConfig{
		Store: rolloutSt, Lease: leaseSt, Membership: membership,
		Clock: fake, MemberID: "node-1", LeaseTTL: ttl,
	})
	require.NoError(t, c1.elect(ctx))

	// Lease TTL expires; a successor takes over.
	fake.Advance(ttl + time.Second)
	c2 := newRolloutCoordinator(rolloutCoordinatorConfig{
		Store: rolloutSt, Lease: leaseSt, Membership: membership,
		Clock: fake, MemberID: "node-2", LeaseTTL: ttl,
	})
	require.NoError(t, c2.elect(ctx))

	// Immediately after electing, the successor is inside its own lock-delay.
	terminal, err := c2.observe(ctx)
	require.NoError(t, err)
	require.False(t, terminal)

	// Past the lock-delay, the successor commits the rollout its predecessor left.
	fake.Advance(ttl + time.Second)
	terminal, err = c2.observe(ctx)
	require.NoError(t, err)
	require.True(t, terminal)
	cur, err := rolloutSt.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, persistence.RolloutCommitted, cur.State())
}

// rolloutBase is a fixed, deterministic reference time for coordinator tests.
// clocktest and these fixtures share no global clock, so the value is arbitrary
// as long as before/after-deadline offsets are computed from it.
var rolloutBase = time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

// mkStaging builds a Staging rollout over the sorted epoch, acked by `ackers`,
// with the given deadline. A rollout acked by its whole epoch reports
// CanCommit()==true; acked by a subset stays incomplete.
func mkStaging(t *testing.T, epoch []string, deadline time.Time, ackers ...string) persistence.Rollout {
	t.Helper()
	r, err := persistence.NewRollout(1, persistence.RolloutProposal{
		ProposerID:   "proposer",
		ConfigDigest: "digest",
		Members:      epoch,
	}, deadline)
	require.Nil(t, err)
	for _, m := range ackers {
		r, err = r.WithAck(m, "build-"+m, rolloutBase)
		require.Nil(t, err)
	}
	return r
}

// TestDecideRollout_CommitWhenAllAckedBeforeDeadline validates the happy path:
// every epoch member has acked, membership is unchanged, deadline not reached →
// the coordinator commits.
func TestDecideRollout_CommitWhenAllAckedBeforeDeadline(t *testing.T) {
	deadline := rolloutBase.Add(5 * time.Minute)
	r := mkStaging(t, []string{"a", "b"}, deadline, "a", "b")

	action, reason := decideRollout(r, []string{"a", "b"}, rolloutBase.Add(time.Minute))

	require.Equal(t, rolloutActionCommit, action)
	require.Empty(t, reason)
}

// TestDecideRollout_WaitWhenAcksIncompleteBeforeDeadline validates that an
// incomplete but healthy rollout is left to run — no commit, no abort.
func TestDecideRollout_WaitWhenAcksIncompleteBeforeDeadline(t *testing.T) {
	deadline := rolloutBase.Add(5 * time.Minute)
	r := mkStaging(t, []string{"a", "b"}, deadline, "a") // b has not acked

	action, _ := decideRollout(r, []string{"a", "b"}, rolloutBase.Add(time.Minute))

	require.Equal(t, rolloutActionWait, action)
}

// TestDecideRollout_AbortWhenDeadlineExceededIncomplete validates: a member
// that never acks lets the deadline pass → the coordinator aborts.
func TestDecideRollout_AbortWhenDeadlineExceededIncomplete(t *testing.T) {
	deadline := rolloutBase.Add(5 * time.Minute)
	r := mkStaging(t, []string{"a", "b"}, deadline, "a") // b never acks

	action, reason := decideRollout(r, []string{"a", "b"}, deadline.Add(time.Second))

	require.Equal(t, rolloutActionAbort, action)
	require.NotEmpty(t, reason)
}

// TestDecideRollout_AbortOnNackBeforeDeadline validates: any Nack aborts the
// rollout immediately, without waiting for the deadline.
func TestDecideRollout_AbortOnNackBeforeDeadline(t *testing.T) {
	deadline := rolloutBase.Add(5 * time.Minute)
	r := mkStaging(t, []string{"a", "b"}, deadline, "a")
	r, err := r.WithNack("b", "plugin build failed")
	require.Nil(t, err)

	action, reason := decideRollout(r, []string{"a", "b"}, rolloutBase.Add(time.Minute))

	require.Equal(t, rolloutActionAbort, action)
	require.Contains(t, reason, "plugin build failed")
}

// TestDecideRollout_AbortOnMembershipChangeEvenIfAllAcked validates: the
// epoch is frozen at Propose; if live membership diverges (here a joiner) the
// coordinator aborts — even though every ORIGINAL epoch member has acked. This
// proves the membership check precedes the commit check.
func TestDecideRollout_AbortOnMembershipChangeEvenIfAllAcked(t *testing.T) {
	deadline := rolloutBase.Add(5 * time.Minute)
	r := mkStaging(t, []string{"a", "b"}, deadline, "a", "b") // epoch fully acked

	action, reason := decideRollout(r, []string{"a", "b", "c"}, rolloutBase.Add(time.Minute))

	require.Equal(t, rolloutActionAbort, action)
	require.NotEmpty(t, reason)
}

// TestDecideRollout_DuplicateMembershipIsNotAChange validates robustness to a
// membership source that transiently reports a duplicate member id (e.g. a
// restarting node whose fresh heartbeat lands before its old row TTLs out): the
// live roster is a SET, so [a,a,b] equals epoch [a,b] and a fully-acked rollout
// commits rather than aborting on routine node churn.
func TestDecideRollout_DuplicateMembershipIsNotAChange(t *testing.T) {
	deadline := rolloutBase.Add(5 * time.Minute)
	r := mkStaging(t, []string{"a", "b"}, deadline, "a", "b")

	action, reason := decideRollout(r, []string{"a", "a", "b"}, rolloutBase.Add(time.Minute))

	require.Equal(t, rolloutActionCommit, action, "a duplicate member id must not read as a membership change (reason=%q)", reason)
}

// TestDecideRollout_WaitOnTerminal validates that a decided rollout yields no
// further action — the coordinator loop exits, it does not re-commit/re-abort.
func TestDecideRollout_WaitOnTerminal(t *testing.T) {
	deadline := rolloutBase.Add(5 * time.Minute)
	r := mkStaging(t, []string{"a"}, deadline, "a")
	tok := persistence.LeaseToken{Owner: "coord", Version: 1}
	committed, err := r.WithCommit(tok)
	require.Nil(t, err)

	action, _ := decideRollout(committed, []string{"a"}, rolloutBase.Add(time.Minute))

	require.Equal(t, rolloutActionWait, action)
}
