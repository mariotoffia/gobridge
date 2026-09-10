package servicebus

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestRunSessionRenewer_TickFiredMidHandlerSurvivesRePacing forces the exact
// interleaving that used to make the renewer tests fail intermittently.
//
// The renewer re-paces itself with ticker.Reset at the END of each handler. A
// test driving it advances the clock from its own goroutine, so the advance can
// land while the handler is still running: the tick is fired and buffered, and
// the Reset arrives after it. Real time.Ticker discards that tick and gets away
// with it because wall time keeps flowing; a fake clock only moves when the test
// says so, so a discarded tick is never re-delivered and the renewer goes
// permanently silent.
//
// Here the interleaving is not left to the scheduler: the mock blocks inside
// RenewMessageLock, so the second Advance provably happens BEFORE the handler's
// Reset. Mutation check: restore the channel drain in clocktest's
// fakeTicker.Reset and this test hangs until its wait deadline.
func TestRunSessionRenewer_TickFiredMidHandlerSurvivesRePacing(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var renews atomic.Int32

	client := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			renews.Add(1)
			entered <- struct{}{}
			<-release // hold the handler open, before its ticker.Reset
			return nil
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:    "q",
		SessionID:    "s1",
		LockDuration: 10 * time.Second,
		Client:       client,
		Clock:        fake,
	}, nil)
	require.NoError(t, err)
	require.NoError(t, recv.ensureClient(context.Background()))

	renewCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go recv.runSessionRenewer(renewCtx, 5*time.Second)

	wait.Until(t, 5*time.Second, "renewer registers its ticker", func() bool {
		return fake.TickerCount() >= 1
	})

	fake.Advance(6 * time.Second) // tick 1
	wait.RequireReceive(t, entered, 5*time.Second)
	// The handler is now parked inside RenewMessageLock, so ticker.Reset has
	// demonstrably not run yet.

	fake.Advance(6 * time.Second) // tick 2, fired mid-handler
	close(release)                // handler completes and re-paces the ticker

	wait.RequireReceive(t, entered, 5*time.Second)
	require.GreaterOrEqual(t, renews.Load(), int32(2),
		"the tick fired while the handler was running must still be delivered after it re-paces")
}
