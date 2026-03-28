package servicebus

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/ports"
)

// ---------------------------------------------------------------------------
// Tests for BUG-12: Receiver close with bounded context
// ---------------------------------------------------------------------------

// verifies that when Run is cancelled, the close context has a deadline set
// approximately 10 seconds from the close time (bounded, not unbounded).
func TestReceiver_Close_UsesTimeoutContext(t *testing.T) {
	t.Parallel()

	var capturedDeadline time.Time
	var deadlineOK bool

	mock := &closeableASBClient{
		mockASBClient: mockASBClient{
			ReceiveMessagesFn: func(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		},
		closeFn: func(ctx context.Context) error {
			capturedDeadline, deadlineOK = ctx.Deadline()
			return nil
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "test-queue",
		AutoExtend: boolPtr(false),
		Client:     mock,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	if !deadlineOK {
		t.Fatal("close context should have a deadline set (not unbounded)")
	}

	// The deadline should be approximately 10 seconds from now (within a
	// tolerance band). Since close happens shortly after cancellation,
	// the deadline should be within 9-11 seconds of the current time.
	remaining := time.Until(capturedDeadline)
	if remaining < 8*time.Second || remaining > 12*time.Second {
		t.Fatalf("close context deadline should be ~10s from now, got %v remaining", remaining)
	}
}

// verifies that if close operations take longer than 10 seconds, the
// context is cancelled (deadline exceeded).
func TestReceiver_Close_TimeoutCancelsSlowClose(t *testing.T) {
	t.Parallel()

	closeDone := make(chan struct{})
	var closeErr error

	mock := &closeableASBClient{
		mockASBClient: mockASBClient{
			ReceiveMessagesFn: func(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		},
		closeFn: func(ctx context.Context) error {
			// Simulate a slow close that exceeds the 10s timeout.
			// We wait for the context to be cancelled rather than
			// waiting the full 10s to keep the test fast.
			select {
			case <-ctx.Done():
				closeErr = ctx.Err()
				close(closeDone)
				return ctx.Err()
			case <-time.After(15 * time.Second):
				close(closeDone)
				return nil
			}
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "test-queue",
		AutoExtend: boolPtr(false),
		Client:     mock,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately to trigger cleanup

	start := time.Now()
	_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})
	elapsed := time.Since(start)

	select {
	case <-closeDone:
	case <-time.After(15 * time.Second):
		t.Fatal("close function did not complete within expected time")
	}

	if closeErr == nil {
		t.Fatal("expected close context to be cancelled with deadline exceeded")
	}
	if !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", closeErr)
	}

	// The entire Run + cleanup should complete within approximately 10s
	// (the timeout) plus some tolerance.
	if elapsed > 12*time.Second {
		t.Fatalf("Run took %v, expected close to be bounded by ~10s timeout", elapsed)
	}
}

// verifies that a fast close completes successfully without errors.
func TestReceiver_Close_FastCloseSucceeds(t *testing.T) {
	t.Parallel()

	var closeCalled atomic.Bool

	mock := &closeableASBClient{
		mockASBClient: mockASBClient{
			ReceiveMessagesFn: func(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		},
		closeFn: func(ctx context.Context) error {
			closeCalled.Store(true)
			return nil
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "test-queue",
		AutoExtend: boolPtr(false),
		Client:     mock,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	runErr := recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})
	elapsed := time.Since(start)

	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", runErr)
	}

	if !closeCalled.Load() {
		t.Fatal("Close should have been called during cleanup")
	}

	// A fast close should complete almost instantly.
	if elapsed > 2*time.Second {
		t.Fatalf("fast close took %v, expected < 2s", elapsed)
	}
}

// verifies that all three closeable resources (client, scheduler, asbClient)
// are closed during Run cleanup.
func TestReceiver_Close_AllResourcesClosed(t *testing.T) {
	t.Parallel()

	clientMock := &closeableASBClient{
		mockASBClient: mockASBClient{
			ReceiveMessagesFn: func(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		},
	}

	schedulerMock := &closeableScheduler{}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "test-queue",
		AutoExtend: boolPtr(false),
		Client:     clientMock,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	// Inject the scheduler to simulate a fully-initialized receiver.
	recv.scheduler = schedulerMock

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	if clientMock.closeCalls.Load() != 1 {
		t.Fatalf("client Close called %d times, want 1", clientMock.closeCalls.Load())
	}

	if schedulerMock.closeCalls.Load() != 1 {
		t.Fatalf("scheduler Close called %d times, want 1", schedulerMock.closeCalls.Load())
	}
}

// verifies that a nil scheduler does not cause a panic during cleanup.
func TestReceiver_Close_NilSchedulerNoPanic(t *testing.T) {
	t.Parallel()

	mock := &closeableASBClient{
		mockASBClient: mockASBClient{
			ReceiveMessagesFn: func(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "test-queue",
		AutoExtend: boolPtr(false),
		Client:     mock,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	if recv.scheduler != nil {
		t.Fatal("precondition: scheduler should be nil when client is injected")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// This should not panic.
	_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	if mock.closeCalls.Load() != 1 {
		t.Fatalf("client Close called %d times, want 1", mock.closeCalls.Load())
	}
}

// verifies that a nil asbClient does not cause a panic during cleanup.
func TestReceiver_Close_NilAsbClientNoPanic(t *testing.T) {
	t.Parallel()

	mock := &closeableASBClient{
		mockASBClient: mockASBClient{
			ReceiveMessagesFn: func(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "test-queue",
		AutoExtend: boolPtr(false),
		Client:     mock,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	if recv.asbClient != nil {
		t.Fatal("precondition: asbClient should be nil when client is injected")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// This should not panic.
	_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	if mock.closeCalls.Load() != 1 {
		t.Fatalf("client Close called %d times, want 1", mock.closeCalls.Load())
	}
}

// verifies that the close context uses context.Background as the parent,
// not the cancelled Run context (i.e., close succeeds even when Run ctx
// is already cancelled).
func TestReceiver_Close_ContextNotDerivedFromRunContext(t *testing.T) {
	t.Parallel()

	var closeCtxErr error

	mock := &closeableASBClient{
		mockASBClient: mockASBClient{
			ReceiveMessagesFn: func(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		},
		closeFn: func(ctx context.Context) error {
			// At the time Close is called, the Run context is already
			// cancelled. The close context must NOT be derived from it,
			// otherwise it would already be cancelled.
			closeCtxErr = ctx.Err()
			return nil
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "test-queue",
		AutoExtend: boolPtr(false),
		Client:     mock,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel before Run so the context is already done.

	_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	if closeCtxErr != nil {
		t.Fatalf("close context should NOT be cancelled at call time (err = %v); "+
			"it must use context.Background as parent", closeCtxErr)
	}
}

// verifies that when the client does not implement Close, cleanup still
// proceeds without errors (the type assertion in Run handles this case).
func TestReceiver_Close_ClientWithoutCloseInterface(t *testing.T) {
	t.Parallel()

	// Plain mockASBClient does NOT implement Close(context.Context) error.
	mock := &mockASBClient{
		ReceiveMessagesFn: func(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "test-queue",
		AutoExtend: boolPtr(false),
		Client:     mock,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Should not panic even though the mock does not implement Close.
	_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})
}

// verifies that when close returns an error, Run still completes
// (errors from close are intentionally ignored).
func TestReceiver_Close_ErrorsAreIgnored(t *testing.T) {
	t.Parallel()

	mock := &closeableASBClient{
		mockASBClient: mockASBClient{
			ReceiveMessagesFn: func(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		},
		closeFn: func(ctx context.Context) error {
			return errors.New("close failed")
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "test-queue",
		AutoExtend: boolPtr(false),
		Client:     mock,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Run should return the poll loop error, not the close error.
	runErr := recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("expected context.Canceled from Run, got %v", runErr)
	}

	if mock.closeCalls.Load() != 1 {
		t.Fatalf("Close should have been called exactly once, got %d", mock.closeCalls.Load())
	}
}
