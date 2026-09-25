package servicebus

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// ---------------------------------------------------------------------------
// Tests for Receiver.Close across a route restart (Close, then Run again)
// ---------------------------------------------------------------------------

// verifies that a route restart — Run, Close, Run again on the SAME
// receiver, Close — releases the stack each Run built. ports.Receiver:
// Close ends one Run, not the receiver, so the second Close must close the
// connection, link and scheduler the restarted Run rebuilt; otherwise
// every restart leaks one AMQP connection and never releases the messages
// its link holds. A Close with no Run since the previous Close finds an
// empty stack and closes nothing twice.
//
// Mutation check: wrap Close's body in a sync.Once again and the second
// Close becomes a no-op, so the rebuilt stack is never closed and the
// "rebuilt stack closed exactly once" assertion fails with 0.
func TestReceiver_CloseAfterRestartReleasesRebuiltStack(t *testing.T) {
	t.Parallel()

	builder := &fakeStackBuilder{}
	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "q",
		AutoExtend: boolPtr(false),
		Connection: ConnectionConfig{Namespace: "ns.servicebus.windows.net"},
	}, nil)
	require.NoError(t, err)
	recv.buildStackFn = builder.build

	// run builds the stack (ensureClient), then the poll loop sees the
	// cancelled context and returns, as it does when a route stops.
	run := func() {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := recv.Run(ctx, func(context.Context, ports.Delivery) error { return nil })
		require.ErrorIs(t, err, context.Canceled)
	}

	run()
	require.Equal(t, 1, builder.buildCount(), "first Run builds a stack")
	first := builder.stackAt(0)
	require.Same(t, first, recv.currentClient())

	require.NoError(t, recv.Close(context.Background()))
	require.Equal(t, int32(1), first.closeCalls.Load(), "first stack closed by the first Close")
	require.Nil(t, recv.currentClient(), "Close leaves an empty stack")

	// A second Close with no Run in between finds the empty stack.
	require.NoError(t, recv.Close(context.Background()))
	require.Equal(t, int32(1), first.closeCalls.Load(), "a Close with no Run since the last Close is a no-op")

	run()
	require.Equal(t, 2, builder.buildCount(), "the restarted Run builds a fresh stack")
	second := builder.stackAt(1)
	require.NotSame(t, first, second)
	require.Same(t, second, recv.currentClient())

	require.NoError(t, recv.Close(context.Background()))
	require.Equal(t, int32(1), second.closeCalls.Load(), "rebuilt stack closed exactly once")
	require.Equal(t, int32(1), first.closeCalls.Load(), "first stack is not closed again")
	require.Nil(t, recv.currentClient())
}
