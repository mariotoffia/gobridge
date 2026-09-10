package bridge

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// Bounded remote operations for the coordinated cluster rollout barrier.
//
// Every barrier decision goes through a shared store, and the drive is ONE
// goroutine: a single call that does not come back stops commit, abort, confirm,
// revert, observation freshness, the local confirm-window deadman and the
// process shutdown that waits on the drive. A deadline alone does not fix that.
// context.WithTimeout expires the CONTEXT, it does not unblock the CALL, so a
// client that ignores its context — an SDK without a client-side timeout, a
// connection into a TCP black hole — still owns the drive goroutine forever.
//
// So a call that blows its budget is ABANDONED: it keeps running on its own
// goroutine (it will unwind if it ever returns) while the drive moves on. That
// trade is only safe if the abandoned goroutines cannot accumulate, which is
// what the single-outstanding rule below is for: while an abandoned call has not
// returned, the barrier starts no new one and fails fast instead. A black-holed
// store therefore costs exactly one parked goroutine, and every subsequent tick
// is instant — which is what keeps the deadman on time rather than merely
// eventually.

// Rollout operation classes. They name what the call is FOR, not which method it
// is, because that is the distinction an operator reading the store-call metric
// needs: a failing "decide" is a rollout that cannot resolve, a failing
// "artifact" is a member that may boot stale, a failing "lease" is a cohort
// without a coordinator.
const (
	// rolloutOpRead is an observation: the rollout row or the committed artifact.
	rolloutOpRead = "read"
	// rolloutOpVote is this member's Ack, Nack or Converge.
	rolloutOpVote = "vote"
	// rolloutOpDecide is a coordinator decision: Commit, Abort, Confirm, Revert.
	rolloutOpDecide = "decide"
	// rolloutOpLease is a coordinator-election call: Acquire, Renew, Release.
	rolloutOpLease = "lease"
	// rolloutOpArtifact is a durable last-committed-config artifact write.
	rolloutOpArtifact = "artifact"
	// rolloutOpPropose is opening (or joining) a rollout from the reload path.
	rolloutOpPropose = "propose"
)

// Outcome tag values for MetricClusterRolloutStoreCalls.
const (
	rolloutCallSuccess = "success"
	rolloutCallFailure = "failure"
	rolloutCallTimeout = "timeout"
	rolloutCallBlocked = "blocked"
)

// defaultRolloutStoreCallTimeout bounds one rollout store or lease call. It is
// deliberately well under the coordinator lease TTL (30 s by default): a tick
// makes a handful of calls, and their sum must stay inside the TTL or a
// coordinator could lose its lease while waiting on its own renewal.
const defaultRolloutStoreCallTimeout = 5 * time.Second

// rolloutOps bounds and meters every remote call the barrier makes. One instance
// is shared by the barrier's proposer, the applier and the coordinator, so the
// single-outstanding rule covers the whole barrier rather than one half of it —
// a store that has stopped answering has stopped answering for all of them.
type rolloutOps struct {
	timeout time.Duration

	mu sync.Mutex
	// obs is set when the drive starts; nil before that (the proposer path can
	// run first) and in hand-wired tests.
	obs *rolloutObserver
	// outstanding is the result channel of a call that blew its budget and was
	// abandoned. Non-nil until that call returns (or never, if it truly never
	// does), and while it is non-nil no new call is started.
	outstanding chan error
}

func newRolloutOps(timeout time.Duration) *rolloutOps {
	return &rolloutOps{timeout: orDefault(timeout, defaultRolloutStoreCallTimeout)}
}

// setObserver attaches the drive's observer so call outcomes reach metrics and
// deep health. Called once, when the drive starts.
func (o *rolloutOps) setObserver(obs *rolloutObserver) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.obs = obs
}

// run executes one remote call of the given class under its own budget. A nil
// receiver runs fn unbounded, which keeps a hand-wired applier (tests, and the
// benchmarks) working without a barrier behind it.
//
// MUST: fn may only close over IMMUTABLE values and locals. An abandoned call
// keeps running after run has returned, so a closure that reads mutable caller
// state (a coordinator's fencing token, an applier's pending repair) races the
// caller's next write to it. Read what the call needs into a local first and
// close over that.
func (o *rolloutOps) run(ctx context.Context, class string, fn func(context.Context) error) error {
	if o == nil {
		return fn(ctx)
	}
	// A caller whose context is already finished gets no call at all: spawning one
	// only to abandon it the same instant would report a budget timeout that never
	// happened and burn a goroutine on the shutdown path.
	if err := ctx.Err(); err != nil {
		o.record(class, rolloutCallFailure, err)
		return err
	}
	if err := o.admit(); err != nil {
		o.record(class, rolloutCallBlocked, err)
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- fn(callCtx) }()

	select {
	case err := <-done:
		if err != nil {
			o.record(class, rolloutCallFailure, err)
			return err
		}
		o.record(class, rolloutCallSuccess, nil)
		return nil
	case <-callCtx.Done():
		// Two different things end callCtx, and saying so matters: an operator
		// reading "did not return within its 5s budget" about a call that was
		// actually cut short by SIGTERM would go looking for a store problem that
		// does not exist.
		if ctxErr := ctx.Err(); ctxErr != nil {
			err := fmt.Errorf("bridge: the coordinated cluster rollout %s call was abandoned because the "+
				"caller's context ended: %w", class, ctxErr)
			// NOT counted as outstanding: a call cut short this way is expected back
			// promptly, and blocking on it would make the drive skip the orderly
			// coordinator resignation that immediately follows a shutdown.
			o.record(class, rolloutCallFailure, err)
			return err
		}
		err := fmt.Errorf("bridge: the coordinated cluster rollout %s call did not return within its %s "+
			"budget and was abandoned; the drive keeps running so the local confirm-window deadman, the "+
			"observation freshness signal and process shutdown are not held behind it: %w",
			class, o.timeout, context.DeadlineExceeded)
		o.markOutstanding(done)
		o.record(class, rolloutCallTimeout, err)
		return err
	}
}

// rolloutOpValue is run for a call that returns a value. The value is read only
// on the success path, so an abandoned call writing it later races nothing.
func rolloutOpValue[T any](
	ctx context.Context, o *rolloutOps, class string, fn func(context.Context) (T, error),
) (T, error) {
	var out T
	err := o.run(ctx, class, func(callCtx context.Context) error {
		v, callErr := fn(callCtx)
		if callErr != nil {
			return callErr
		}
		out = v
		return nil
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return out, nil
}

// admit reports whether a new call may start. It clears the outstanding marker
// the moment the abandoned call finally returns, so the refusal lasts exactly as
// long as the outage does and never latches.
func (o *rolloutOps) admit() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.outstanding == nil {
		return nil
	}
	select {
	case <-o.outstanding:
		o.outstanding = nil
		return nil
	default:
		return fmt.Errorf("bridge: a previous coordinated cluster rollout store call has still not "+
			"returned, so this one is refused rather than parking another goroutine on an unresponsive "+
			"store; the barrier retries on the next poll: %w", shared.ErrUnavailable)
	}
}

func (o *rolloutOps) markOutstanding(done chan error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.outstanding = done
}

// record meters one call outcome. err is nil on success.
func (o *rolloutOps) record(class, outcome string, err error) {
	o.mu.Lock()
	obs := o.obs
	o.mu.Unlock()
	obs.observeCall(class, outcome, err)
}
