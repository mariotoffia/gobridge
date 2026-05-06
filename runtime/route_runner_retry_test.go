package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/runtime"
)

func TestRetryDelay_HonoursRetryAfter(t *testing.T) {
	policy := routing.RoutePolicy{Backoff: routing.BackoffPolicy{
		InitialInterval: time.Second,
		MaxInterval:     30 * time.Second,
		Multiplier:      2.0,
	}}
	sendErr := shared.ErrThrottled.WithRetryAfter(5 * time.Second)

	for _, attempt := range []int{1, 2, 5} {
		if got := runtime.RetryDelay(policy, attempt, sendErr); got != 5*time.Second {
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
		if got := runtime.RetryDelay(policy, attempt+1, sendErr); got != wantDelay {
			t.Fatalf("attempt %d: retryDelay = %v, want %v", attempt+1, got, wantDelay)
		}
	}
}

func TestRetryDelay_ZeroPolicyUsesDefaults(t *testing.T) {
	if got := runtime.RetryDelay(routing.RoutePolicy{}, 1, errors.New("io error")); got <= 0 {
		t.Fatalf("retryDelay = %v, want > 0", got)
	}
}

func TestRouteRunner_DirectHoldTransientSendUsesBackoff(t *testing.T) {
	receiver, sender, _, _, runner := makeRunner(t, func(cfg *runtime.RouteRunnerConfig) {
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

	del := NewFakeDelivery(&messaging.Envelope{ID: "msg-retry-backoff"})
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("emit failed: %v", err)
	}

	waitFor(t, 2*time.Second, "delivery retried with backoff", del.IsRetried)
	if del.RetryAfter < 500*time.Millisecond {
		t.Fatalf("RetryAfter = %v, want >= %v", del.RetryAfter, 500*time.Millisecond)
	}
}
