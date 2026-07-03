package servicebus

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// --- Regression: Receiver.ApplyCredentials commit-before-build bricks -------
//
// These tests pin the build-first / commit-and-swap-on-success contract
// for Receiver.ApplyCredentials (mirror of Sender.ApplyCredentials) and
// the session-mode close-before-build rebuild-pending recovery path.

const (
	rotCS1 = "Endpoint=sb://ns1.servicebus.windows.net/;SharedAccessKeyName=k;SharedAccessKey=djE="
	rotCS2 = "Endpoint=sb://ns2.servicebus.windows.net/;SharedAccessKeyName=k;SharedAccessKey=djI="
)

// fakeStackBuilder is an injectable buildStackFn seam: it hands out a
// fresh closeableASBClient per successful build so tests can assert
// which stack is live / closed, and can be toggled to fail so a
// credential rotation's rebuild fails deterministically without a
// network or a malformed connection string.
type fakeStackBuilder struct {
	mu     sync.Mutex
	fail   bool
	builds int
	stacks []*closeableASBClient
}

func (b *fakeStackBuilder) setFail(fail bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fail = fail
}

func (b *fakeStackBuilder) buildCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builds
}

func (b *fakeStackBuilder) stackAt(i int) *closeableASBClient {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stacks[i]
}

func (b *fakeStackBuilder) build(context.Context, ConnectionConfig) (receiverStack, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.builds++
	if b.fail {
		return receiverStack{}, errors.New("servicebus-test: build boom")
	}
	c := &closeableASBClient{}
	b.stacks = append(b.stacks, c)
	return receiverStack{client: c}, nil
}

// connSnapshot reads the receiver's committed connection under the
// stack lock so assertions are race-free.
func connSnapshot(r *Receiver) ConnectionConfig {
	r.initMu.Lock()
	defer r.initMu.Unlock()
	return r.cfg.Connection
}

func rebuildPendingState(r *Receiver) bool {
	r.initMu.Lock()
	defer r.initMu.Unlock()
	return r.rebuildPending
}

// (a) Session-mode rotation whose rebuild fails must NOT advance
// cfg.Connection, and a subsequent ApplyCredentials with the SAME new
// credentials must NOT be short-circuited (changed=false) — it retries
// the build and succeeds once the fake stops failing.
func TestReceiver_ApplyCredentials_SessionRebuildFails_RePushRetries(t *testing.T) {
	t.Parallel()

	builder := &fakeStackBuilder{}
	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "q",
		SessionID:  "s1",
		Connection: ConnectionConfig{ConnectionString: shared.NewSecret(rotCS1)},
	}, nil)
	require.NoError(t, err)
	recv.buildStackFn = builder.build

	ctx := context.Background()
	require.NoError(t, recv.ensureClient(ctx)) // builds stack[0]
	require.Same(t, builder.stackAt(0), recv.currentClient())

	// Rotation whose rebuild fails. Session mode closes the old link
	// FIRST (exclusive lock), so the live client is nil afterwards, but
	// cfg.Connection must stay on the OLD value.
	builder.setFail(true)
	set := connectivity.NewCredentialSet(pwCred("", rotCS2), nil)

	err = recv.ApplyCredentials(ctx, set)
	require.Error(t, err)
	require.Nil(t, recv.currentClient(), "session rebuild closes the old link before the (failed) build")
	require.True(t, rebuildPendingState(recv), "a failed session rebuild leaves a pending rebuild")
	require.Equal(t, rotCS1, connSnapshot(recv).ConnectionString.Reveal(),
		"cfg.Connection must NOT advance on a failed rebuild")
	require.Equal(t, int32(1), builder.stackAt(0).closeCalls.Load(), "old stack closed exactly once")

	// Re-push the SAME new credentials once the build recovers. This must
	// NOT be swallowed by the changed=false short-circuit: the pending
	// rebuild drives a real build that now succeeds and commits.
	builder.setFail(false)
	require.NoError(t, recv.ApplyCredentials(ctx, set))

	require.Same(t, builder.stackAt(1), recv.currentClient(), "re-push rebuilds and swaps in a fresh stack")
	require.False(t, rebuildPendingState(recv), "successful rebuild clears the pending flag")
	require.Equal(t, rotCS2, connSnapshot(recv).ConnectionString.Reveal(),
		"cfg.Connection advances only after a successful build")
}

// (b) A successful rotation swaps the stack cleanly: the new stack is
// live, the old one is closed exactly once, cfg.Connection is committed,
// and no rebuild is left pending.
func TestReceiver_ApplyCredentials_CleanRotation_SwapsAndCommits(t *testing.T) {
	t.Parallel()

	builder := &fakeStackBuilder{}
	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "q",
		Connection: ConnectionConfig{ConnectionString: shared.NewSecret(rotCS1)},
	}, nil)
	require.NoError(t, err)
	recv.buildStackFn = builder.build

	ctx := context.Background()
	require.NoError(t, recv.ensureClient(ctx)) // builds stack[0]
	old := recv.currentClient()
	require.Same(t, builder.stackAt(0), old)

	set := connectivity.NewCredentialSet(pwCred("", rotCS2), nil)
	require.NoError(t, recv.ApplyCredentials(ctx, set))

	require.Same(t, builder.stackAt(1), recv.currentClient(), "rotation swaps in the rebuilt stack")
	require.NotSame(t, old, recv.currentClient())
	require.Equal(t, int32(1), builder.stackAt(0).closeCalls.Load(), "old stack closed after the swap")
	require.False(t, rebuildPendingState(recv))
	require.Equal(t, rotCS2, connSnapshot(recv).ConnectionString.Reveal(), "cfg.Connection committed on success")
	require.Equal(t, 2, builder.buildCount(), "one cold build + one rotation build")
}

// (c) Non-session rotation whose new build fails must keep the OLD
// working stack alive and polling (never nilled), and must NOT advance
// cfg.Connection so a retry with the same credentials rebuilds.
func TestReceiver_ApplyCredentials_NonSessionBuildFails_KeepsOldStackAlive(t *testing.T) {
	t.Parallel()

	builder := &fakeStackBuilder{}
	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "q",
		Connection: ConnectionConfig{ConnectionString: shared.NewSecret(rotCS1)},
	}, nil)
	require.NoError(t, err)
	recv.buildStackFn = builder.build

	ctx := context.Background()
	require.NoError(t, recv.ensureClient(ctx)) // builds stack[0]
	stack0 := builder.stackAt(0)
	require.Same(t, stack0, recv.currentClient())

	builder.setFail(true)
	set := connectivity.NewCredentialSet(pwCred("", rotCS2), nil)

	err = recv.ApplyCredentials(ctx, set)
	require.Error(t, err)
	require.Same(t, stack0, recv.currentClient(), "non-session build-first keeps the old stack live on failure")
	require.Equal(t, int32(0), stack0.closeCalls.Load(), "old stack is not closed when the rebuild fails")
	require.False(t, rebuildPendingState(recv), "non-session mode never enters the pending-rebuild state")
	require.Equal(t, rotCS1, connSnapshot(recv).ConnectionString.Reveal(),
		"cfg.Connection must NOT advance on a failed rebuild")

	// Retry with the same credentials rebuilds cleanly once healthy.
	builder.setFail(false)
	require.NoError(t, recv.ApplyCredentials(ctx, set))
	require.Same(t, builder.stackAt(1), recv.currentClient())
	require.Equal(t, int32(1), stack0.closeCalls.Load(), "old stack closed after the successful swap")
	require.Equal(t, rotCS2, connSnapshot(recv).ConnectionString.Reveal())
}

// sessionStackMock is a session receiver seam whose ReceiveMessages
// blocks until the poll context is cancelled OR the stack is closed
// (simulating an AMQP link detach), so a mid-poll credential rotation
// that closes the old link unblocks the poll loop instead of hanging.
type sessionStackMock struct {
	mockASBClient
	closed    chan struct{}
	closeOnce sync.Once
	renews    atomic.Int32
}

func newSessionStackMock() *sessionStackMock {
	m := &sessionStackMock{closed: make(chan struct{})}
	m.ReceiveMessagesFn = func(ctx context.Context, _ int, _ *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-m.closed:
			return nil, errors.New("amqp: link detached: the session lock was lost")
		}
	}
	m.RenewMessageLockFn = func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
		m.renews.Add(1)
		return nil
	}
	return m
}

func (m *sessionStackMock) Close(context.Context) error {
	m.closeOnce.Do(func() { close(m.closed) })
	return nil
}

var _ asbAPI = (*sessionStackMock)(nil)

// sessionRecoveryBuilder is a buildStackFn seam for the self-heal test:
// it fails the next build when armed, otherwise yields a fresh
// sessionStackMock.
type sessionRecoveryBuilder struct {
	mu     sync.Mutex
	fail   bool
	stacks []*sessionStackMock
}

func (b *sessionRecoveryBuilder) setFail(fail bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fail = fail
}

func (b *sessionRecoveryBuilder) latest() *sessionStackMock {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.stacks) == 0 {
		return nil
	}
	return b.stacks[len(b.stacks)-1]
}

func (b *sessionRecoveryBuilder) build(context.Context, ConnectionConfig) (receiverStack, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail {
		return receiverStack{}, errors.New("servicebus-test: session build boom")
	}
	m := newSessionStackMock()
	b.stacks = append(b.stacks, m)
	return receiverStack{client: m}, nil
}

// (design) Session-mode self-heal: when ApplyCredentials closes the old
// link and its rebuild fails, the poll loop (rebuildPendingStack) retries
// the pending rebuild with the pending connection and recovers WITHOUT an
// external re-push. The bridge CredentialRefresher is Warn-only and does
// not retry, so this poll-loop retry is the recovery mechanism.
func TestPollLoop_SessionRebuildPending_SelfHeals(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	builder := &sessionRecoveryBuilder{}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:    "q",
		SessionID:    "s1",
		LockDuration: 10 * time.Second,
		Connection:   ConnectionConfig{ConnectionString: shared.NewSecret(rotCS1)},
		Clock:        fake,
	}, nil)
	require.NoError(t, err)
	recv.buildStackFn = builder.build

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- recv.Run(ctx, func(context.Context, ports.Delivery) error { return nil })
	}()

	// Wait until the poll loop is live on the initial stack.
	waitUntil(t, 5*time.Second, func() bool { return recv.currentClient() != nil },
		"receiver never started")
	initial := builder.latest()
	require.NotNil(t, initial)

	// Rotate with a failing build: session mode closes the old link
	// first, so the live client goes nil and a rebuild is left pending.
	builder.setFail(true)
	set := connectivity.NewCredentialSet(pwCred("", rotCS2), nil)
	require.Error(t, recv.ApplyCredentials(context.Background(), set))
	require.True(t, rebuildPendingState(recv))

	// Let the build recover; the poll loop must self-heal by retrying the
	// pending rebuild. Drive the fake clock through any poll backoff /
	// renewer ticks until a fresh stack is live.
	builder.setFail(false)
	driveFakeClock(t, fake, 5*time.Second, func() bool {
		c := recv.currentClient()
		return c != nil && c != initial
	})

	require.False(t, rebuildPendingState(recv), "pending rebuild cleared after self-heal")
	require.NotNil(t, recv.currentClient())
	require.Equal(t, rotCS2, connSnapshot(recv).ConnectionString.Reveal(),
		"cfg.Connection commits once the pending rebuild succeeds")

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

// driveFakeClock advances fake time whenever a backoff timer is pending
// (spinning otherwise) until cond holds or the guard elapses. Mirrors the
// deterministic clock-driving pattern used by the session-accept tests.
func driveFakeClock(t *testing.T, fake *clocktest.Fake, guard time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(guard)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met before guard elapsed")
		}
		if fake.TimerCount() > 0 || fake.TickerCount() > 0 {
			fake.Advance(pollBackoffMax + 10*time.Second)
		} else {
			runtime.Gosched()
		}
	}
}

// --- Regression: stale in-flight rebuild clobbers newer stack (NEW-DEFECT) --

// A slow in-flight session rebuild of an OLDER connection must NOT
// overwrite a newer committed stack when it finally completes. Exact
// interleaving from the re-review: pending(conn1) → poll loop starts
// building conn1 (blocked) → rotation conn2 lands successfully → unblock
// conn1 build → its commit must be fenced (generation mismatch), the
// stale stack discarded, and the receiver stays on conn2.
func TestReceiver_SessionRebuild_StaleBuildDoesNotClobberNewer(t *testing.T) {
	t.Parallel()

	const (
		cs0 = "Endpoint=sb://ns0.servicebus.windows.net/;SharedAccessKeyName=k;SharedAccessKey=djA="
		cs1 = "Endpoint=sb://ns1.servicebus.windows.net/;SharedAccessKeyName=k;SharedAccessKey=djE="
		cs2 = "Endpoint=sb://ns2.servicebus.windows.net/;SharedAccessKeyName=k;SharedAccessKey=djI="
	)

	var (
		mu         sync.Mutex
		stacks     = map[string]*closeableASBClient{}
		cs1FailNP  = true        // first cs1 build (rotation A) fails, no block
		cs1Gate    chan struct{} // gate for the in-flight (poll-loop) cs1 build
		cs1Started chan struct{} // closed once the gated cs1 build begins
	)
	readStack := func(key string) *closeableASBClient {
		mu.Lock()
		defer mu.Unlock()
		return stacks[key]
	}

	build := func(_ context.Context, conn ConnectionConfig) (receiverStack, error) {
		key := conn.ConnectionString.Reveal()
		mu.Lock()
		if key == cs1 {
			if cs1FailNP {
				cs1FailNP = false
				mu.Unlock()
				return receiverStack{}, errors.New("servicebus-test: cs1 first build fails")
			}
			gate, started := cs1Gate, cs1Started
			mu.Unlock()
			if started != nil {
				close(started) // signal: captured gen, build began
			}
			if gate != nil {
				<-gate // block: the slow in-flight rebuild of conn1
			}
			c := &closeableASBClient{}
			mu.Lock()
			stacks[cs1] = c
			mu.Unlock()
			return receiverStack{client: c}, nil
		}
		c := &closeableASBClient{}
		stacks[key] = c
		mu.Unlock()
		return receiverStack{client: c}, nil
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "q",
		SessionID:  "s1",
		Connection: ConnectionConfig{ConnectionString: shared.NewSecret(cs0)},
	}, nil)
	require.NoError(t, err)
	recv.buildStackFn = build

	ctx := context.Background()
	require.NoError(t, recv.ensureClient(ctx)) // builds cs0
	require.Same(t, readStack(cs0), recv.currentClient())

	// Rotation A (→cs1): build fails, establishing pending(cs1) at gen 1.
	require.Error(t, recv.ApplyCredentials(ctx, connectivity.NewCredentialSet(pwCred("", cs1), nil)))
	require.True(t, rebuildPendingState(recv))
	require.Equal(t, cs0, connSnapshot(recv).ConnectionString.Reveal(), "failed rebuild must not advance cfg.Connection")

	// Arm the gate for the poll loop's in-flight rebuild of cs1.
	mu.Lock()
	cs1Gate = make(chan struct{})
	cs1Started = make(chan struct{})
	gate, started := cs1Gate, cs1Started
	mu.Unlock()

	// Poll loop's pending rebuild of cs1 (captures gen 1, then blocks).
	rebuildDone := make(chan error, 1)
	go func() { rebuildDone <- recv.rebuildPendingStack(ctx) }()
	<-started // the cs1 rebuild has captured gen 1 and is blocked on the gate

	// Rotation B (→cs2) lands successfully while the cs1 rebuild is stuck.
	require.NoError(t, recv.ApplyCredentials(ctx, connectivity.NewCredentialSet(pwCred("", cs2), nil)))
	require.False(t, rebuildPendingState(recv))
	require.Equal(t, cs2, connSnapshot(recv).ConnectionString.Reveal())
	require.Same(t, readStack(cs2), recv.currentClient(), "conn2 stack is live")

	// Unblock the stale conn1 build: its commit must be fenced (gen 1 !=
	// current gen 2), discarding the freshly built stack and leaving cs2.
	close(gate)
	require.NoError(t, <-rebuildDone)

	require.Same(t, readStack(cs2), recv.currentClient(), "stale conn1 build must NOT clobber the conn2 stack")
	require.Equal(t, cs2, connSnapshot(recv).ConnectionString.Reveal(), "cfg.Connection must stay on the newer conn2")
	require.False(t, rebuildPendingState(recv), "no rebuild pending after the newer rotation committed")
	require.Equal(t, int32(1), readStack(cs1).closeCalls.Load(), "the superseded conn1 stack is discarded (closed)")
	require.Equal(t, int32(0), readStack(cs2).closeCalls.Load(), "the live conn2 stack is not closed")
}

// --- Regression: stale client pinned in session renewer (FIX 2) -------------

// The session renewer must resolve the live receiver seam via
// currentClient() on EVERY tick: after a credential-rotation stack swap
// it must renew the NEW session's lock, not the old (closed) client's.
func TestRunSessionRenewer_RenewsViaCurrentClientAfterSwap(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()

	var renews1, renews2 atomic.Int32
	client1 := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			renews1.Add(1)
			return nil
		},
	}
	client2 := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			renews2.Add(1)
			return nil
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:    "q",
		SessionID:    "s1",
		LockDuration: 10 * time.Second,
		Client:       client1,
		Clock:        fake,
	}, nil)
	require.NoError(t, err)
	require.NoError(t, recv.ensureClient(context.Background())) // installs client1
	require.Same(t, client1, recv.currentClient())

	renewCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go recv.runSessionRenewer(renewCtx, 5*time.Second)

	// One renewer ticker registered.
	waitUntil(t, 5*time.Second, func() bool { return fake.TickerCount() >= 1 },
		"renewer ticker never registered")

	fake.Advance(6 * time.Second) // one tick → renew via client1
	waitUntil(t, 5*time.Second, func() bool { return renews1.Load() >= 1 },
		"renewer never renewed via the initial client")
	require.Zero(t, renews2.Load())

	// Rotate the stack: the renewer must pick up the NEW client.
	old := recv.swapStack(receiverStack{client: client2})
	require.Same(t, client1, old.client)

	fake.Advance(6 * time.Second) // next tick → renew via client2
	waitUntil(t, 5*time.Second, func() bool { return renews2.Load() >= 1 },
		"renewer must renew via the NEW client after a stack swap")

	cancel()
}
