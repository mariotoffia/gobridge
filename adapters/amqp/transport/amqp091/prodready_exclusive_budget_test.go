// ═══════════════════════════════════════════════
// Production-readiness remediation tests: heartbeat-aware exclusive-consumer
// failover budget (c5-exclusive-failover).
//
// A stale exclusive consumer is held by the broker until missed heartbeats
// (~2x the interval) reap the partitioned peer's connection. The old fixed
// ~10-retry (~25-30s) budget suits the 10s default heartbeat but is too short
// once the heartbeat is raised: with heartbeat:30s the stale consumer lingers
// ~60s and the standby exhausts the budget and fails the component before it
// can take over. The budget is now DERIVED from the heartbeat for the
// stale-exclusive (403) race, floored at the fixed default so shorter
// heartbeats keep today's behaviour.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestExclusiveRaceRetryBudget pins the heartbeat→budget arithmetic, the
// floor, and the upper cap. Mutation: return the fixed reconnectRaceRetryBudget
// unconditionally and the scaled rows (30s/60s) fail; drop the cap clamp and
// the large-heartbeat rows (300s/600s) fail.
func TestExclusiveRaceRetryBudget(t *testing.T) {
	tests := []struct {
		name      string
		heartbeat time.Duration
		want      int
	}{
		{"zero-uses-default", 0, reconnectRaceRetryBudget},
		{"negative-uses-default", -time.Second, reconnectRaceRetryBudget},
		{"1s-floored", time.Second, reconnectRaceRetryBudget},
		{"10s-default-floored", 10 * time.Second, reconnectRaceRetryBudget}, // 3*10/5=6 < 10
		{"16s-floored", 16 * time.Second, reconnectRaceRetryBudget},         // 3*16/5=9 < 10
		{"17s-at-floor", 17 * time.Second, reconnectRaceRetryBudget},        // 3*17/5=10.2→10
		{"30s-scaled", 30 * time.Second, 18},                                // 3*30/5=18
		{"60s-scaled", 60 * time.Second, 36},                                // 3*60/5=36
		{"80s-at-cap", 80 * time.Second, exclusiveRaceRetryMaxBudget},       // 3*80/5=48==cap
		{"90s-capped", 90 * time.Second, exclusiveRaceRetryMaxBudget},       // 3*90/5=54→48
		{"300s-capped", 300 * time.Second, exclusiveRaceRetryMaxBudget},     // 3*300/5=180→48
		{"600s-capped", 600 * time.Second, exclusiveRaceRetryMaxBudget},     // 3*600/5=360→48
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, exclusiveRaceRetryBudget(tt.heartbeat))
			require.GreaterOrEqual(t, exclusiveRaceRetryBudget(tt.heartbeat), reconnectRaceRetryBudget,
				"budget must never drop below the fixed default")
			require.LessOrEqual(t, exclusiveRaceRetryBudget(tt.heartbeat), exclusiveRaceRetryMaxBudget,
				"budget must never exceed the upper cap so a genuine 403 is not masked indefinitely")
		})
	}
}

// TestExclusiveStaleConsumerRace pins which errors widen the budget: only a
// 403 ACCESS_REFUSED on an EXCLUSIVE consumer. A 404 (topology re-declare)
// keeps the fixed budget; a non-exclusive 403 is a real permission error.
func TestExclusiveStaleConsumerRace(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		exclusive bool
		want      bool
	}{
		{"403-exclusive", shared.ErrNotAuthorized.WithMessage("refused"), true, true},
		{"403-not-exclusive", shared.ErrNotAuthorized.WithMessage("refused"), false, false},
		{"404-exclusive", shared.ErrNotFound.WithMessage("no queue"), true, false},
		{"nil", nil, true, false},
		{"plain-error", errors.New("boom"), true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, exclusiveStaleConsumerRace(tt.err, tt.exclusive))
		})
	}
}

// runExclusiveRaceScenario drives an exclusive Receiver whose consume-channel
// open always fails with a 403, on a session with the given heartbeat, and
// returns the channel-open attempts plus the terminal Run error. Mirrors
// runReceiverRaceScenario but exercises the heartbeat-aware budget.
func runExclusiveRaceScenario(t *testing.T, heartbeat time.Duration, metrics ports.MetricsExporter) (int, error) {
	t.Helper()

	rawErr := &amqp.Error{Code: 403, Reason: "ACCESS_REFUSED - queue 'q' in exclusive use"}
	mc := newMockConnection()
	mc.ChannelFn = func() (*amqpChannel, error) { return nil, rawErr }
	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	// Set the heartbeat BEFORE Start so the session's goroutines never race the
	// write; the receiver reads it via r.heartbeat() to size the budget.
	sess.opts.Heartbeat = heartbeat
	require.NoError(t, sess.Start(context.Background()))
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	fake := clocktest.New()
	r := &Receiver{
		cfg:       ReceiverConfig{QueueName: "q", Exclusive: true},
		session:   sess,
		logger:    slog.Default(),
		metrics:   metrics,
		clk:       fake,
		started:   make(chan struct{}),
		randFloat: func() float64 { return 0.5 },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, func(context.Context, ports.Delivery) error { return nil }) }()

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
	}, 10*time.Second, time.Millisecond, "Run must terminate once the heartbeat-aware budget is exhausted")

	return mc.channelCalls(), runErr
}

// TestReceiver_Run_Exclusive403_HeartbeatAwareBudget proves the standby retries
// the stale-exclusive 403 for the HEARTBEAT-DERIVED budget (18 at heartbeat:30s)
// rather than the fixed 10. Mutation: pin the budget to reconnectRaceRetryBudget
// and the receiver fails after 10 retries (calls=11) → this fails at calls=19.
func TestReceiver_Run_Exclusive403_HeartbeatAwareBudget(t *testing.T) {
	rec := &ports.RecordingExporter{}

	calls, runErr := runExclusiveRaceScenario(t, 30*time.Second, rec)

	var be *shared.BridgeError
	require.True(t, errors.As(runErr, &be), "terminal error must be a BridgeError, got %v", runErr)
	require.Equal(t, shared.ErrCodeNotAuthorized, be.Code)

	wantBudget := exclusiveRaceRetryBudget(30 * time.Second)
	require.Equal(t, 18, wantBudget, "heartbeat:30s must yield an 18-retry budget")
	require.Equal(t, wantBudget+1, calls, "initial attempt + one per budgeted retry")
	require.Len(t, rec.FindEntries(MetricAMQP091ReconnectRaceRetried), wantBudget)
}

// TestReceiver_Heartbeat_NilSession returns 0 so the budget falls back to the
// fixed default when no session is bound.
func TestReceiver_Heartbeat_NilSession(t *testing.T) {
	r := &Receiver{}
	require.Zero(t, r.heartbeat())
	require.Equal(t, reconnectRaceRetryBudget, exclusiveRaceRetryBudget(r.heartbeat()))
}
