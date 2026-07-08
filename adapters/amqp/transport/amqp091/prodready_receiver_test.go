// ═══════════════════════════════════════════════
// Production-readiness remediation tests: receiver reconnect loop.
//
// Covers Finding #4 — the receiver no longer hot-spins when consumeLoop
// fails while the session stays connected, and it fails the component on
// permanent transport errors instead of retrying them forever.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestIsPermanentError verifies the classification that decides whether
// the receiver fails the component or retries.
func TestIsPermanentError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"not-found", shared.ErrNotFound.WithMessage("missing queue"), true},
		{"not-authorized", shared.ErrNotAuthorized.WithMessage("nope"), true},
		{"not-supported", shared.ErrNotSupported.WithMessage("nope"), true},
		{"protocol", shared.ErrProtocolError.WithMessage("bad frame"), true},
		{"transient-unavailable", shared.ErrUnavailable.WithMessage("retry"), false},
		{"transient-conn-lost", shared.ErrConnectionLost.WithMessage("drop"), false},
		{"plain-error", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPermanentError(tt.err); got != tt.want {
				t.Fatalf("isPermanentError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestReceiverBackoff verifies the bounded exponential schedule.
func TestReceiverBackoff(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{0, receiverRetryInitial},
		{1, receiverRetryInitial},
		{2, 200 * time.Millisecond},
		{3, 400 * time.Millisecond},
		{4, 800 * time.Millisecond},
		{100, receiverRetryMax}, // capped, no overflow
	}
	for _, tt := range tests {
		if got := receiverBackoff(tt.failures); got != tt.want {
			t.Errorf("receiverBackoff(%d) = %s, want %s", tt.failures, got, tt.want)
		}
	}
}

// TestReceiver_Run_PermanentError_FailsComponent verifies that a
// genuinely permanent transport error causes Run to return that error
// WITHOUT retrying — no hot loop, no infinite reconnect.
//
// The probe error is a 403 ACCESS_REFUSED on a NON-exclusive consumer:
// unlike 404 (queue re-declare window after a reconnect) and
// 403-with-exclusive (stale exclusive consumer held for ~2x heartbeat),
// it is never a reconnect race (see isReconnectRaceError) and must fail
// the component on the first occurrence.
func TestReceiver_Run_PermanentError_FailsComponent(t *testing.T) {
	mc := newMockConnection()
	mc.ChannelFn = func() (*amqpChannel, error) {
		return nil, &amqp.Error{Code: 403, Reason: "ACCESS_REFUSED - permission denied"}
	}
	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()

	r := &Receiver{
		cfg:     ReceiverConfig{QueueName: "q"}, // Exclusive: false
		session: sess,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
		started: make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, func(context.Context, ports.Delivery) error { return nil }) }()

	err := wait.RequireReceive(t, done, 2*time.Second)
	var be *shared.BridgeError
	if !errors.As(err, &be) || be.Code != shared.ErrCodeNotAuthorized {
		t.Fatalf("Run returned %v, want ErrNotAuthorized (permanent)", err)
	}
	if calls := mc.channelCalls(); calls != 1 {
		t.Fatalf("permanent error must not be retried: channelCalls = %d, want 1", calls)
	}
}

// TestReceiver_Run_TransientWhileConnected_BacksOff_NoHotLoop verifies
// the core Finding #4 fix: when consumeLoop fails fast but the session
// stays connected (so waitForReconnect returns immediately), the receiver
// parks on a bounded backoff timer instead of hot-spinning. Determinism
// comes from a fake clock: no further consume attempt happens until the
// test advances time.
func TestReceiver_Run_TransientWhileConnected_BacksOff_NoHotLoop(t *testing.T) {
	mc := newMockConnection()
	mc.ChannelFn = func() (*amqpChannel, error) {
		// Transient failure -> MapError -> ErrUnavailable (not permanent).
		return nil, errors.New("temporary channel open failure")
	}
	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()

	fake := clocktest.New()
	r := &Receiver{
		cfg:     ReceiverConfig{QueueName: "q"},
		session: sess,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
		clk:     fake,
		started: make(chan struct{}),
		// Un-jittered backoff so Advance(receiverRetryInitial) fires the
		// pending timer exactly (jitter would make the deadline 0.75-1.25x).
		randFloat: func() float64 { return 0.5 },
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, func(context.Context, ports.Delivery) error { return nil }) }()

	// After the first fast failure the receiver must be parked on its
	// backoff timer (not looping). TimerCount==1 proves it is waiting.
	wait.Until(t, 2*time.Second, "receiver parked on backoff timer", func() bool {
		return fake.TimerCount() == 1
	})
	if calls := mc.channelCalls(); calls != 1 {
		t.Fatalf("hot loop detected: channelCalls = %d after one failure, want 1", calls)
	}

	// Releasing the backoff yields exactly one more attempt, which fails
	// again and re-parks — demonstrating bounded, clock-driven retries.
	fake.Advance(receiverRetryInitial)
	wait.Until(t, 2*time.Second, "second attempt after backoff", func() bool {
		return mc.channelCalls() >= 2
	})
	wait.Until(t, 2*time.Second, "receiver re-parked on backoff timer", func() bool {
		return fake.TimerCount() == 1
	})

	cancel()
	err := wait.RequireReceive(t, done, 2*time.Second)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
}
