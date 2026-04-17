package sqs

import (
	"context"
	"fmt"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
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

	extendCalls := 0
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			extendCalls++
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
		nil,
	)

	// processingCancel is set — verify the fix.
	if del.processingCancel == nil {
		t.Fatal("expected processingCancel to be set (fix verified)")
	}

	// Wait for autoExtendMaxFailures (3) × ticker interval (1s) + margin.
	time.Sleep(4 * time.Second)

	// The goroutine should have exited after 3 consecutive failures
	// and called processingCancel, cancelling the processing context.
	if processingCtx.Err() == nil {
		t.Error("expected processing context to be cancelled after max extend failures")
	} else {
		t.Log("BUG-3 FIX VERIFIED: processingCancel was called on extend exhaustion")
	}

	t.Logf("BUG-3 FIX: auto-extend called ChangeMessageVisibility %d times", extendCalls)

	if extendCalls < 3 {
		t.Errorf("expected at least 3 extend calls, got %d", extendCalls)
	}

	// Clean up: stop auto-extend and release the delivery context.
	del.stopAutoExtend()
	del.cleanupContext()
}
