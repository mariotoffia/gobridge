package sqs

// Production-readiness regression tests for shutdown settlement: a send
// that completes while the bridge is shutting down used to fail its
// Ack/Retry because the delivery context was already cancelled —
// DeleteMessage was never issued, guaranteeing duplicate egress on
// restart for every in-flight message. Ack and Retry now settle under a
// bounded context derived with context.WithoutCancel (mirroring the
// runtime's panic-path settlement pattern).

import (
	"context"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// Verifies Ack still deletes the message when the delivery context was
// cancelled before settlement (shutdown racing a completed send): the
// DeleteMessage call must run on a live, deadline-bounded context.
func TestDelivery_Ack_SettlesAfterShutdownCancel(t *testing.T) {
	var ctxErr error
	var hadDeadline bool
	mock := &mockSQSClient{
		DeleteMessageFn: func(ctx context.Context, _ *awssqs.DeleteMessageInput, _ ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
			ctxErr = ctx.Err()
			_, hadDeadline = ctx.Deadline()
			return &awssqs.DeleteMessageOutput{}, nil
		},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e-ack", Payload: []byte("p")})
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, false, nil, nil, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shutdown already cancelled the delivery context

	if err := d.Ack(ctx); err != nil {
		t.Fatalf("Ack after shutdown cancel: %v", err)
	}
	if n := len(mock.DeleteCalls); n != 1 {
		t.Fatalf("DeleteMessage calls = %d, want 1", n)
	}
	if ctxErr != nil {
		t.Fatalf("DeleteMessage ran on a dead context (%v); settlement must strip cancellation", ctxErr)
	}
	if !hadDeadline {
		t.Fatal("settlement context must be bounded by a deadline, not unbounded")
	}
}

// Verifies Retry (nack) reaches SQS under the same bounded settlement
// context when the delivery context is already cancelled.
func TestDelivery_Retry_SettlesAfterShutdownCancel(t *testing.T) {
	var ctxErr error
	var hadDeadline bool
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(ctx context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			ctxErr = ctx.Err()
			_, hadDeadline = ctx.Deadline()
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e-retry", Payload: []byte("p")})
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, false, nil, nil, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := d.Retry(ctx, 0, nil); err != nil {
		t.Fatalf("Retry after shutdown cancel: %v", err)
	}
	if n := len(mock.ChangeVisibilityCalls); n != 1 {
		t.Fatalf("ChangeMessageVisibility calls = %d, want 1", n)
	}
	if ctxErr != nil {
		t.Fatalf("ChangeMessageVisibility ran on a dead context (%v)", ctxErr)
	}
	if !hadDeadline {
		t.Fatal("settlement context must be bounded by a deadline")
	}
}

// Verifies a live delivery context is now bounded by an unconditional
// settlement deadline (Finding 5): the SDK HTTP client has no overall
// request timeout, so a black-holed connection during DeleteMessage would
// otherwise wedge the delivery goroutine for the TCP RTO (tens of
// minutes), holding a MaxInFlight slot. Ack/Retry/Extend/auto-extend all
// impose sqsSettlementTimeout even while ctx is live.
func TestDelivery_Ack_LiveContextIsBounded(t *testing.T) {
	var hadDeadline bool
	mock := &mockSQSClient{
		DeleteMessageFn: func(ctx context.Context, _ *awssqs.DeleteMessageInput, _ ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
			_, hadDeadline = ctx.Deadline()
			return &awssqs.DeleteMessageOutput{}, nil
		},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e-live", Payload: []byte("p")})
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, false, nil, nil, nil, nil)

	// context.Background() has no deadline of its own.
	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if !hadDeadline {
		t.Fatal("a live context must be bounded by an unconditional settlement deadline")
	}
}

// TestDelivery_Retry_LiveContextIsBounded mirrors the Ack case for the
// nack path (Finding 5).
func TestDelivery_Retry_LiveContextIsBounded(t *testing.T) {
	var hadDeadline bool
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(ctx context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			_, hadDeadline = ctx.Deadline()
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e-live-r", Payload: []byte("p")})
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, false, nil, nil, nil, nil)

	if err := d.Retry(context.Background(), 0, nil); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if !hadDeadline {
		t.Fatal("Retry on a live context must be bounded by a settlement deadline")
	}
}

// TestDelivery_Extend_LiveContextIsBounded mirrors the Ack case for the
// explicit Extend path (Finding 5).
func TestDelivery_Extend_LiveContextIsBounded(t *testing.T) {
	var hadDeadline bool
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(ctx context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			_, hadDeadline = ctx.Deadline()
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e-live-e", Payload: []byte("p")})
	// autoExtend disabled so only the explicit Extend issues a CMV.
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, false, nil, nil, nil, nil)

	if err := d.Extend(context.Background(), time.Now().Add(5*time.Second)); err != nil {
		t.Fatalf("Extend: %v", err)
	}
	if !hadDeadline {
		t.Fatal("Extend on a live context must be bounded by a settlement deadline")
	}
}
