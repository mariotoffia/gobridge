package bridge

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Tests for the barrier's behaviour under TRANSIENT failure. They exist because
// the barrier turns local, recoverable problems into cohort-wide, permanent ones
// unless each is classified correctly: a vote is unretryable, an abort
// blocks the next change for a whole TTL, and a coordinator step-down costs a
// full lock delay.

// TestRolloutApplier_AbstainsWhenTheBuildFailsTransiently proves a transient
// build failure produces NO vote rather than a Nack.
//
// A Nack is permanent and unretryable and the coordinator aborts on the
// first one. Builder.Plan opens stores and resolves credentials, so it
// fails on a throttled store, a flaky credential provider, and — because the
// applier builds under the drive loop's context — an ordinary SIGTERM. Nacking
// those would let a single restarting member abort every in-flight rollout in
// the cohort, and the member could not take the vote back afterwards. Abstaining
// lets the deadline decide, which the operator can simply re-propose.
func TestRolloutApplier_AbstainsWhenTheBuildFailsTransiently(t *testing.T) {
	store := memoryrollout.NewStore()
	rc := testRolloutConfig(store, "node-a")
	rc.PollInterval = time.Hour // hand-stepped
	rc.LeaseTTL = time.Hour

	// Builder.Plan runs the blueprint validator first, then opens durable stores
	// and resolves credentials — every one of which can fail transiently. This
	// gate stands in for all of them: it passes the initial build and is
	// throttled for the applier's candidate build until it recovers.
	gate := &throttlingValidator{}
	s := newTestSupervisor(WithClusterRollout(rc), WithSupervisorBlueprintValidator(gate.validate))

	changes := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, soloCohortConfig(0), changes)
	t.Cleanup(func() { cancel(); <-errCh })

	cand := soloCohortConfig(99)
	cand.Bindings[0].Address = "addr/rolled"
	digest, ok := configCanonicalBytesDigest(cand)
	require.True(t, ok)
	s.rollout.stage(digest, cand, cand)
	_, err := store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: digest, ConfigVersion: 99,
		Members: []string{"node-a"}, TTL: time.Hour,
	})
	require.NoError(t, err)

	applier := &rolloutApplier{host: supervisorRolloutHost{s}, barrier: s.rollout, store: store, memberID: "node-a"}
	require.NoError(t, applier.step(context.Background()))

	got, err := store.Current(context.Background())
	require.NoError(t, err)
	assert.Empty(t, got.Nacks(),
		"a transient build failure must not cast the permanent vote that aborts the cohort's rollout")
	assert.Empty(t, got.Acks(), "nor may it ack a candidate it never managed to build")
	assert.Equal(t, persistence.RolloutProposed, got.State(),
		"the rollout stays open for the deadline to decide")

	// The same member votes normally once the store recovers — proof the
	// abstention left the rollout genuinely recoverable rather than merely
	// undecided.
	gate.recovered.Store(true)
	require.NoError(t, applier.step(context.Background()))
	got, err = store.Current(context.Background())
	require.NoError(t, err)
	assert.Contains(t, got.Acks(), "node-a")
}

// throttlingValidator passes the first build and reports throttling for every
// later one until recovered — a dependency that is rate-limited exactly while
// the applier is trying to prove the candidate.
type throttlingValidator struct {
	calls     atomic.Int32
	recovered atomic.Bool
}

func (v *throttlingValidator) validate(*ports.BridgeConfig) error {
	if v.calls.Add(1) > 1 && !v.recovered.Load() {
		return shared.ErrThrottled
	}
	return nil
}

// TestTransientBuildFailure_Classification pins which causes abstain. The
// default MUST be to nack: a genuinely bad config has to be rejected promptly
// rather than left to burn the full rollout deadline, so only positively
// identified transient causes abstain.
func TestTransientBuildFailure_Classification(t *testing.T) {
	live := context.Background()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"context cancelled", context.Canceled, true},
		{"context deadline", context.DeadlineExceeded, true},
		{"store throttled", shared.ErrThrottled, true},
		{"dependency unavailable", shared.ErrUnavailable, true},
		{"a genuinely invalid candidate", errors.New("route references unknown receiver"), false},
		{"a missing plugin", shared.ErrNotFound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, transientBuildFailure(live, tc.err))
		})
	}

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	assert.True(t, transientBuildFailure(dead, errors.New("wrapped without the sentinel")),
		"a dead context means shutdown regardless of how the builder wrapped the error")
}

// flakySenderFactory fails the first `failures` sender builds after the initial
// runtime, then succeeds — a broker refusing a connection during a swap.
type flakySenderFactory struct {
	fakeTransportFactory
	builds   atomic.Int32
	failures int32
}

func (f *flakySenderFactory) NewSender(_ context.Context, _ ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	n := f.builds.Add(1)
	if n > 1 && n <= 1+f.failures {
		return nil, errors.New("broker refused the connection")
	}
	return &fakeSender{}, nil
}

// TestRolloutApplier_RetriesAFailedAdopt proves a member whose post-commit swap
// fails RETRIES instead of stranding itself one generation behind the cohort.
//
// The barrier has already committed cluster-wide at this point, so a member that
// silently gives up leaves exactly the mixed-version cohort forbids — and
// the rollout row reads "committed" on every member, so nothing would reveal it.
// The common causes are transient, which is precisely why a retry converges.
func TestRolloutApplier_RetriesAFailedAdopt(t *testing.T) {
	store := memoryrollout.NewStore()
	rc := testRolloutConfig(store, "node-a")
	rc.PollInterval = time.Hour // hand-stepped
	rc.LeaseTTL = time.Hour
	tf := &flakySenderFactory{failures: 1}
	s := newTestSupervisorTransport(tf, WithClusterRollout(rc))

	changes := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, soloCohortConfig(0), changes)
	t.Cleanup(func() { cancel(); <-errCh })

	cand := soloCohortConfig(99)
	cand.Bindings[0].Address = "addr/rolled"
	digest, ok := configCanonicalBytesDigest(cand)
	require.True(t, ok)
	s.rollout.stage(digest, cand, cand)
	r, err := store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: digest, ConfigVersion: 99,
		Members: []string{"node-a"}, TTL: time.Hour,
	})
	require.NoError(t, err)
	commitAs(t, store, r.Generation(), "node-a")

	applier := &rolloutApplier{host: supervisorRolloutHost{s}, barrier: s.rollout, store: store, memberID: "node-a"}

	// First attempt: the swap fails and the old config is recovered.
	require.NoError(t, applier.step(context.Background()))
	require.Equal(t, 0, s.Config().Version, "the failed swap must recover the previous generation")

	// Second attempt: the transient cause is gone and the member converges.
	require.NoError(t, applier.step(context.Background()))
	assert.Equal(t, 99, s.Config().Version,
		"a retry must converge the member instead of stranding it behind the cohort")
}

// TestRolloutApplier_GivesUpAfterRepeatedAdoptFailures proves the retry is
// BOUNDED and that giving up is loud. An unbounded retry would rebuild the
// runtime every poll interval forever whenever the cause is deterministic —
// a self-inflicted outage rather than a recovery — so after the cap the member
// stops and reports the divergence instead.
func TestRolloutApplier_GivesUpAfterRepeatedAdoptFailures(t *testing.T) {
	store := memoryrollout.NewStore()
	rc := testRolloutConfig(store, "node-a")
	rc.PollInterval = time.Hour
	rc.LeaseTTL = time.Hour
	tf := &flakySenderFactory{failures: 1000} // never recovers
	s := newTestSupervisorTransport(tf, WithClusterRollout(rc))

	changes := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, soloCohortConfig(0), changes)
	t.Cleanup(func() { cancel(); <-errCh })

	cand := soloCohortConfig(99)
	cand.Bindings[0].Address = "addr/rolled"
	digest, ok := configCanonicalBytesDigest(cand)
	require.True(t, ok)
	s.rollout.stage(digest, cand, cand)
	r, err := store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: digest, ConfigVersion: 99,
		Members: []string{"node-a"}, TTL: time.Hour,
	})
	require.NoError(t, err)
	commitAs(t, store, r.Generation(), "node-a")

	applier := &rolloutApplier{host: supervisorRolloutHost{s}, barrier: s.rollout, store: store, memberID: "node-a", obs: &rolloutObserver{}}
	for range maxAdoptAttempts + 3 {
		require.NoError(t, applier.step(context.Background()))
	}

	assert.Equal(t, int32(1+maxAdoptAttempts), tf.builds.Load(),
		"the initial build plus exactly maxAdoptAttempts swap attempts, then no more")
	degraded, reason := s.Degraded()
	assert.True(t, degraded, "an unapplied committed generation must be surfaced, not silently dropped")
	assert.Contains(t, reason, "older config generation")

	status := applier.obs.status()
	assert.Equal(t, string(persistence.RolloutCommitted), status.State)
	assert.False(t, status.Applied,
		"committed AND not-applied is the signal that identifies a split member")
}

// TestJoinerRule_StagesTheBootConfigSoTheMemberCanVote proves a member that
// RESTARTED can still vote on a rollout carrying the config it booted.
//
// Staging otherwise happens only in the proposer path, which a booting member
// never runs — it receives its config as Run's `initial`, not as a change
// through apply. Without this the member holds no candidate, stays silent by
// design, and can never obtain one (its own watcher does not re-deliver
// unchanged content), so every rollout for that config would deadline-abort with
// that member permanently unable to participate.
func TestJoinerRule_StagesTheBootConfigSoTheMemberCanVote(t *testing.T) {
	store := memoryrollout.NewStore()
	// An in-flight rollout for a DIFFERENT candidate: the ordinary shape for a
	// member restarting mid-rollout.
	other := coordinatedClusteredCfg("r1")
	other.Bindings[0].Address = "addr/someone-elses-change"
	other.Version = 8
	seedRollout(t, store, other, persistence.RolloutProposed)

	boot := coordinatedClusteredCfg("r1")
	boot.Version = 7
	s := joinerSupervisor(store)
	require.NoError(t, s.checkCoordinatedRolloutPreflight(context.Background(), boot))

	digest, ok := configCanonicalBytesDigest(boot)
	require.True(t, ok)
	cand, staged := s.rollout.candidate(digest)
	require.True(t, staged, "a booting member must stage its own config so it can vote on it later")
	assert.True(t, configContentEqual(cand.frozen, boot))
}

// TestRolloutCoordinatorTick_TransientRenewalFailureKeepsTheLease proves a
// coordinator does NOT step down on a transient renewal error.
//
// Stepping down clears electedAt, so the next tick re-elects and must wait out a
// fresh FULL lock delay before it may decide anything. One throttled call would
// therefore cost the cohort a whole lease TTL of decision latency, and a flaky
// store would starve every rollout into its deadline — turning a store blip into
// aborted config changes.
func TestRolloutCoordinatorTick_TransientRenewalFailureKeepsTheLease(t *testing.T) {
	f := newCoordFixture(t, "node-a", "node-a")
	r := f.propose(t, "node-a")
	f.elected(t)
	require.NoError(t, f.store.Ack(context.Background(), r.Generation(), "node-a", "build"))

	flaky := &flakyRenewLease{electionLeaseStore: f.lease, err: shared.ErrThrottled}
	f.coord.lease = flaky

	require.Error(t, f.coord.tick(context.Background()), "the outage is reported so the loop can log it")
	assert.True(t, f.coord.tok.Valid(), "a transient renewal failure must NOT drop the fencing token")

	// The store recovers: the SAME coordinator decides immediately, with no
	// fresh lock delay.
	flaky.err = nil
	require.NoError(t, f.coord.tick(context.Background()))

	got, err := f.store.Current(context.Background())
	require.NoError(t, err)
	assert.Equal(t, persistence.RolloutCommitted, got.State(),
		"the coordinator resumes deciding as soon as the store recovers")
}

// flakyRenewLease fails Renew with a configurable error while delegating
// everything else, modelling a store outage that does not depose the owner.
type flakyRenewLease struct {
	*electionLeaseStore
	err error
}

func (f *flakyRenewLease) Renew(
	ctx context.Context, id string, tok persistence.LeaseToken, ttl time.Duration, eps map[string]string,
) (persistence.LeaseToken, error) {
	if f.err != nil {
		return persistence.LeaseToken{}, f.err
	}
	return f.electionLeaseStore.Renew(ctx, id, tok, ttl, eps)
}

var _ ports.LeaseStore = (*flakyRenewLease)(nil)
