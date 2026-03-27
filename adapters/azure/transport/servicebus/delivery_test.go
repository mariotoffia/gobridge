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

// verifies Delivery.Envelope returns the wrapped domain envelope.
func TestDelivery_Envelope(t *testing.T) {
	env := &domain.Envelope{ID: "msg-1", Subject: "test"}
	mock := &mockASBClient{}
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	d := newDelivery(context.Background(), env, mock, nil, msg, 30*time.Second, false, nil)

	if d.Envelope() != env {
		t.Fatal("Envelope() should return the original envelope")
	}
}

// verifies Ack completes the underlying Service Bus message.
func TestDelivery_Ack_CompletesMessage(t *testing.T) {
	mock := &mockASBClient{}
	env := &domain.Envelope{ID: "msg-1"}
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	d := newDelivery(context.Background(), env, mock, nil, msg, 30*time.Second, false, nil)

	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("Ack failed: %v", err)
	}

	mock.mu.Lock()
	count := len(mock.CompleteCalls)
	mock.mu.Unlock()

	if count != 1 {
		t.Fatalf("expected 1 CompleteMessage call, got %d", count)
	}
	if mock.CompleteCalls[0] != msg {
		t.Fatal("CompleteMessage called with wrong message")
	}
}

// verifies Ack maps CompleteMessage failures to a BridgeError.
func TestDelivery_Ack_Error(t *testing.T) {
	mock := &mockASBClient{
		CompleteMessageFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.CompleteMessageOptions) error {
			return errors.New("unauthorized access")
		},
	}
	env := &domain.Envelope{ID: "msg-1"}
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	d := newDelivery(context.Background(), env, mock, nil, msg, 30*time.Second, false, nil)

	err := d.Ack(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	_, ok := domain.AsBridgeError(err)
	if !ok {
		t.Fatalf("expected BridgeError, got %T", err)
	}
}

// verifies Retry abandons the message for redelivery.
func TestDelivery_Retry_AbandonsMessage(t *testing.T) {
	mock := &mockASBClient{}
	env := &domain.Envelope{ID: "msg-1"}
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	d := newDelivery(context.Background(), env, mock, nil, msg, 30*time.Second, false, nil)

	if err := d.Retry(context.Background(), 0, errors.New("transient")); err != nil {
		t.Fatalf("Retry failed: %v", err)
	}

	mock.mu.Lock()
	count := len(mock.AbandonCalls)
	mock.mu.Unlock()

	if count != 1 {
		t.Fatalf("expected 1 AbandonMessage call, got %d", count)
	}
	if mock.AbandonCalls[0] != msg {
		t.Fatal("AbandonMessage called with wrong message")
	}
}

// verifies Retry maps AbandonMessage failures to a BridgeError.
func TestDelivery_Retry_Error(t *testing.T) {
	mock := &mockASBClient{
		AbandonMessageFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.AbandonMessageOptions) error {
			return errors.New("connection reset")
		},
	}
	env := &domain.Envelope{ID: "msg-1"}
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	d := newDelivery(context.Background(), env, mock, nil, msg, 30*time.Second, false, nil)

	err := d.Retry(context.Background(), 0, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	_, ok := domain.AsBridgeError(err)
	if !ok {
		t.Fatalf("expected BridgeError, got %T", err)
	}
}

// verifies Extend renews the message lock.
func TestDelivery_Extend_RenewsLock(t *testing.T) {
	mock := &mockASBClient{}
	env := &domain.Envelope{ID: "msg-1"}
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	d := newDelivery(context.Background(), env, mock, nil, msg, 30*time.Second, false, nil)

	if err := d.Extend(context.Background(), time.Now().Add(60*time.Second)); err != nil {
		t.Fatalf("Extend failed: %v", err)
	}

	mock.mu.Lock()
	count := len(mock.RenewCalls)
	mock.mu.Unlock()

	if count != 1 {
		t.Fatalf("expected 1 RenewMessageLock call, got %d", count)
	}
	if mock.RenewCalls[0] != msg {
		t.Fatal("RenewMessageLock called with wrong message")
	}
}

// verifies Extend maps RenewMessageLock failures to a BridgeError.
func TestDelivery_Extend_Error(t *testing.T) {
	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			return errors.New("network error")
		},
	}
	env := &domain.Envelope{ID: "msg-1"}
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	d := newDelivery(context.Background(), env, mock, nil, msg, 30*time.Second, false, nil)

	err := d.Extend(context.Background(), time.Now().Add(60*time.Second))
	if err == nil {
		t.Fatal("expected error")
	}
	_, ok := domain.AsBridgeError(err)
	if !ok {
		t.Fatalf("expected BridgeError, got %T", err)
	}
}

// verifies auto-extend periodically renews the lock while the delivery is active.
func TestDelivery_AutoExtend_CallsRenew(t *testing.T) {
	var renewCount atomic.Int32
	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			renewCount.Add(1)
			return nil
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	d := newDelivery(context.Background(), env, mock, nil, msg, 2*time.Second, true, nil)

	time.Sleep(1500 * time.Millisecond)
	d.stop()
	time.Sleep(100 * time.Millisecond)

	count := renewCount.Load()
	if count < 1 {
		t.Fatalf("expected at least 1 auto-extend renew call, got %d", count)
	}
}

// verifies auto-extend stops after Ack.
func TestDelivery_AutoExtend_StopsOnAck(t *testing.T) {
	var renewCount atomic.Int32
	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			renewCount.Add(1)
			return nil
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	d := newDelivery(context.Background(), env, mock, nil, msg, 2*time.Second, true, nil)

	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("Ack failed: %v", err)
	}

	time.Sleep(1500 * time.Millisecond)

	count := renewCount.Load()
	if count > 0 {
		t.Fatalf("auto-extend should not fire after Ack, got %d calls", count)
	}
}

// verifies auto-extend stops after Retry.
func TestDelivery_AutoExtend_StopsOnRetry(t *testing.T) {
	var renewCount atomic.Int32
	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			renewCount.Add(1)
			return nil
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	d := newDelivery(context.Background(), env, mock, nil, msg, 2*time.Second, true, nil)

	if err := d.Retry(context.Background(), 0, nil); err != nil {
		t.Fatalf("Retry failed: %v", err)
	}

	time.Sleep(1500 * time.Millisecond)

	count := renewCount.Load()
	if count > 0 {
		t.Fatalf("auto-extend should not fire after Retry, got %d calls", count)
	}
}

// verifies renew is not called when auto-extend is disabled.
func TestDelivery_NoAutoExtend(t *testing.T) {
	var renewCount atomic.Int32
	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			renewCount.Add(1)
			return nil
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	d := newDelivery(context.Background(), env, mock, nil, msg, 2*time.Second, false, nil)

	time.Sleep(1500 * time.Millisecond)
	d.stop()

	if renewCount.Load() > 0 {
		t.Fatal("auto-extend should not fire when disabled")
	}
}

// verifies calling stop multiple times is safe and Ack still succeeds.
func TestDelivery_MultipleStopsAreSafe(t *testing.T) {
	mock := &mockASBClient{}
	env := &domain.Envelope{ID: "msg-1"}
	msg := &azservicebus.ReceivedMessage{MessageID: "test-msg"}
	d := newDelivery(context.Background(), env, mock, nil, msg, 30*time.Second, true, nil)

	d.stop()
	d.stop()
	d.stop()

	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("Ack after multiple stops should succeed: %v", err)
	}
}

// verifies auto-extend scheduling respects ReceivedMessage.LockedUntil when set.
func TestDelivery_AutoExtend_UsesLockedUntil(t *testing.T) {
	var renewCount atomic.Int32
	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			renewCount.Add(1)
			return nil
		},
	}

	lockedUntil := time.Now().Add(4 * time.Second)
	env := &domain.Envelope{ID: "msg-1"}
	msg := &azservicebus.ReceivedMessage{
		MessageID:  "test-msg",
		LockedUntil: &lockedUntil,
	}
	d := newDelivery(context.Background(), env, mock, nil, msg, 30*time.Second, true, nil)

	time.Sleep(2500 * time.Millisecond)
	d.stop()
	time.Sleep(100 * time.Millisecond)

	count := renewCount.Load()
	if count < 1 {
		t.Fatalf("expected at least 1 renew call using LockedUntil-based interval (~2s), got %d", count)
	}
}
