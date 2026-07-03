package sqs

// Production-readiness regression tests for the receive-batch
// auto-extend defect: visibility clocks for every message of a
// ReceiveMessage batch start ticking at receive, but deliveries used to
// be created serially — the auto-extend goroutine for batch-mate N+1
// only started after emit(N) returned. Under MaxInFlight saturation a
// later batch-mate burned its whole visibility window with no extension
// running → expiry → source redelivery → duplicate amplification and a
// stale receipt handle failing the eventual Ack. The poll loop now
// constructs ALL deliveries (starting auto-extend) before the emit loop.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// visibilitySnapshot returns a race-free copy of the mock's recorded
// ChangeMessageVisibility calls (auto-extend goroutines append
// concurrently under the mock's mutex).
func visibilitySnapshot(m *mockSQSClient) []awssqs.ChangeMessageVisibilityInput {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]awssqs.ChangeMessageVisibilityInput, len(m.ChangeVisibilityCalls))
	copy(out, m.ChangeVisibilityCalls)
	return out
}

// twoMessageBatchMock returns a mock whose first ReceiveMessage yields
// two messages (rh-1, rh-2) and whose subsequent polls block on ctx.
func twoMessageBatchMock() *mockSQSClient {
	var polls atomic.Int32
	return &mockSQSClient{
		ReceiveMessageFn: func(ctx context.Context, _ *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			if polls.Add(1) > 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return &awssqs.ReceiveMessageOutput{Messages: []sqstypes.Message{
				{MessageId: aws.String("m-1"), ReceiptHandle: aws.String("rh-1"), Body: aws.String("b1")},
				{MessageId: aws.String("m-2"), ReceiptHandle: aws.String("rh-2"), Body: aws.String("b2")},
			}}, nil
		},
	}
}

// Verifies the second batch-mate's auto-extend runs WHILE the first
// message's emit is still blocked (the saturation scenario): both
// tickers must be registered before any emit returns, and advancing the
// fake clock by one auto-extend interval must extend rh-2's visibility
// even though emit #1 has not returned yet.
func TestReceiver_BatchMateAutoExtendRunsWhileFirstEmitBlocked(t *testing.T) {
	fake := clocktest.New()
	mock := twoMessageBatchMock()

	r, err := NewReceiver(ReceiverConfig{
		QueueURL:          "https://q",
		VisibilityTimeout: 30, // auto-extend interval = 15s
		Client:            mock,
		Clock:             fake,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	firstEmitEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var emits atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(ectx context.Context, _ ports.Delivery) error {
			if emits.Add(1) == 1 {
				close(firstEmitEntered)
				select {
				case <-releaseFirst:
				case <-ectx.Done():
				}
			}
			return nil
		})
	}()

	wait.RequireClosed(t, firstEmitEntered, time.Second)

	// Both deliveries must already exist with auto-extend running: two
	// tickers registered with the fake clock while emit #1 is blocked.
	wait.Until(t, time.Second, "auto-extend tickers for BOTH batch-mates registered", func() bool {
		return fake.TickerCount() >= 2
	})

	fake.Advance(15 * time.Second)

	wait.Until(t, time.Second, "batch-mate rh-2 visibility extended while emit #1 is blocked", func() bool {
		for _, c := range visibilitySnapshot(mock) {
			if aws.ToString(c.ReceiptHandle) == "rh-2" {
				return true
			}
		}
		return false
	})
	if n := emits.Load(); n != 1 {
		t.Fatalf("emit count = %d, want 1 (emit is serial; rh-2 must be extended before its emit)", n)
	}

	close(releaseFirst)
	cancel()
	if err := wait.RequireReceive(t, done, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
}

// Verifies the emit-error path of batch pre-creation: when emit fails,
// the current AND all not-yet-emitted batch-mates' delivery contexts are
// cancelled so their pre-started auto-extend goroutines stop (no leaked
// goroutines endlessly extending messages the runtime will never see).
func TestReceiver_EmitErrorCancelsPendingBatchMates(t *testing.T) {
	fake := clocktest.New()
	mock := twoMessageBatchMock()

	r, err := NewReceiver(ReceiverConfig{
		QueueURL:          "https://q",
		VisibilityTimeout: 30,
		Client:            mock,
		Clock:             fake,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	emitErr := errors.New("route runner rejected delivery")
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- r.Run(context.Background(), func(ectx context.Context, _ ports.Delivery) error {
			select {
			case <-release:
			case <-ectx.Done():
			}
			return emitErr
		})
	}()

	// Both auto-extend goroutines must be live before the failure.
	wait.Until(t, time.Second, "both auto-extend tickers registered", func() bool {
		return fake.TickerCount() >= 2
	})

	close(release)
	if err := wait.RequireReceive(t, done, time.Second); !errors.Is(err, emitErr) {
		t.Fatalf("Run returned %v, want the emit error", err)
	}

	// pending[i:] cancellation stops every auto-extend loop; each loop
	// stops its ticker on exit.
	wait.Until(t, time.Second, "all auto-extend tickers stopped after emit error", func() bool {
		return fake.TickerCount() == 0
	})
}
