package sqs

import (
	"context"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ═══════════════════════════════════════════════════════════════════════════
// GAP-10: SQS Delivery Auto-Extend Boundary Conditions
//
// Auto-extend is skipped when visibilityTimeout <= 1. Test boundaries:
// - visibilityTimeout == 1: auto-extend should NOT start
// - visibilityTimeout == 2: auto-extend SHOULD start at 1s interval
// ═══════════════════════════════════════════════════════════════════════════

// TestAutoExtend_Boundary_Timeout1_Disabled verifies that auto-extend does
// not start when visibilityTimeout == 1 (interval would be 0.5s, guard is > 1).
func TestAutoExtend_Boundary_Timeout1_Disabled(t *testing.T) {
	mock := &mockSQSClient{}

	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "boundary-1", Subject: "test"})
	fake := clocktest.New()
	del := newDelivery(
		parentCtx,
		env,
		mock,
		"https://sqs.us-west-1.amazonaws.com/q",
		"handle-1",
		1, // visibilityTimeout = 1 → no auto-extend
		true,
		func() {}, // processingCancel
		nil,
		&ports.NoopExporter{},
		fake,
	)

	// NEGATIVE, asserted on the clock rather than on elapsed wall time: the
	// auto-extend loop's first act is to register a ticker, so "no ticker was
	// ever registered" is the deterministic proof it never started. Advancing
	// the fake past several would-be intervals leaves nothing to fire.
	fake.Advance(10 * time.Second)

	assert.Equal(t, 0, fake.TickerCount(),
		"auto-extend should NOT register a ticker when visibilityTimeout == 1")

	// Measured BEFORE Ack: Ack issues a single final pre-delete
	// visibility-margin extension for such a small window
	// (Finding: c8-autoextend-margin), which is unrelated to whether the
	// background auto-extend loop started.
	mock.mu.Lock()
	callsBeforeAck := len(mock.ChangeVisibilityCalls)
	mock.mu.Unlock()

	assert.Equal(t, 0, callsBeforeAck,
		"auto-extend should NOT start when visibilityTimeout == 1")

	_ = del.Ack(context.Background())
}

// TestAutoExtend_Boundary_Timeout2_Enabled verifies that auto-extend DOES
// start when visibilityTimeout == 2 (interval = 1s, fires at least once).
// The 1s interval is driven by a clocktest.Fake, so this completes in
// microseconds and runs on every `make test` — it is a regression guard,
// not an integration test.
func TestAutoExtend_Boundary_Timeout2_Enabled(t *testing.T) {
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(
			_ context.Context,
			_ *awssqs.ChangeMessageVisibilityInput,
			_ ...func(*awssqs.Options),
		) (*awssqs.ChangeMessageVisibilityOutput, error) {
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}

	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "boundary-2", Subject: "test"})
	fake := clocktest.New()
	del := newDelivery(
		parentCtx,
		env,
		mock,
		"https://sqs.us-west-1.amazonaws.com/q",
		"handle-2",
		2, // visibilityTimeout = 2 → auto-extend at 1s interval
		true,
		func() {}, // processingCancel
		nil,
		&ports.NoopExporter{},
		fake,
	)

	wait.Until(t, time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	// SYNC: advance fake clock to trigger auto-extend tick.
	fake.Advance(1 * time.Second)
	wait.Until(t, time.Second, "auto-extend fired", func() bool {
		mock.mu.Lock()
		n := len(mock.ChangeVisibilityCalls)
		mock.mu.Unlock()
		return n >= 1
	})

	_ = del.Ack(context.Background())

	mock.mu.Lock()
	calls := len(mock.ChangeVisibilityCalls)
	mock.mu.Unlock()

	assert.GreaterOrEqual(t, calls, 1,
		"auto-extend should fire at least once when visibilityTimeout == 2")
}
