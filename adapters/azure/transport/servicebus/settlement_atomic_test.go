package servicebus

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// --- c6-settle-atomic: exactly one settlement outcome wins -----------------

// TestDelivery_Settlement_ExactlyOneOutcomeUnderConcurrency drives many
// concurrent Ack/Retry calls at a SINGLE delivery and asserts that exactly
// ONE terminal broker call (CompleteMessage from Ack, or AbandonMessage
// from an immediate Retry) is ever made. This is the settlement-atomicity
// guard: the panic-recovery path, a settlement-timeout path and a runtime
// retry can all reach settlement, and without an atomic settled-state CAS
// they would double-settle (Complete-after-Abandon, or a duplicate
// scheduled copy). Race-clean by construction: only the CAS winner touches
// the client, so `-race` sees a single writer.
//
// Mutation: on the unfixed code (sync.Once only stops auto-renewal, no
// settled CAS) every goroutine proceeds to its terminal call, so the total
// is the number of goroutines, not 1, and the assertion fails.
func TestDelivery_Settlement_ExactlyOneOutcomeUnderConcurrency(t *testing.T) {
	t.Parallel()

	const iterations = 200
	const goroutines = 8

	for iter := 0; iter < iterations; iter++ {
		mock := &mockASBClient{}
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1"})
		msg := &azservicebus.ReceivedMessage{MessageID: "settle-me"}
		// No scheduler → an immediate Retry (after=0) settles via Abandon,
		// so every contender resolves to a single Complete or Abandon.
		d := newDelivery(context.Background(), env, mock, nil, msg,
			deliveryTuning{}, nil, nil, nil, clocktest.New())

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for g := 0; g < goroutines; g++ {
			g := g
			go func() {
				defer wg.Done()
				<-start // release all contenders at once
				if g%2 == 0 {
					_ = d.Ack(context.Background())
				} else {
					_ = d.Retry(context.Background(), 0, nil)
				}
			}()
		}
		close(start)
		wg.Wait()

		mock.mu.Lock()
		completes := len(mock.CompleteCalls)
		abandons := len(mock.AbandonCalls)
		mock.mu.Unlock()

		require.Equalf(t, 1, completes+abandons,
			"exactly one terminal settlement must win (iter %d): completes=%d abandons=%d",
			iter, completes, abandons)
	}
}

// TestDelivery_Extend_NoOpAfterSettlement pins the Extend guard: once a
// delivery has settled, a late visibility-extension request must be a
// no-op and never call RenewMessageLock (which would race the terminal
// broker call).
//
// Mutation: drop the `d.settled.Load()` guard in Extend and the post-Ack
// Extend forwards to RenewMessageLock, so renewCount becomes 1.
func TestDelivery_Extend_NoOpAfterSettlement(t *testing.T) {
	t.Parallel()

	var renewCount atomic.Int32
	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			renewCount.Add(1)
			return nil
		},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1"})
	msg := &azservicebus.ReceivedMessage{MessageID: "settle-me"}
	d := newDelivery(context.Background(), env, mock, nil, msg,
		deliveryTuning{}, nil, nil, nil, clocktest.New())

	require.NoError(t, d.Ack(context.Background()))
	require.NoError(t, d.Extend(context.Background(), time.Time{}))
	require.Zero(t, renewCount.Load(), "Extend must be a no-op after settlement")
}

// --- c6-renew-stop: renewal stays alive UNTIL settlement returns -----------

// TestDelivery_Ack_KeepsRenewingUntilSettlementReturns proves lock
// auto-renewal is NOT stopped before the terminal CompleteMessage returns.
// CompleteMessage is held open; while it blocks, a renewal tick is fired
// and must still reach the broker (RenewMessageLock). If renewal were
// stopped first (the bug), a throttled/slow Complete would outlive the
// remaining lock and a second consumer could pick the message up
// (duplicate).
//
// Mutation: the unfixed Ack calls d.stop() BEFORE CompleteMessage, so the
// renewer is already cancelled when Complete blocks; advancing the clock
// fires no renewal and the `renewed` wait times out.
func TestDelivery_Ack_KeepsRenewingUntilSettlementReturns(t *testing.T) {
	t.Parallel()

	completeStarted := make(chan struct{})
	completeRelease := make(chan struct{})
	renewed := make(chan struct{}, 1)
	var renewCount atomic.Int32

	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			renewCount.Add(1)
			signal(renewed)
			return nil
		},
		CompleteMessageFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.CompleteMessageOptions) error {
			close(completeStarted)
			<-completeRelease // hold the settlement open
			return nil
		},
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1"})
	msg := &azservicebus.ReceivedMessage{MessageID: "settle-me"}
	clk := newSignalClock()
	d := newDelivery(context.Background(), env, mock, nil, msg,
		deliveryTuning{lockDuration: 2 * time.Second, autoExtend: true}, nil, nil, nil, clk)

	<-clk.started // renewal ticker armed

	ackDone := make(chan error, 1)
	go func() { ackDone <- d.Ack(context.Background()) }()

	<-completeStarted // settlement is now in-flight and blocked

	// lockDuration/2 = 1s renewal interval. Firing it while Complete is
	// blocked must still renew the lock.
	clk.Advance(1 * time.Second)
	select {
	case <-renewed:
	case <-time.After(2 * time.Second):
		t.Fatal("lock renewal did not fire during a blocked settlement: renewal was stopped BEFORE settlement returned")
	}
	require.GreaterOrEqual(t, renewCount.Load(), int32(1),
		"lock renewal must stay alive until settlement returns")

	close(completeRelease) // let Ack finish
	require.NoError(t, <-ackDone)

	<-clk.stopped // the deferred stop() cancelled the renewer AFTER settlement
}

// TestAutoExtendLoop_NoRenewAfterSettlementReturns is the fix #3 companion
// to the renew-through-settle test above. Once the terminal broker call has
// RETURNED (settleReturned set), a still-pending renewal tick must NOT fire
// RenewMessageLock: the message is already settled, so a renew would hit an
// already-completed message — a spurious LockLost warn plus a bogus
// MetricASBLockRenewalFailures bump. The loop must instead observe the flag
// and return (its deferred ticker.Stop signals `stopped`).
//
// The two outcomes are mutually exclusive, so the select is deterministic:
//
//   - fixed:    guard returns → ticker.Stop → `stopped` fires, renewCount==0.
//   - mutation: drop the `d.settleReturned.Load()` guard → the tick calls
//     RenewMessageLock → `renewed` fires and the test FAILs.
func TestAutoExtendLoop_NoRenewAfterSettlementReturns(t *testing.T) {
	t.Parallel()

	renewed := make(chan struct{}, 1)
	var renewCount atomic.Int32
	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			renewCount.Add(1)
			signal(renewed)
			return nil
		},
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1"})
	msg := &azservicebus.ReceivedMessage{MessageID: "settle-me"}
	clk := newSignalClock()
	// autoExtend:false so newDelivery does NOT start its own loop; the test
	// drives autoExtendLoop directly to isolate the post-settlement guard.
	d := newDelivery(context.Background(), env, mock, nil, msg,
		deliveryTuning{lockDuration: 2 * time.Second}, nil, nil, nil, clk)

	// The terminal settlement broker call has already returned.
	d.settleReturned.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.autoExtendLoop(ctx, time.Second)

	<-clk.started            // renewal ticker armed, loop parked in select
	clk.Advance(time.Second) // fire one renewal tick

	select {
	case <-clk.stopped:
		require.Zero(t, renewCount.Load(),
			"no lock renewal may fire after the terminal settlement call returned")
	case <-renewed:
		t.Fatal("renewal fired AFTER settlement returned: the auto-extend loop is not gated by settleReturned")
	}
}
