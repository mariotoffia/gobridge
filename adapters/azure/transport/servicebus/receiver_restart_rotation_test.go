package servicebus

import (
	"context"
	"errors"
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

// exclusiveSessionBuilder is a buildStackFn seam that models the broker's
// exclusive lock on a pinned session: a built stack holds the lock until
// its client is closed, and a build while another stack holds it fails
// the way AcceptSessionForQueue does (session-cannot-be-locked). It
// records the connection of every build attempt, labelled "A" (rotCS1)
// or "B" (rotCS2), so a test can assert which connection a Run used.
type exclusiveSessionBuilder struct {
	mu       sync.Mutex
	fail     bool
	holder   *closeableASBClient
	builds   []string
	receives atomic.Int32
}

func (b *exclusiveSessionBuilder) setFail(fail bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fail = fail
}

// buildsSince returns the labels of the build attempts from index from on.
func (b *exclusiveSessionBuilder) buildsSince(from int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.builds[from:]...)
}

func (b *exclusiveSessionBuilder) lockHolder() *closeableASBClient {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.holder
}

func (b *exclusiveSessionBuilder) build(_ context.Context, conn ConnectionConfig) (receiverStack, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch conn.ConnectionString.Reveal() {
	case rotCS1:
		b.builds = append(b.builds, "A")
	case rotCS2:
		b.builds = append(b.builds, "B")
	default:
		b.builds = append(b.builds, "?")
	}
	if b.fail {
		return receiverStack{}, errors.New("servicebus-test: network down")
	}
	if b.holder != nil {
		return receiverStack{}, errors.New("amqp: com.microsoft:session-cannot-be-locked")
	}
	c := &closeableASBClient{}
	// The poll blocks until its Run ends, so a Run never loops back to
	// rebuildPendingStack on its own while the test drives it.
	c.ReceiveMessagesFn = func(ctx context.Context, _ int, _ *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
		b.receives.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	c.closeFn = func(context.Context) error {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.holder == c {
			b.holder = nil
		}
		return nil
	}
	b.holder = c
	return receiverStack{client: c}, nil
}

// verifies that a route restart while a pinned-session credential rotation
// is still pending completes the rotation on the pending connection. The
// rotation to B closed the A stack first (exclusive session lock) and its
// build failed, so the receiver is left with an empty stack, rebuildPending
// set, and cfg.Connection still on A. The route runner then ends the Run,
// closes the receiver and calls Run again on the same receiver. That Run
// must build from B and commit it. Building from A instead re-accepts the
// session on the old credentials, the A stack then holds the session lock,
// and every retry of the pending B rebuild fails with session-cannot-be-
// locked: the rotation never commits.
//
// The fake clock never advances, so the poll loop cannot retry after a
// failed build; the first build after the Close decides the outcome and a
// regression fails at once instead of spinning through backoff.
//
// Mutation check: drop the rebuildPending early return in ensureClient and
// the restarted Run builds from A first, so the "first build after Close"
// assertion fails with "A".
func TestReceiver_RestartDuringPendingSessionRebuildUsesPendingConnection(t *testing.T) {
	t.Parallel()

	builder := &exclusiveSessionBuilder{}
	recv, err := NewReceiver(ReceiverConfig{
		QueueName:    "q",
		SessionID:    "s1",
		LockDuration: 10 * time.Second,
		Connection:   ConnectionConfig{ConnectionString: shared.NewSecret(rotCS1)},
		Clock:        clocktest.New(),
	}, nil)
	require.NoError(t, err)
	recv.buildStackFn = builder.build
	emit := func(context.Context, ports.Delivery) error { return nil }

	// Run 1 builds on A and blocks in its poll.
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	done1 := make(chan error, 1)
	go func() { done1 <- recv.Run(ctx1, emit) }()
	waitUntil(t, 5*time.Second, func() bool { return builder.receives.Load() >= 1 },
		"first Run never polled")
	require.Equal(t, []string{"A"}, builder.buildsSince(0))

	// The rotation to B closes the A stack, then its build fails.
	builder.setFail(true)
	require.Error(t, recv.ApplyCredentials(context.Background(),
		connectivity.NewCredentialSet(pwCred("", rotCS2), nil)))
	require.True(t, rebuildPendingState(recv), "a failed session rebuild leaves a pending rebuild")
	require.Nil(t, builder.lockHolder(), "the rotation released the session lock held on A")
	require.Equal(t, rotCS1, connSnapshot(recv).ConnectionString.Reveal())

	// Route restart: the Run ends, the runner closes the receiver, and
	// the broker is reachable again.
	cancel1()
	require.ErrorIs(t, <-done1, context.Canceled)
	require.NoError(t, recv.Close(context.Background()))
	require.True(t, rebuildPendingState(recv), "Close keeps the pending rebuild")
	builder.setFail(false)
	mark := len(builder.buildsSince(0))

	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- recv.Run(ctx2, emit) }()
	t.Cleanup(func() {
		cancel2()
		<-done2
	})

	waitUntil(t, 5*time.Second, func() bool { return len(builder.buildsSince(mark)) > 0 },
		"restarted Run never built a stack")
	require.Equal(t, "B", builder.buildsSince(mark)[0],
		"first build after Close must use the pending connection, not re-accept the session on A")

	waitUntil(t, 5*time.Second, func() bool { return !rebuildPendingState(recv) },
		"restarted Run never committed the pending rebuild")
	require.Equal(t, rotCS2, connSnapshot(recv).ConnectionString.Reveal(),
		"cfg.Connection commits to the pending connection")
	require.Equal(t, []string{"B"}, builder.buildsSince(mark),
		"exactly one build after Close, on the pending connection")
	require.Same(t, builder.lockHolder(), recv.currentClient(),
		"the live stack is the one holding the session lock")
}
