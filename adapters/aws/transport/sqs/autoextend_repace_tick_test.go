package sqs

import (
	"context"
	"errors"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestAutoExtendLoop_TickFiredMidHandlerSurvivesRePacing forces the exact
// interleaving that used to make the auto-extend tests fail intermittently.
//
// The loop re-paces itself with ticker.Reset when a failure shortens the retry
// cadence, at the END of the handler. A test driving it advances the clock from
// its own goroutine, so the advance can land while the handler is still inside
// ChangeMessageVisibility: the tick is fired and buffered, and the Reset arrives
// after it. Under a fake clock a discarded tick is never re-delivered — the loop
// goes silent and the test times out waiting for the next failure.
//
// The interleaving is forced rather than raced: the mock blocks inside
// ChangeMessageVisibility, so the second Advance provably precedes the handler's
// clock read and its Reset. Mutation check: restore the channel drain in
// clocktest's fakeTicker.Reset and this test hangs until its wait deadline.
func TestAutoExtendLoop_TickFiredMidHandlerSurvivesRePacing(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{}, 4)
	release := make(chan struct{})

	mock := &mockSQSClient{}
	mock.ChangeMessageVisibilityFn = func(context.Context, *awssqs.ChangeMessageVisibilityInput, ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
		entered <- struct{}{}
		<-release // hold the handler open, before its clock read and Reset
		return nil, errors.New("always fail")
	}

	ctx := context.Background()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "repace", Payload: []byte("x"), CreatedAt: time.Now()})
	fake := clocktest.New()
	d := newDelivery(ctx, env, mock, "https://test-queue", "receipt-repace", 30, true,
		func() {}, nil, nil, fake)
	t.Cleanup(func() { d.stopAutoExtend(); d.cleanupContext() })

	wait.Until(t, 5*time.Second, "auto-extend loop registers its ticker", func() bool {
		return fake.TickerCount() >= 1
	})

	fake.Advance(10 * time.Second) // tick 1 at the vis/3 cadence
	wait.RequireReceive(t, entered, 5*time.Second)
	// The handler is parked inside ChangeMessageVisibility, so it has not yet
	// read the clock or re-paced the ticker.

	fake.Advance(10 * time.Second) // tick 2, fired mid-handler
	close(release)                 // handler completes; the shorter retry re-paces the ticker

	wait.RequireReceive(t, entered, 5*time.Second)
	require.GreaterOrEqual(t, len(visibilitySnapshot(mock)), 2,
		"the tick fired while the handler was running must still be delivered after it re-paces")
}
