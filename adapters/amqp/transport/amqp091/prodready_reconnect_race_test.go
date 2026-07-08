// ═══════════════════════════════════════════════
// Production-readiness remediation tests: reconnect-race classification (F3a).
//
// Two permanent-classified AMQP errors are, during a reconnect window,
// transient broker races:
//
//   - 403 ACCESS_REFUSED on an EXCLUSIVE consumer (broker still holds
//     the stale exclusive consumer for ~2x heartbeat after a partition),
//   - 404 NOT_FOUND (consume racing the session's topology reconcile
//     after a broker restart).
//
// Before this fix they killed the component immediately → route
// terminal → pod crash loop on recurrence. Now they are retried with a
// bounded budget (reconnectRaceRetryBudget) and surfaced via Warn +
// MetricAMQP091ReconnectRaceRetried; once the budget is exhausted the
// original permanent error still fails the component so genuine
// misconfiguration is not masked.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestIsReconnectRaceError pins the classification table.
func TestIsReconnectRaceError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		exclusive bool
		want      bool
	}{
		{"nil", nil, true, false},
		{"plain-error", errors.New("boom"), true, false},
		{"404-not-exclusive", shared.ErrNotFound.WithMessage("no queue"), false, true},
		{"404-exclusive", shared.ErrNotFound.WithMessage("no queue"), true, true},
		{"403-exclusive", shared.ErrNotAuthorized.WithMessage("refused"), true, true},
		{"403-not-exclusive", shared.ErrNotAuthorized.WithMessage("refused"), false, false},
		{"406-not-supported", shared.ErrNotSupported.WithMessage("precondition"), true, false},
		{"unavailable", shared.ErrUnavailable.WithMessage("down"), true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isReconnectRaceError(tt.err, tt.exclusive))
		})
	}
}

// runReceiverRaceScenario drives a Receiver whose consume channel open
// always fails with rawErr, using a fake clock to release each bounded
// backoff deterministically, and returns the observed channel-open
// attempts plus the terminal Run error.
func runReceiverRaceScenario(t *testing.T, rawErr error, exclusive bool, metrics ports.MetricsExporter) (int, error) {
	t.Helper()

	mc := newMockConnection()
	mc.ChannelFn = func() (*amqpChannel, error) { return nil, rawErr }
	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	require.NoError(t, sess.Start(context.Background()))
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	fake := clocktest.New()
	r := &Receiver{
		cfg:     ReceiverConfig{QueueName: "q", Exclusive: exclusive},
		session: sess,
		logger:  slog.Default(),
		metrics: metrics,
		clk:     fake,
		started: make(chan struct{}),
		// Un-jittered backoff so a 6s advance always fires the pending timer.
		randFloat: func() float64 { return 0.5 },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, func(context.Context, ports.Delivery) error { return nil }) }()

	// Pump the fake clock: every time the receiver parks on its backoff
	// timer, release it. 6s per advance exceeds the 5s backoff cap, so
	// each advance fires exactly the pending timer. Loop until Run
	// returns with the budget-exhausted terminal error.
	var runErr error
	require.Eventually(t, func() bool {
		select {
		case runErr = <-done:
			return true
		default:
		}
		if fake.TimerCount() >= 1 {
			fake.Advance(6 * time.Second)
		}
		return false
	}, 10*time.Second, time.Millisecond, "Run must terminate once the race-retry budget is exhausted")

	return mc.channelCalls(), runErr
}

// TestReceiver_Run_Reconnect404_BoundedRetry_ThenFails verifies the 404
// reconnect-window race: retried exactly reconnectRaceRetryBudget times
// (metric emitted per retry), then the component fails with the original
// permanent NOT_FOUND error.
func TestReceiver_Run_Reconnect404_BoundedRetry_ThenFails(t *testing.T) {
	rec := &ports.RecordingExporter{}
	rawErr := &amqp.Error{Code: 404, Reason: "NOT_FOUND - no queue 'q'"}

	calls, runErr := runReceiverRaceScenario(t, rawErr, false, rec)

	var be *shared.BridgeError
	require.True(t, errors.As(runErr, &be), "terminal error must be a BridgeError, got %v", runErr)
	require.Equal(t, shared.ErrCodeNotFound, be.Code)

	// Initial attempt + one attempt per budgeted retry.
	require.Equal(t, reconnectRaceRetryBudget+1, calls)
	require.Len(t, rec.FindEntries(MetricAMQP091ReconnectRaceRetried), reconnectRaceRetryBudget)
}

// TestReceiver_Run_Exclusive403_BoundedRetry_ThenFails verifies the
// stale-exclusive-consumer race: 403 on an exclusive consumer is retried
// within the budget instead of instantly killing the component.
func TestReceiver_Run_Exclusive403_BoundedRetry_ThenFails(t *testing.T) {
	rec := &ports.RecordingExporter{}
	rawErr := &amqp.Error{Code: 403, Reason: "ACCESS_REFUSED - queue 'q' in exclusive use"}

	calls, runErr := runReceiverRaceScenario(t, rawErr, true, rec)

	var be *shared.BridgeError
	require.True(t, errors.As(runErr, &be), "terminal error must be a BridgeError, got %v", runErr)
	require.Equal(t, shared.ErrCodeNotAuthorized, be.Code)

	require.Equal(t, reconnectRaceRetryBudget+1, calls)
	require.Len(t, rec.FindEntries(MetricAMQP091ReconnectRaceRetried), reconnectRaceRetryBudget)
}

// TestReceiver_Run_Reconnect404_RecoversWithinBudget verifies the happy
// half of the race fix: when the topology heals before the budget is
// spent (reconcile re-declared the queue), the receiver resumes
// consuming instead of failing the component.
func TestReceiver_Run_Reconnect404_RecoversWithinBudget(t *testing.T) {
	mc := newMockConnection()
	failures := 3
	blockForever := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blockForever) }) }
	defer release()
	mc.ChannelFn = func() (*amqpChannel, error) {
		if mc.channelCalls() <= failures {
			return nil, &amqp.Error{Code: 404, Reason: "NOT_FOUND - no queue 'q'"}
		}
		// "Queue re-declared": there is no way to fake a successful
		// consume without a live broker (amqpChannel wraps *amqp.Channel),
		// so hold the open until the test finishes; reaching this point
		// at all proves the receiver survived the race window.
		<-blockForever
		return nil, context.Canceled
	}

	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	require.NoError(t, sess.Start(context.Background()))
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	fake := clocktest.New()
	rec := &ports.RecordingExporter{}
	r := &Receiver{
		cfg:     ReceiverConfig{QueueName: "q"},
		session: sess,
		logger:  slog.Default(),
		metrics: rec,
		clk:     fake,
		started: make(chan struct{}),
		// Un-jittered backoff so a 6s advance always fires the pending timer.
		randFloat: func() float64 { return 0.5 },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, func(context.Context, ports.Delivery) error { return nil }) }()

	require.Eventually(t, func() bool {
		select {
		case err := <-done:
			t.Fatalf("Run terminated during the race window: %v", err)
		default:
		}
		if fake.TimerCount() >= 1 {
			fake.Advance(6 * time.Second)
		}
		return mc.channelCalls() > failures
	}, 10*time.Second, time.Millisecond, "receiver must retry past the race window and attempt a fresh consume")

	require.Len(t, rec.FindEntries(MetricAMQP091ReconnectRaceRetried), failures)

	cancel()
	release() // unblock the held channel-open so Run can observe the cancel
	err := wait.RequireReceive(t, done, 2*time.Second)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
}
