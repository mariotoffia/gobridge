package sqs

// Production-readiness regression tests:
//
//   - Started() must reach a terminal state (closed) even when Run
//     fails during initialisation, so a readiness probe selecting only
//     on Started() never hangs.
//   - Visibility-duration conversions clamp in int64 BEFORE narrowing
//     to int32 — a naive int32 conversion of a huge duration is
//     unspecified in Go and can wrap negative, turning a "hold this
//     message for a long time" request into near-immediate redelivery.

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ---------------------------------------------------------------------------
// Started() closes on init failure
// ---------------------------------------------------------------------------

// Verifies Started() is closed when Run fails before the poll loop
// starts (here: queue URL resolution fails), instead of leaving a
// readiness probe hanging forever.
func TestReceiver_StartedClosesOnInitFailure(t *testing.T) {
	mock := &mockSQSClient{
		GetQueueUrlFn: func(_ context.Context, _ *awssqs.GetQueueUrlInput, _ ...func(*awssqs.Options)) (*awssqs.GetQueueUrlOutput, error) {
			return nil, errors.New("AccessDenied: not allowed")
		},
	}
	r, err := NewReceiver(ReceiverConfig{QueueName: "orders", Client: mock}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	runErr := r.Run(context.Background(), func(context.Context, ports.Delivery) error {
		t.Error("emit must not be called when init fails")
		return nil
	})
	if runErr == nil {
		t.Fatal("Run must return the init error")
	}

	// Run has returned, so the deferred signal must already have fired.
	wait.RequireClosed(t, r.Started(), time.Second)
}

// ---------------------------------------------------------------------------
// int64 clamp before int32 narrowing
// ---------------------------------------------------------------------------

// Verifies Retry clamps the delay in int64 seconds: extreme and
// degenerate durations never produce a negative VisibilityTimeout.
func TestDelivery_Retry_DelayClampedInt64(t *testing.T) {
	tests := []struct {
		name  string
		after time.Duration
		want  int32
	}{
		{"max int64 duration clamps to SQS max", time.Duration(math.MaxInt64), 43200},
		{"13h clamps to SQS max", 13 * time.Hour, 43200},
		{"negative delay clamps to zero", -5 * time.Second, 0},
		{"zero delay stays zero", 0, 0},
		{"sub-second positive delay rounds up to 1s", 500 * time.Millisecond, 1},
		{"in-range delay passes through", 90 * time.Second, 90},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockSQSClient{}
			env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e-clamp", Payload: []byte("p")})
			d := newDelivery(context.Background(), env, mock, "q", "rh", 30, false, nil, nil, nil, nil)

			if err := d.Retry(context.Background(), tt.after, nil); err != nil {
				t.Fatalf("Retry: %v", err)
			}
			if n := len(mock.ChangeVisibilityCalls); n != 1 {
				t.Fatalf("ChangeMessageVisibility calls = %d, want 1", n)
			}
			got := mock.ChangeVisibilityCalls[0].VisibilityTimeout
			if got != tt.want {
				t.Fatalf("VisibilityTimeout = %d, want %d", got, tt.want)
			}
			if got < 0 {
				t.Fatalf("VisibilityTimeout = %d is negative: int32 wrap-around leaked through", got)
			}
		})
	}
}

// Verifies Extend clamps a far-future deadline to the SQS max instead of
// wrapping negative in the int32 conversion.
func TestDelivery_Extend_HugeDeadlineClampedToSQSMax(t *testing.T) {
	fake := clocktest.New()
	mock := &mockSQSClient{}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e-extend", Payload: []byte("p")})
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, false, nil, nil, nil, fake)

	// ~114 years out: the int64 second count vastly exceeds MaxInt32.
	until := fake.Now().Add(1_000_000 * time.Hour)
	if err := d.Extend(context.Background(), until); err != nil {
		t.Fatalf("Extend: %v", err)
	}
	if n := len(mock.ChangeVisibilityCalls); n != 1 {
		t.Fatalf("ChangeMessageVisibility calls = %d, want 1", n)
	}
	if got := mock.ChangeVisibilityCalls[0].VisibilityTimeout; got != 43200 {
		t.Fatalf("VisibilityTimeout = %d, want 43200 (SQS max, never negative)", got)
	}
}

// Verifies the shared clamp helper across the int64 domain.
func TestClampVisibilitySeconds_Int64Domain(t *testing.T) {
	tests := []struct {
		name    string
		seconds int64
		want    int32
	}{
		{"max int64", math.MaxInt64, 43200},
		{"just above SQS max", 43201, 43200},
		{"SQS max", 43200, 43200},
		{"in range", 300, 300},
		{"floor", 2, 2},
		{"below floor", 1, 2},
		{"zero", 0, 2},
		{"negative", -100, 2},
		{"min int64", math.MinInt64, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampVisibilitySeconds(tt.seconds); got != tt.want {
				t.Fatalf("clampVisibilitySeconds(%d) = %d, want %d", tt.seconds, got, tt.want)
			}
		})
	}
}
