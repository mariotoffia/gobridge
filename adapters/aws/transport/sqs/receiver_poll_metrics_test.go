package sqs

import (
	"context"
	"errors"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestPollLoop_EmitsPollErrorsCounter is the regression for Finding 5 on the
// poll path: a failed ReceiveMessage must increment the SQSPollErrors counter,
// not merely log a warning. The loop is driven with a fake clock so the
// post-error backoff sleep blocks (no real time passes); cancelling the
// context releases it and the loop returns.
func TestPollLoop_EmitsPollErrorsCounter(t *testing.T) {
	t.Parallel()

	rec := &ports.RecordingExporter{}
	fake := clocktest.New()
	mock := &mockSQSClient{
		ReceiveMessageFn: func(context.Context, *awssqs.ReceiveMessageInput, ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			return nil, errors.New("poll boom")
		},
	}
	r, err := NewReceiver(ReceiverConfig{
		QueueURL: "http://test/q",
		Client:   mock,
		Metrics:  rec,
		Clock:    fake,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	r.storeClient(mock)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- r.pollLoop(ctx, "http://test/q", 10, func(context.Context, ports.Delivery) error { return nil })
	}()

	// The first poll error increments the counter before the loop blocks on
	// its (fake-clock) backoff sleep.
	wait.Until(t, time.Second, "poll error counted", func() bool {
		return len(rec.FindEntries(MetricSQSPollErrors)) >= 1
	})

	// Release the loop: cancellation is observed by the backoff select.
	cancel()
	if err := wait.RequireReceive(t, done, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("pollLoop returned %v, want context.Canceled", err)
	}

	if got := len(rec.FindEntries(MetricSQSPollErrors)); got < 1 {
		t.Fatalf("%s entries: want >= 1, got %d", MetricSQSPollErrors, got)
	}
}
