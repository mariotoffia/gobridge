package runtime_test

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
	"github.com/mariotoffia/gobridge/domain/routing"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// ---------------------------------------------------------------------------
// Item (1): Stop double-Stop must block until teardown completes.
// ---------------------------------------------------------------------------

// blockingCloseDLQStore is a DLQ store whose io.Closer.Close blocks on a gate,
// so a test can hold Stop's teardown at the store-close step — which runs AFTER
// the mutex-guarded session/manager close, i.e. exactly the window where a
// second Stop could observe terminal state and (before the fix) return early
// while resources were still being released.
type blockingCloseDLQStore struct {
	*FakeDLQStore
	enteredOnce sync.Once
	entered     chan struct{}
	release     chan struct{}
	finished    atomic.Bool
}

func (s *blockingCloseDLQStore) Close() error {
	s.enteredOnce.Do(func() { close(s.entered) })
	<-s.release
	s.finished.Store(true)
	return nil
}

// TestRuntime_StopSecondCallBlocksUntilTeardownComplete is the regression for
// the double-Stop early-return defect: a concurrent second Stop must not return
// until the first Stop's teardown has fully released every resource. Otherwise a
// caller relying on "Stop returned ⇒ resources released" proceeds too early.
func TestRuntime_StopSecondCallBlocksUntilTeardownComplete(t *testing.T) {
	dlq := &blockingCloseDLQStore{
		FakeDLQStore: NewFakeDLQStore(),
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
	}
	rt := goruntime.New(
		goruntime.WithInstanceID("stop-double"),
		goruntime.WithDLQStore(dlq),
	)
	cfg, receiver, sender := helperMinimalRoute("route-blocking-store")
	require.NoError(t, rt.AddRoute(cfg, receiver, sender, nil, nil))
	require.NoError(t, rt.Start(context.Background()))

	// Goroutine A stops the runtime; its teardown blocks inside the store Close,
	// which runs after the mutex-guarded close section, so B can acquire rt.mu.
	errA := make(chan error, 1)
	go func() { errA <- rt.Stop(context.Background()) }()

	select {
	case <-dlq.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first Stop never reached the store Close")
	}

	// Goroutine B stops concurrently. With the fix it must block on stopDone
	// until A's teardown finishes; without it, B returns immediately while the
	// store handle is still being closed.
	errB := make(chan error, 1)
	go func() { errB <- rt.Stop(context.Background()) }()

	// B must NOT return while A's teardown (store Close) is still blocked.
	select {
	case <-errB:
		t.Fatal("second Stop returned before the first Stop's teardown completed")
	case <-time.After(200 * time.Millisecond):
	}
	assert.False(t, dlq.finished.Load(), "teardown (store close) must still be in flight")

	// Release A's teardown; both Stops must now complete and B must observe the
	// completed teardown.
	close(dlq.release)
	require.NoError(t, <-errA)
	require.NoError(t, <-errB)
	assert.True(t, dlq.finished.Load(),
		"second Stop returned before the first Stop's teardown finished")

	// A sequential Stop-after-Stop still returns nil promptly.
	require.NoError(t, rt.Stop(context.Background()))
}

// ---------------------------------------------------------------------------
// Item (2): manager/lease must not close while a final drain is mid-send.
// ---------------------------------------------------------------------------

// recordingLeaseStore records the wall-clock time of the first Release call so a
// test can prove the lease was released only AFTER an in-flight send completed.
type recordingLeaseStore struct {
	*FakeLeaseStore
	mu          sync.Mutex
	released    bool
	releaseTime time.Time
}

func (s *recordingLeaseStore) Release(ctx context.Context, leaseID string, token persistence.LeaseToken) error {
	s.mu.Lock()
	if !s.released {
		s.released = true
		s.releaseTime = time.Now()
	}
	s.mu.Unlock()
	return s.FakeLeaseStore.Release(ctx, leaseID, token)
}

func (s *recordingLeaseStore) releasedAt() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.releaseTime, s.released
}

// TestRuntime_StopWaitsForFinalDrainBeforeManagerClose is the regression for the
// manager-close gating defect: on a shutdown whose ctx has already expired, the
// session manager (which releases the lease and closes the session) must not be
// closed while a drainer send is still in flight. Were it closed early, the
// in-flight send's Complete would run against a released lease/stale fencing
// token and the record would resurface on restart as a duplicate.
//
// The deterministic discriminator is ordering: the lease Release must happen
// AFTER the in-flight send unblocks. With the fix, Stop waits (storeCloseGrace)
// for the drainer to confirm done before closing managers, so Release strictly
// follows the send. Without the fix, managers close immediately and Release
// precedes the send.
func TestRuntime_StopWaitsForFinalDrainBeforeManagerClose(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := &recordingLeaseStore{FakeLeaseStore: NewFakeLeaseStore()}
	dlq := NewFakeDLQStore()

	rt := goruntime.New(
		goruntime.WithInstanceID("bridge-drain-gate"),
		goruntime.WithOutboxStore(outbox),
		goruntime.WithLeaseStore(lease),
		goruntime.WithDLQStore(dlq),
	)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()

	// Gate the drainer's send so the test can hold it "mid-send" across Stop.
	sendEntered := make(chan struct{})
	releaseSend := make(chan struct{})
	var enterOnce sync.Once
	var sendUnblocked struct {
		mu sync.Mutex
		at time.Time
	}
	sender.SendFn = func(_ *messaging.Envelope) error {
		enterOnce.Do(func() { close(sendEntered) })
		<-releaseSend
		sendUnblocked.mu.Lock()
		sendUnblocked.at = time.Now()
		sendUnblocked.mu.Unlock()
		return nil
	}

	sessCfg := fastSessionConfig("mqtt-sess-drain-gate")

	cfg := goruntime.RouteConfig{
		ID: "drain-gate-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "binding-1", Address: "devices/1/state"},
			},
		},
		Bindings: []routing.DestinationBinding{
			{ID: "binding-1", SessionID: "mqtt-sess-drain-gate"},
		},
	}
	require.NoError(t, rt.AddRoute(cfg, receiver, sender, sess, &sessCfg))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, rt.Start(ctx))

	waitFor(t, 2*time.Second, "sess started", func() bool { return sess.IsStarted() })

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "msg-drain-gate-1",
		Subject: "device.state.update",
		Payload: []byte("hello"),
	})
	del := NewFakeDelivery(env)
	require.NoError(t, receiver.Emit(ctx, del))

	// The periodic drainer claims the pending record and enters the (blocked)
	// send. The record is now claimed, mid-send, lease still held.
	select {
	case <-sendEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("drainer never reached the send")
	}

	// Stop with an ALREADY-CANCELLED ctx: forces drainersDone=false on the first
	// wait, so the storeCloseGrace path (which must precede manager close) is
	// exercised.
	stopCtx, stopCancel := context.WithCancel(context.Background())
	stopCancel()
	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		_ = rt.Stop(stopCtx)
	}()

	// Give Stop a moment to enter the grace-wait, then release the send. With the
	// fix, the lease Release happens only after the drainer (and thus this send)
	// has confirmed done.
	select {
	case <-stopDone:
		t.Fatal("Stop returned before the in-flight send was released — manager closed early")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseSend)

	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not complete")
	}

	// The record must have completed successfully (no stale-token strand).
	assert.Equal(t, 1, outbox.CompletedCount(),
		"the in-flight record must complete before the lease is released")

	releaseAt, released := lease.releasedAt()
	require.True(t, released, "lease must be released on shutdown")
	sendUnblocked.mu.Lock()
	sentAt := sendUnblocked.at
	sendUnblocked.mu.Unlock()
	require.False(t, sentAt.IsZero(), "send must have unblocked")
	assert.True(t, releaseAt.After(sentAt) || releaseAt.Equal(sentAt),
		"lease Release (%s) must not precede the in-flight send completing (%s)",
		releaseAt, sentAt)
}
