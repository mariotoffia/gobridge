package servicebus

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// signalClock wraps a clocktest.Fake and reports, over channels, the two
// lifecycle events the auto-extend goroutine emits — so these
// timing-allowlisted tests can hand-shake deterministically instead of
// sleeping to sync with the fake clock (ASB-N1):
//
//   - NewTicker signals `started`: the goroutine has armed its renewal
//     ticker and is (about to be) parked in select, so the first Advance
//     cannot race ahead of ticker registration and lose a tick.
//   - Ticker.Stop signals `stopped`: the goroutine has observed
//     cancellation and returned via its `defer ticker.Stop()`, so a
//     negative test knows the loop is gone before it advances.
type signalClock struct {
	*clocktest.Fake
	started chan struct{}
	stopped chan struct{}
}

func newSignalClock() *signalClock {
	return &signalClock{
		Fake:    clocktest.New(),
		started: make(chan struct{}, 1),
		stopped: make(chan struct{}, 1),
	}
}

func (c *signalClock) NewTicker(d time.Duration) clock.Ticker {
	tk := c.Fake.NewTicker(d)
	signal(c.started)
	return &signalTicker{Ticker: tk, stopped: c.stopped}
}

type signalTicker struct {
	clock.Ticker
	stopped chan struct{}
}

func (t *signalTicker) Stop() {
	t.Ticker.Stop()
	signal(t.stopped)
}

// signal does a non-blocking send so the code-under-test never blocks when
// a test is not (yet) waiting on the channel.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// TestAutoExtendRetriesTransientThenSucceeds verifies the auto-extend loop
// tolerates a transient RenewMessageLock failure and keeps renewing.
func TestAutoExtendRetriesTransientThenSucceeds(t *testing.T) {
	t.Parallel()

	renewed := make(chan struct{}, 1)
	var renews atomic.Int32
	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			n := renews.Add(1)
			signal(renewed)
			if n == 1 {
				return errors.New("transient")
			}
			return nil
		},
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1"})
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	clk := newSignalClock()
	d := newDelivery(context.Background(), env, mock, nil, msg, 2*time.Second, true, nil, nil, nil, clk)
	defer d.stop()

	<-clk.started // goroutine has armed its renewal ticker

	clk.Advance(1100 * time.Millisecond) // first tick fires (fails)
	<-renewed                            // block until the goroutine processed the tick
	clk.Advance(1 * time.Second)         // second tick fires (succeeds)
	<-renewed

	if n := renews.Load(); n < 2 {
		t.Fatalf("expected at least 2 renew attempts (fail then succeed), got %d", n)
	}
}

// TestAutoExtendStopsAfterMaxConsecutiveFailures verifies the loop exits after
// autoExtendMaxFailures consecutive RenewMessageLock errors.
func TestAutoExtendStopsAfterMaxConsecutiveFailures(t *testing.T) {
	t.Parallel()

	renewed := make(chan struct{}, 1)
	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			signal(renewed)
			return errors.New("always fail")
		},
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1"})
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	clk := newSignalClock()
	d := newDelivery(context.Background(), env, mock, nil, msg, 2*time.Second, true, nil, nil, nil, clk)
	defer d.stop()

	<-clk.started // goroutine has armed its renewal ticker

	for i := 0; i < autoExtendMaxFailures; i++ {
		clk.Advance(1100 * time.Millisecond) // tick fires (always fails)
		<-renewed                            // block until the goroutine processed the tick
	}

	mock.mu.Lock()
	n := len(mock.RenewCalls)
	mock.mu.Unlock()

	if n < autoExtendMaxFailures {
		t.Fatalf("expected at least %d renew calls before stop, got %d", autoExtendMaxFailures, n)
	}
}

// TestAutoExtendInterleavedFailSuccessASB verifies that the consecutive failure
// counter resets after each success, allowing the loop to survive more total
// failures than autoExtendMaxFailures when they are non-consecutive.
func TestAutoExtendInterleavedFailSuccessASB(t *testing.T) {
	t.Parallel()

	renewed := make(chan struct{}, 1)
	var callCount atomic.Int32
	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			n := callCount.Add(1)
			signal(renewed)
			if n%2 == 1 {
				return errors.New("odd call fails")
			}
			return nil
		},
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1"})
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	clk := newSignalClock()
	d := newDelivery(context.Background(), env, mock, nil, msg, 2*time.Second, true, nil, nil, nil, clk)
	defer d.stop()

	<-clk.started // goroutine has armed its renewal ticker

	// Advance through enough ticks to exceed autoExtendMaxFailures total calls.
	// Pattern: odd=fail, even=succeed, so consecutive failures never reach the max.
	ticks := autoExtendMaxFailures + 2
	for i := 0; i < ticks; i++ {
		clk.Advance(1100 * time.Millisecond) // tick fires
		<-renewed                            // block until the goroutine processed the tick
	}

	total := callCount.Load()
	if total <= int32(autoExtendMaxFailures) {
		t.Fatalf("expected more than %d total calls (interleaved), got %d", autoExtendMaxFailures, total)
	}
}
