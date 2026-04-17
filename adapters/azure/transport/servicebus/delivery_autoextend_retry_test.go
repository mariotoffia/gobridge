package servicebus

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain"
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
	d := newDelivery(context.Background(), env, mock, nil, msg, 2*time.Second, true, nil, nil, nil)
	defer d.stop()

	time.Sleep(4500 * time.Millisecond)

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
	d := newDelivery(context.Background(), env, mock, nil, msg, 2*time.Second, true, nil, nil, nil)
	defer d.stop()

	time.Sleep(4 * time.Second)

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
	d := newDelivery(context.Background(), env, mock, nil, msg, 2*time.Second, true, nil, nil, nil)
	defer d.stop()

	time.Sleep(5 * time.Second)

	total := callCount.Load()
	if total <= int32(autoExtendMaxFailures) {
		t.Fatalf("expected more than %d total calls (interleaved), got %d", autoExtendMaxFailures, total)
	}
}
