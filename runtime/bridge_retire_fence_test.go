package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// retireOutboxStore counts claims and holds every Persist until persisted is
// closed or its context ends, so a shared_outbox delivery stays in flight on
// demand.
type retireOutboxStore struct {
	graftOutboxStore
	claims    atomic.Int32
	persisted <-chan struct{}
}

func (s *retireOutboxStore) Persist(ctx context.Context, _ []*persistence.OutboxRecord) error {
	select {
	case <-s.persisted:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *retireOutboxStore) Claim(context.Context, string, persistence.LeaseToken, int) ([]*persistence.OutboxRecord, error) {
	s.claims.Add(1)
	return nil, nil
}

// claimOnceStrategy polls promptly until the drainer has claimed once, then
// waits an hour, so any later claim is the final batch of a stopped drainer.
type claimOnceStrategy struct{ store *retireOutboxStore }

func (s claimOnceStrategy) NextInterval(int) time.Duration {
	if s.store.claims.Load() == 0 {
		return time.Millisecond
	}
	return time.Hour
}

// retiringUnit is a runtime retiring an exclusive shared_outbox unit (route
// r1, session s1) whose drainer has claimed once. One delivery is held in
// Persist, so the Retire is draining.
type retiringUnit struct {
	rt      *Runtime
	outbox  *retireOutboxStore
	mgr     *session.Manager
	retired <-chan error
	release func()
}

func startRetiringUnit(t *testing.T) retiringUnit {
	t.Helper()
	persisted := make(chan struct{})
	u := retiringUnit{outbox: &retireOutboxStore{persisted: persisted}}
	u.release = sync.OnceFunc(func() { close(persisted) })
	t.Cleanup(u.release)
	u.rt = New(WithInstanceID("retire-in-drain"), WithLeaseStore(&graftLeaseStore{}),
		WithOutboxStore(u.outbox), WithDLQStore(&graftDLQStore{}))
	cfg := session.DefaultConfig("s1", true)
	cfg.DrainStrategy = claimOnceStrategy{store: u.outbox}
	recv := newComponentReceiver()
	require.NoError(t, u.rt.AddRoute(outboxRoute("r1"), recv, &componentSender{}, newRetireSession(), &cfg))
	startComponentRuntime(t, u.rt)
	wait.Until(t, 2*time.Second, "the drainer claims once", func() bool { return u.outbox.claims.Load() == 1 })
	u.rt.mu.Lock()
	u.mgr = u.rt.sessionMgrs["s1"]
	u.rt.mu.Unlock()
	wait.RequireClosed(t, recv.ready, 2*time.Second)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "held", Subject: "retire"})
	require.NoError(t, recv.emit(recv.ctx, &syntheticDelivery{env: env}))

	retired := make(chan error, 1)
	u.retired = retired
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		retired <- u.rt.Retire(ctx, Unit{Routes: []string{"r1"}, Sessions: []string{"s1"}})
	}()
	wait.Until(t, 2*time.Second, "Retire takes the unit out", func() bool { return len(u.rt.Routes()) == 0 })
	return u
}

// TestRetire_FenceDuringTheDrainFencesTheRetiringDrainer pins that a Fence
// that lands while a Retire drains still reaches the drainer the Retire took
// out: Fence forbids every fresh claim, the final batch of a stopping drainer
// included.
func TestRetire_FenceDuringTheDrainFencesTheRetiringDrainer(t *testing.T) {
	u := startRetiringUnit(t)

	u.rt.Fence()

	require.NoError(t, wait.RequireReceive(t, u.retired, 5*time.Second))
	assert.Equal(t, int32(1), u.outbox.claims.Load(), "a fenced drainer claims nothing in its final batch")
}

// TestRetire_DLQWritesDuringTheDrainAreFencedOnTheHeldLease pins that while a
// Retire drains, a delivery that must be dead-lettered for the retiring
// exclusive session is fenced on that session's still-held lease, as it is
// during Stop's drain, rather than refused.
func TestRetire_DLQWritesDuringTheDrainAreFencedOnTheHeldLease(t *testing.T) {
	u := startRetiringUnit(t)

	want, held := u.mgr.Token()
	require.True(t, held, "precondition: the retiring session still holds its lease")
	got, allowed := u.rt.dlqToken("s1")
	assert.True(t, allowed, "the write is fenced on the held lease, not refused")
	assert.Equal(t, want, got)

	u.release()
	require.NoError(t, wait.RequireReceive(t, u.retired, 5*time.Second))
}

// TestRetire_KeepsRefusingDLQWritesWhenAComponentDidNotStop pins that when a
// retired component does not stop, its exclusive session keeps refusing DLQ
// writes: the lease is released once the manager closes, so a write from the
// straggler would otherwise go through unfenced.
func TestRetire_KeepsRefusingDLQWritesWhenAComponentDidNotStop(t *testing.T) {
	rt := New(WithInstanceID("retire-stuck-exclusive"), WithLeaseStore(&graftLeaseStore{}),
		WithOutboxStore(&graftOutboxStore{}), WithDLQStore(&graftDLQStore{}))
	release := make(chan struct{})
	cfg := session.DefaultConfig("s1", true)
	require.NoError(t, rt.AddRoute(outboxRoute("r1"), stuckReceiver{release: release}, &componentSender{}, newRetireSession(), &cfg))
	startComponentRuntime(t, rt)
	t.Cleanup(func() { close(release) })
	wait.Until(t, 2*time.Second, "s1 acquires its lease", func() bool { return rt.LeaseStatus()["s1"] })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorContains(t, rt.Retire(ctx, Unit{Routes: []string{"r1"}, Sessions: []string{"s1"}}), "did not finish")

	_, allowed := rt.dlqToken("s1")
	assert.False(t, allowed)
}

// TestRetire_GivesAStuckComponentTheGraceBeforeClosingItsSession pins that when
// ctx ends before the unit's components stop, Retire still waits the store-close
// grace for them before it closes their sessions — a drainer's final Complete
// must run against a live lease — and closes the sessions once they stop.
func TestRetire_GivesAStuckComponentTheGraceBeforeClosingItsSession(t *testing.T) {
	rt := New(WithInstanceID("retire-grace"))
	s1, release := newRetireSession(), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1"}, s1, nopRouteSender{}))
	require.NoError(t, rt.AddRoute(ridingRoute("r1", "s1"), stuckReceiver{release: release}, &componentSender{}, s1, nil))
	startComponentRuntime(t, rt)

	// Cancelled and deadline-less: the grace is the full policy-derived one.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	retired := make(chan error, 1)
	go func() { retired <- rt.Retire(ctx, Unit{Routes: []string{"r1"}, Sessions: []string{"s1"}}) }()
	wait.Until(t, 2*time.Second, "Retire takes the unit out", func() bool { return len(rt.Routes()) == 0 })
	wait.Silent(t, s1.closed, 25*time.Millisecond)
	releaseOnce()

	err := wait.RequireReceive(t, retired, 5*time.Second)
	require.ErrorIs(t, err, context.Canceled, "ctx ended before the components stopped")
	assert.Equal(t, int32(1), s1.closes.Load(), "the session closes once the component stops")
}
