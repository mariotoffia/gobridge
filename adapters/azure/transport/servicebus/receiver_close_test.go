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
// Tests for Receiver.Close lifecycle
// ---------------------------------------------------------------------------

// verifies that Close forwards the caller-provided context to the
// underlying resource close operations, preserving any deadline.
func TestReceiver_Close_UsesCallerContext(t *testing.T) {
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
	cancel()

	_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer closeCancel()
	_ = recv.Close(closeCtx)

	if !deadlineOK {
		t.Fatal("close context should have a deadline set (not unbounded)")
	}

	remaining := time.Until(capturedDeadline)
	if remaining < 8*time.Second || remaining > 12*time.Second {
		t.Fatalf("close context deadline should be ~10s from now, got %v remaining", remaining)
	}
}

// verifies that if close operations take longer than the caller's
// timeout, the context is cancelled (deadline exceeded).
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
	cancel()

	_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer closeCancel()

	start := time.Now()
	_ = recv.Close(closeCtx)
	elapsed := time.Since(start)

	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("close function did not complete within expected time")
	}

	if closeErr == nil {
		t.Fatal("expected close context to be cancelled with deadline exceeded")
	}
	if !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", closeErr)
	}

	if elapsed > 2*time.Second {
		t.Fatalf("Close took %v, expected it to be bounded by ~500ms timeout", elapsed)
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

	runErr := recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", runErr)
	}

	start := time.Now()
	_ = recv.Close(context.Background())
	elapsed := time.Since(start)

	if !closeCalled.Load() {
		t.Fatal("Close should have been called")
	}

	if elapsed > 2*time.Second {
		t.Fatalf("fast close took %v, expected < 2s", elapsed)
	}
}

// verifies that all three closeable resources (client, scheduler, asbClient)
// are closed by Close.
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

	recv.scheduler = schedulerMock

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	_ = recv.Close(context.Background())

	if clientMock.closeCalls.Load() != 1 {
		t.Fatalf("client Close called %d times, want 1", clientMock.closeCalls.Load())
	}

	if schedulerMock.closeCalls.Load() != 1 {
		t.Fatalf("scheduler Close called %d times, want 1", schedulerMock.closeCalls.Load())
	}
}

// verifies that a nil scheduler does not cause a panic during Close.
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

	_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	_ = recv.Close(context.Background())

	if mock.closeCalls.Load() != 1 {
		t.Fatalf("client Close called %d times, want 1", mock.closeCalls.Load())
	}
}

// verifies that a nil asbClient does not cause a panic during Close.
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

	_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	_ = recv.Close(context.Background())

	if mock.closeCalls.Load() != 1 {
		t.Fatalf("client Close called %d times, want 1", mock.closeCalls.Load())
	}
}

// verifies that Close with a fresh context works after Run exits with a
// cancelled context (close context is independent of Run context).
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
	cancel()

	_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	_ = recv.Close(context.Background())

	if closeCtxErr != nil {
		t.Fatalf("close context should NOT be cancelled at call time (err = %v); "+
			"caller must use a fresh context", closeCtxErr)
	}
}

// verifies that when the client does not implement Close, cleanup still
// proceeds without errors (the type assertion in Close handles this case).
func TestReceiver_Close_ClientWithoutCloseInterface(t *testing.T) {
	t.Parallel()

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

	_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	// Should not panic even though the mock does not implement Close.
	_ = recv.Close(context.Background())
}

// verifies that Close returns nil even when underlying close returns an error.
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

	runErr := recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("expected context.Canceled from Run, got %v", runErr)
	}

	closeErr := recv.Close(context.Background())
	if closeErr != nil {
		t.Fatalf("Close should return nil even when underlying close fails, got %v", closeErr)
	}

	if mock.closeCalls.Load() != 1 {
		t.Fatalf("Close should have been called exactly once, got %d", mock.closeCalls.Load())
	}
}

// verifies that calling Close multiple times is idempotent — the
// underlying resources are closed exactly once.
func TestReceiver_Close_Idempotent(t *testing.T) {
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

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	_ = recv.Close(context.Background())
	_ = recv.Close(context.Background())
	_ = recv.Close(context.Background())

	if mock.closeCalls.Load() != 1 {
		t.Fatalf("client Close called %d times, want exactly 1", mock.closeCalls.Load())
	}
}
