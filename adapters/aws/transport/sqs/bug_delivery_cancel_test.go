package sqs

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG-3: SQS processingCancel Race Window
//
// newDelivery() starts autoExtendLoop goroutine immediately (line 69-71),
// but processingCancel is set AFTER newDelivery returns (receiver.go:140).
// If auto-extend fails fast, processingCancel is nil.
// ═══════════════════════════════════════════════════════════════════════════

// TestBug3_Delivery_ProcessingCancelSetAfterConstruction verifies that
// processingCancel is set during newDelivery when passed as a parameter,
// eliminating the race window where the auto-extend goroutine could
// observe a nil processingCancel.
func TestBug3_Delivery_ProcessingCancelSetAfterConstruction(t *testing.T) {
	mock := &mockSQSClient{}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	env := &domain.Envelope{ID: "bug3-test", Payload: []byte("data")}
	del := newDelivery(
		context.Background(),
		env,
		mock,
		"https://q",
		"receipt-handle",
		30,    // visibilityTimeout
		false, // autoExtend=false (no goroutine started)
		cancel,
		nil,
		&ports.NoopExporter{},
		nil,
	)

	if del.processingCancel == nil {
		t.Error("expected processingCancel to be set after newDelivery when passed as parameter")
	} else {
		t.Log("BUG-3 FIX VERIFIED: processingCancel is set during construction")
	}
}

// TestBug3_Delivery_AutoExtendExhaustsCancelsProcessing verifies that
// when auto-extend exhausts its max failures, the processingCancel
// function (now passed at construction time) is called, properly
// cancelling the processing context.
func TestBug3_Delivery_AutoExtendExhaustsCancelsProcessing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: auto-extend test needs ~4s for 3 failure cycles")
	}

	// Atomic because the mock callback is invoked from the auto-extend
	// goroutine while wait.Until predicates read it from the test goroutine.
	var extendCalls atomic.Int32
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			extendCalls.Add(1)
			return nil, fmt.Errorf("simulated extend failure")
		},
	}

	env := &domain.Envelope{ID: "bug3-autoext", Payload: []byte("data")}

	// Create a processing context with cancel — this simulates what
	// the receiver's Run loop does: the cancel func is now passed into
	// newDelivery so it is available before the auto-extend goroutine starts.
	processingCtx, processingCancel := context.WithCancel(context.Background())

	// visibilityTimeout=2 → ticker fires at 1s intervals
	// autoExtend=true → goroutine starts immediately
	// processingCancel is set at construction — the fix.
	fake := clocktest.New()
	del := newDelivery(
		processingCtx,
		env,
		mock,
		"https://q",
		"receipt-handle",
		2,    // visibilityTimeout=2 → 1s ticker
		true, // autoExtend
		processingCancel,
		nil,
		&ports.NoopExporter{},
		fake,
	)

	// processingCancel is set — verify the fix.
	if del.processingCancel == nil {
		t.Fatal("expected processingCancel to be set (fix verified)")
	}

	wait.Until(t, time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	// SYNC: advance to trigger autoExtendMaxFailures (3) failure cycles.
	for i := 0; i < autoExtendMaxFailures; i++ {
		fake.Advance(1 * time.Second)
		want := int32(i + 1)
		wait.Until(t, time.Second, "failure tick", func() bool {
			return extendCalls.Load() >= want
		})
	}

	// The goroutine should have exited after 3 consecutive failures
	// and called processingCancel, cancelling the processing context.
	wait.Until(t, time.Second, "processing context cancelled", func() bool {
		return processingCtx.Err() != nil
	})
	t.Log("BUG-3 FIX VERIFIED: processingCancel was called on extend exhaustion")

	final := extendCalls.Load()
	t.Logf("BUG-3 FIX: auto-extend called ChangeMessageVisibility %d times", final)

	if final < 3 {
		t.Errorf("expected at least 3 extend calls, got %d", final)
	}

	// Clean up: stop auto-extend and release the delivery context.
	del.stopAutoExtend()
	del.cleanupContext()
}
