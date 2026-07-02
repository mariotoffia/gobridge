package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/runtime/route"
)

func TestRetryDelay_HonoursRetryAfter(t *testing.T) {
	policy := routing.RoutePolicy{Backoff: routing.BackoffPolicy{
		InitialInterval: time.Second,
		MaxInterval:     30 * time.Second,
		Multiplier:      2.0,
	}}
	sendErr := shared.ErrThrottled.WithRetryAfter(5 * time.Second)

	for _, attempt := range []int{1, 2, 5} {
		if got := route.RetryDelay(policy, attempt, sendErr); got != 5*time.Second {
			t.Fatalf("attempt %d: retryDelay = %v, want %v", attempt, got, 5*time.Second)
		}
	}
}

func TestRetryDelay_TransientErrorUsesBackoff(t *testing.T) {
	policy := routing.RoutePolicy{Backoff: routing.BackoffPolicy{
		InitialInterval: time.Second,
		MaxInterval:     30 * time.Second,
		Multiplier:      2.0,
	}}
	sendErr := errors.New("conn refused")
	want := []time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}

	for attempt, wantDelay := range want {
		if got := route.RetryDelay(policy, attempt+1, sendErr); got != wantDelay {
			t.Fatalf("attempt %d: retryDelay = %v, want %v", attempt+1, got, wantDelay)
		}
	}
}

func TestRetryDelay_ZeroPolicyUsesDefaults(t *testing.T) {
	if got := route.RetryDelay(routing.RoutePolicy{}, 1, errors.New("io error")); got <= 0 {
		t.Fatalf("retryDelay = %v, want > 0", got)
	}
}

func TestRouteRunner_DirectHoldTransientSendUsesBackoff(t *testing.T) {
	receiver, sender, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.Backoff = routing.BackoffPolicy{
			InitialInterval: 500 * time.Millisecond,
			MaxInterval:     30 * time.Second,
			Multiplier:      2.0,
		}
		cfg.Policy.MaxReplayAttempts = 5
	})
	sender.SendErr = shared.ErrUnavailable

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-retry-backoff"}))
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("emit failed: %v", err)
	}

	waitFor(t, 2*time.Second, "delivery retried with backoff", del.IsRetried)
	if del.RetryAfter < 500*time.Millisecond {
		t.Fatalf("RetryAfter = %v, want >= %v", del.RetryAfter, 500*time.Millisecond)
	}
}

// TestRetryDelay_PublicJitterStaysWithinBounds exercises the public
// RetryDelay (which draws from math/rand/v2) and asserts the default
// equal-jitter keeps every sample within [d*(1-jitter), d] and never
// above MaxInterval — finding #27 via the production entry point.
func TestRetryDelay_PublicJitterStaysWithinBounds(t *testing.T) {
	policy := routing.RoutePolicy{Backoff: routing.BackoffPolicy{
		InitialInterval: time.Second,
		MaxInterval:     30 * time.Second,
		Multiplier:      2.0,
		JitterFactor:    0.2,
	}}
	sendErr := errors.New("transient")
	base := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	for i, d := range base {
		attempt := i + 1
		lo := time.Duration(float64(d) * 0.8)
		for s := 0; s < 200; s++ {
			got := route.RetryDelay(policy, attempt, sendErr)
			if got < lo || got > d {
				t.Fatalf("attempt %d sample %d: got %v, want within [%v, %v]", attempt, s, got, lo, d)
			}
		}
	}
}

// TestRouteRunner_DirectHold_MaxReplay_PerTransportCount proves the
// MaxReplayAttempts boundary is honored per transport for ASB and amqp10
// sources, not only SQS. At the cap a recoverable send error poisons the message
// to the DLQ; one below the cap the same error retries — proving the boundary is
// exact, not off-by-one. Regression for E5 (asb) / E5-AMQP10 — before the fix
// receiveCount() read only the SQS header, returned 0 for these transports, and
// the cap never triggered.
func TestRouteRunner_DirectHold_MaxReplay_PerTransportCount(t *testing.T) {
	cases := []struct {
		name   string
		header string
		value  any
		poison bool // true: count >= cap, expect DLQ; false: count < cap, expect retry
	}{
		// ASB DeliveryCount is 1-based: 3 == 3rd delivery == cap; 2 == below cap.
		{"asb at cap poisons", "asb.delivery-count", 3, true},
		{"asb below cap retries", "asb.delivery-count", 2, false},
		// amqp10 delivery-count is 0-based: raw 2 -> count 3 == cap; raw 1 ->
		// count 2 == below cap.
		{"amqp10 at cap poisons", "amqp10.delivery-count", uint32(2), true},
		{"amqp10 below cap retries", "amqp10.delivery-count", uint32(1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			receiver, sender, dlqStore, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
				cfg.Policy.MaxReplayAttempts = 3
			})
			sender.SendErr = shared.ErrUnavailable // recoverable: retries while under cap

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { _ = runner.Run(ctx) }()

			del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{
				ID:      "msg-" + tc.name,
				Payload: []byte("x"),
				Headers: map[string]any{tc.header: tc.value},
			}))
			if err := receiver.Emit(ctx, del); err != nil {
				t.Fatalf("emit: %v", err)
			}
			if tc.poison {
				waitFor(t, time.Second, "poisoned to DLQ and acked", func() bool {
					return dlqStore.Count() == 1 && del.IsAcked()
				})
				if del.IsRetried() {
					t.Fatal("message should be poisoned to DLQ, not retried, once the source count reached the cap")
				}
			} else {
				waitFor(t, time.Second, "retried while below cap", del.IsRetried)
				if dlqStore.Count() != 0 {
					t.Fatal("message below the cap must not be poisoned to the DLQ")
				}
				if del.IsAcked() {
					t.Fatal("message below the cap must not be acked (still retrying)")
				}
			}
		})
	}
}
