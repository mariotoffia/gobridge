package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// closeContextSession records the context its Close was invoked with, so a test
// can assert what the manager actually asked the transport to honour.
type closeContextSession struct {
	mu       sync.Mutex
	called   bool
	deadline time.Time
	hasDL    bool
	ctxErr   error
	events   chan ports.SessionEvent
}

func (s *closeContextSession) Start(context.Context) error { return nil }
func (s *closeContextSession) Reconcile(context.Context, connectivity.SessionPlan) error {
	return nil
}
func (s *closeContextSession) Health(context.Context) ports.SessionHealth {
	return ports.SessionHealth{}
}
func (s *closeContextSession) Events() <-chan ports.SessionEvent { return s.events }
func (s *closeContextSession) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called = true
	s.deadline, s.hasDL = ctx.Deadline()
	s.ctxErr = ctx.Err()
	return nil
}

var _ ports.Session = (*closeContextSession)(nil)

// TestCloseSourceBounded_PassesReleaseDeadlineIntoClose pins that the
// session-failure teardown gives the transport a DEADLINE, not only an external
// ceiling raced against it.
//
// The manager races Close against a hard ceiling so a ctx-ignoring adapter
// cannot hang it, and treats "the ceiling fired while Close was still parked"
// as a wedge that must NOT hand the lease to a standby. Without a deadline on
// the context, a cooperative adapter has nothing to abort on: a slow but
// well-behaved disconnect runs past the ceiling, gets classified as a wedge,
// terminalizes the process, and extends the outage to the lease TTL.
//
// Counterfactual (the pre-fix detached-but-unbounded context): Close received
// context.WithoutCancel(ctx) — no deadline at all — so ctx.Deadline() reports
// false here. The sibling lease-release path already passed the same bound.
func TestCloseSourceBounded_PassesReleaseDeadlineIntoClose(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	sess := &closeContextSession{events: make(chan ports.SessionEvent)}
	mgr := NewWithMetrics(Config{
		SessionID: "sess-close-deadline",
		Exclusive: true,
		LeaseTTL:  5 * time.Second,
	}, sess, newLeaseLossStore(100, nil), "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	// The caller's context is already cancelled — a shutdown in progress. The
	// close must still run (detached) AND still be bounded.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.True(t, mgr.closeSourceBounded(ctx, "session failure"),
		"a cooperative Close completes within the ceiling")

	sess.mu.Lock()
	defer sess.mu.Unlock()
	require.True(t, sess.called)
	require.NoError(t, sess.ctxErr,
		"the close context is detached from the caller's cancellation")
	require.True(t, sess.hasDL,
		"a cooperative adapter needs a deadline to abort on, or its slowness is misread as a wedge")
	remaining := time.Until(sess.deadline)
	require.Positive(t, remaining)
	require.LessOrEqual(t, remaining, mgr.releaseTimeout()-closeAbortMargin,
		"the cooperative deadline must fire strictly before the manager's ceiling: both are armed at "+
			"almost the same instant, so without a margin a well-behaved adapter that aborts at its "+
			"deadline still loses the race, is judged a wedge, and the lease is retained until its TTL")
}
