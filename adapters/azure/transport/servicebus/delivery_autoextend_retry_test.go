package servicebus

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
)

// TestAutoExtendRetriesTransientThenSucceeds verifies the auto-extend loop
// tolerates a transient RenewMessageLock failure and keeps renewing.
func TestAutoExtendRetriesTransientThenSucceeds(t *testing.T) {
	t.Parallel()

	var renews atomic.Int32
	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			n := renews.Add(1)
			if n == 1 {
				return errors.New("transient")
			}
			return nil
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	fake := clocktest.New()
	d := newDelivery(context.Background(), env, mock, nil, msg, 2*time.Second, true, nil, nil, fake)
	defer d.stop()

	// OTHER: real-time sync for clocktest.Fake goroutine startup
	deadline := time.Now().Add(2 * time.Second)
	for fake.TickerCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(1 * time.Millisecond)
	}

	fake.Advance(1100 * time.Millisecond)  // first tick fires (fails)
	time.Sleep(20 * time.Millisecond)       // OTHER: let goroutine process tick
	fake.Advance(1 * time.Second)           // second tick fires (succeeds)
	time.Sleep(20 * time.Millisecond)       // OTHER: let goroutine process tick

	if n := renews.Load(); n < 2 {
		t.Fatalf("expected at least 2 renew attempts (fail then succeed), got %d", n)
	}
}

// TestAutoExtendStopsAfterMaxConsecutiveFailures verifies the loop exits after
// autoExtendMaxFailures consecutive RenewMessageLock errors.
func TestAutoExtendStopsAfterMaxConsecutiveFailures(t *testing.T) {
	t.Parallel()

	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			return errors.New("always fail")
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	fake := clocktest.New()
	d := newDelivery(context.Background(), env, mock, nil, msg, 2*time.Second, true, nil, nil, fake)
	defer d.stop()

	// OTHER: real-time sync for clocktest.Fake goroutine startup
	deadline := time.Now().Add(2 * time.Second)
	for fake.TickerCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(1 * time.Millisecond)
	}

	for i := 0; i < autoExtendMaxFailures; i++ {
		fake.Advance(1100 * time.Millisecond) // tick fires (always fails)
		time.Sleep(20 * time.Millisecond)      // OTHER: let goroutine process tick
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

	var callCount atomic.Int32
	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			n := callCount.Add(1)
			if n%2 == 1 {
				return errors.New("odd call fails")
			}
			return nil
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	fake := clocktest.New()
	d := newDelivery(context.Background(), env, mock, nil, msg, 2*time.Second, true, nil, nil, fake)
	defer d.stop()

	// OTHER: real-time sync for clocktest.Fake goroutine startup
	deadline := time.Now().Add(2 * time.Second)
	for fake.TickerCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(1 * time.Millisecond)
	}

	// Advance through enough ticks to exceed autoExtendMaxFailures total calls.
	// Pattern: odd=fail, even=succeed, so consecutive failures never reach the max.
	ticks := autoExtendMaxFailures + 2
	for i := 0; i < ticks; i++ {
		fake.Advance(1100 * time.Millisecond) // tick fires
		time.Sleep(20 * time.Millisecond)      // OTHER: let goroutine process tick
	}

	total := callCount.Load()
	if total <= int32(autoExtendMaxFailures) {
		t.Fatalf("expected more than %d total calls (interleaved), got %d", autoExtendMaxFailures, total)
	}
}
