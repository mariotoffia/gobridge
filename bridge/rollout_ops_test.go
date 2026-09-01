package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// Bounded-operation tests for the coordinated cluster rollout drive.
//
// The barrier's remote dependency is a shared store. Every one of these tests
// models the same failure: a store call that NEVER RETURNS — a TCP black hole,
// an SDK without a client-side timeout, a table that stopped answering. That is
// strictly worse than an error, and it is the failure a barrier must survive,
// because a rollout drive that parks in one call stops doing the three local
// things that keep this member safe: reverting a provisional config on its own
// deadman, telling the truth about how old its observation is, and unwinding on
// SIGTERM.

// TestClusterRolloutOps_NoCallClassStaysBlockedForever pins the primitive every
// other bound rests on: each rollout store or lease call runs under its own
// budget, and a call that ignores its context is ABANDONED when the budget runs
// out rather than owning the drive goroutine forever.
//
// The second assertion is the reason abandonment is safe: while an abandoned
// call is still outstanding the barrier refuses to start another one. Without
// that, a permanently black-holed store would leak one goroutine per poll tick
// for the life of the process — trading a wedged drive for an unbounded leak.
func TestClusterRolloutOps_NoCallClassStaysBlockedForever(t *testing.T) {
	for _, class := range []string{
		rolloutOpRead, rolloutOpVote, rolloutOpDecide,
		rolloutOpLease, rolloutOpArtifact, rolloutOpPropose,
	} {
		t.Run(class, func(t *testing.T) {
			ops := newRolloutOps(20 * time.Millisecond)
			blackHole := make(chan struct{})
			// Released at cleanup so the abandoned call returns and its
			// goroutine exits with the test rather than outliving it.
			t.Cleanup(func() { close(blackHole) })

			err := ops.run(context.Background(), class, func(context.Context) error {
				<-blackHole // ignores its context, exactly like a black-holed SDK call
				return nil
			})
			require.Error(t, err, "a call that never returns must not own the drive goroutine")
			assert.ErrorIs(t, err, context.DeadlineExceeded)

			err = ops.run(context.Background(), class, func(context.Context) error { return nil })
			assert.ErrorIs(t, err, shared.ErrUnavailable,
				"while a call is still outstanding the barrier must not pile another one on top of it")
		})
	}
}

// TestClusterRolloutOps_AdmitsAgainOnceTheAbandonedCallReturns proves the
// refusal above is a bounded consequence of the outage, not a latch: the moment
// the black-holed call finally comes back, the barrier resumes issuing calls. A
// latch here would turn one slow call into a permanently deaf member.
func TestClusterRolloutOps_AdmitsAgainOnceTheAbandonedCallReturns(t *testing.T) {
	ops := newRolloutOps(20 * time.Millisecond)
	blackHole := make(chan struct{})

	err := ops.run(context.Background(), rolloutOpRead, func(context.Context) error {
		<-blackHole
		return nil
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	close(blackHole)
	wait.Until(t, 5*time.Second, "the barrier issues calls again once the store answers", func() bool {
		return ops.run(context.Background(), rolloutOpRead, func(context.Context) error { return nil }) == nil
	})
}

// blackHoledDrive wires a driver over a store that can be black-holed mid-test,
// with a store-call budget short enough that the drive keeps ticking. It returns
// the driver, the host, the black hole (call hole.blackHole to swallow a call
// class), and the UNWRAPPED store, so a test can read the real rollout row
// without going through the hole it just opened.
func blackHoledDrive(
	t *testing.T, boot *ports.BridgeConfig,
) (*ClusterRolloutDriver, *fakeRolloutHost, *blackHoleRolloutStore, *memoryrollout.Store) {
	t.Helper()
	inner := memoryrollout.NewStore()
	hole := newBlackHoleRolloutStore(inner)
	t.Cleanup(hole.freeAll)

	host := newFakeRolloutHost(boot)
	rc := fastRolloutConfig(hole, "node-a")
	rc.StoreCallTimeout = 50 * time.Millisecond
	rc.Encode = newConfigCodecFake().encode
	d := NewClusterRolloutDriver(host, rc)
	require.NotNil(t, d)
	return d, host, hole, inner
}

// TestClusterRolloutDrive_ShutdownIsNotBlockedByAContextIgnoringStore is the
// shutdown half of the acceptance gate. The rollout drive is the FIRST thing a
// process shutdown waits on, so a barrier store that never answers used to hold
// SIGTERM ahead of the runtime drain and the HTTP shutdown until the platform
// SIGKILLed the process mid-drain.
//
// The stop function is deliberately given an unbounded context here: the point
// is that the drive goroutine unwinds ON ITS OWN, not that the caller can walk
// away from it.
func TestClusterRolloutDrive_ShutdownIsNotBlockedByAContextIgnoringStore(t *testing.T) {
	d, _, hole, _ := blackHoledDrive(t, soloCohortConfig(0))
	hole.blackHole(rolloutOpRead)

	stop := d.Start(context.Background(), clock.System, nil)
	require.NotNil(t, stop)
	wait.RequireReceive(t, hole.entered, 5*time.Second)

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop(context.Background())
	}()
	wait.RequireClosed(t, stopped, 10*time.Second)
}

// TestClusterRolloutDrive_StatusGoesStaleWhileTheStoreIsBlackHoled is the
// freshness half of the acceptance gate. A rollout status is a snapshot of a
// SHARED row, so a member that cannot re-read it keeps serving its last
// observation. Reporting that observation without saying how old it is invites
// exactly the wrong operator conclusion — "the cohort is still staging" — when
// the truth is "this member has not seen the row for a minute".
func TestClusterRolloutDrive_StatusGoesStaleWhileTheStoreIsBlackHoled(t *testing.T) {
	boot := soloCohortConfig(0)
	d, _, hole, _ := blackHoledDrive(t, boot)

	stop := d.Start(context.Background(), clock.System, nil)
	defer stop(context.Background())

	// Drive a real rollout first, so the snapshot the member goes stale HOLDING is
	// a live one — that is the dangerous shape: a status that still reads
	// "committed, everyone acked" long after this member stopped being able to
	// check.
	candidate := soloCohortConfig(7)
	candidate.Bindings[0].Address = "addr/rolled"
	require.NoError(t, d.Propose(context.Background(), boot, candidate, candidate))
	wait.Until(t, 5*time.Second, "the drive publishes a fresh observation of the rollout", func() bool {
		st, ok := d.Status()
		return ok && st.State != "" && !st.Stale
	})

	hole.blackHole(rolloutOpRead)
	wait.Until(t, 10*time.Second, "the status goes stale while the store cannot be read", func() bool {
		st, ok := d.Status()
		return ok && st.Stale && st.LastError != ""
	})
	st, _ := d.Status()
	assert.NotEmpty(t, st.State, "the last observation is still reported — flagged stale, not discarded")
}

// TestClusterRolloutDeadman_RevertsWhileTheStoreIsBlackHoled is the deadman half
// of the acceptance gate, and the one that costs traffic when it fails.
//
// A member that provisionally swapped to a generation it never converged on is
// running a config the cohort has NOT confirmed. The confirm window exists so
// that member returns to the last confirmed generation on its own when no
// coordinator decides — and "no coordinator decides" and "the store stopped
// answering" are the SAME outage. So the local deadman must run off cached local
// state on the drive's own cadence, never as a step that a store read has to
// reach first.
func TestClusterRolloutDeadman_RevertsWhileTheStoreIsBlackHoled(t *testing.T) {
	// The window is generous relative to the ~50 ms this cohort needs to elect,
	// commit provisionally and swap, so the store goes dark well inside it on any
	// machine. A tight window would race the coordinator's own Revert and prove
	// the wrong thing.
	const window = time.Second
	boot := soloWindowConfig(0, window)
	boot.Bindings[0].Address = "addr/original"

	d, host, hole, inner := blackHoledDrive(t, boot)
	// The candidate never reaches convergence — the confirm window's raison
	// d'être: a config that builds and swaps but cannot reach broker readiness.
	host.unconverged[7] = true

	stop := d.Start(context.Background(), clock.System, nil)
	defer stop(context.Background())

	candidate := soloWindowConfig(7, window)
	candidate.Bindings[0].Address = "addr/never-converges"
	require.NoError(t, d.Propose(context.Background(), boot, candidate, candidate))

	wait.Until(t, 5*time.Second, "the member provisionally swaps to the candidate", func() bool {
		return host.Config().Version == 7
	})

	// The store goes dark BEFORE the window expires, so no coordinator decision
	// can land and no observation can be taken.
	hole.blackHole(rolloutOpRead, rolloutOpVote, rolloutOpDecide, rolloutOpArtifact)

	wait.Until(t, 15*time.Second, "the local deadman reverts to the last confirmed generation", func() bool {
		return host.Config().Version == 0
	})
	assert.Equal(t, "addr/original", host.Config().Bindings[0].Address,
		"the last confirmed generation serves again")

	r, err := inner.Current(context.Background())
	require.NoError(t, err)
	assert.Equal(t, persistence.RolloutCommitted, r.State(),
		"no coordinator could decide through the black hole — the revert was this member's own deadman")
}

// TestClusterRolloutDrive_MetersEveryStoreCallClassAndItsFreshness pins the
// operator-facing half of the bound. Bounding the calls without publishing what
// happened to them would trade a wedged drive for a silent one: the member keeps
// serving, the barrier stops moving, and nothing says which call class stopped
// answering or how old the status it is still publishing has become.
func TestClusterRolloutDrive_MetersEveryStoreCallClassAndItsFreshness(t *testing.T) {
	rec := &ports.RecordingExporter{}
	boot := soloCohortConfig(0)
	d, _, hole, _ := blackHoledDrive(t, boot)

	stop := d.Start(context.Background(), clock.System, rec)
	defer stop(context.Background())

	candidate := soloCohortConfig(7)
	candidate.Bindings[0].Address = "addr/rolled"
	require.NoError(t, d.Propose(context.Background(), boot, candidate, candidate))
	wait.Until(t, 5*time.Second, "the rollout resolves so every call class has run", func() bool {
		st, ok := d.Status()
		return ok && st.State == string(persistence.RolloutCommitted)
	})

	for _, class := range []string{rolloutOpPropose, rolloutOpRead, rolloutOpVote, rolloutOpDecide, rolloutOpLease} {
		assert.Positive(t, countEntries(rec, shared.MetricClusterRolloutStoreCalls, shared.TagKeyOperation, class),
			"call class %q must be metered", class)
	}
	assert.NotEmpty(t, rec.FindEntries(shared.MetricClusterRolloutObservationAge),
		"the freshness of the published observation must be reported, not just its content")

	// The failure side is what an alarm fires on, so it has to be dimensioned too.
	hole.blackHole(rolloutOpRead)
	wait.Until(t, 10*time.Second, "the abandoned read is metered as a timeout", func() bool {
		return countEntries(rec, shared.MetricClusterRolloutStoreCalls,
			shared.TagKeyOutcome, rolloutCallTimeout) > 0
	})
}
