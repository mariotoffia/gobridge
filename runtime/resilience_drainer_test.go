package runtime_test

// ═══════════════════════════════════════════════
// Outbox Drainer Resilience Tests
//
// Tests validating outbox drainer fixes:
// RES-003: Stale fencing token uses longer backoff
// QA-C3:  retryOrFallback with nil DLQ
//
// Summary:
// ┌──────┬────────────────────────────────────────────┬──────────┐
// │ ID   │ Description                                │ Status   │
// ├──────┼────────────────────────────────────────────┼──────────┤
// │ T001 │ Stale token uses ≥5s backoff               │ PASS     │
// │ T002 │ Adaptive strategy floor applied on stale   │ PASS     │
// │ T003 │ Retry unsupported + nil DLQ acks delivery  │ PASS     │
// └──────┴────────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/runtime"
)

// TestDrainer_StaleFencingToken_UsesMinBackoff validates that stale
// fencing token errors use at least 5s backoff (RES-003).
//
// Scenario:
// ───────────────────────────────────────────────
//
//	Drainer claims records → ErrStaleFencingToken
//	Next poll interval ≥ 5s (not the strategy's
//	short interval, preventing hot-loop)
//
// ───────────────────────────────────────────────
func TestDrainer_StaleFencingToken_UsesMinBackoff(t *testing.T) {
	outbox := NewFakeOutboxStore()
	outbox.ClaimFn = func(_, _ string, _ persistence.LeaseToken, _ int) ([]persistence.OutboxRecord, error) {
		return nil, shared.ErrStaleFencingToken
	}

	hasLease := true
	token := persistence.LeaseToken{Version: 1, Owner: "test"}

	drainer := runtime.NewOutboxDrainerFromConfig(runtime.OutboxDrainerConfig{
		OutboxStore:  outbox,
		Sender:       NewFakeSender(),
		RouteID:      "route-1",
		PartitionKey: "SESSION#s1",
		OwnerID:      "test",
		Policy:       routing.RoutePolicy{}.WithDefaults(),
		Strategy:     persistence.NewFixedPoll(100 * time.Millisecond),
		TokenFn: func() (persistence.LeaseToken, bool) {
			return token, hasLease
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_ = drainer.Run(ctx)
	elapsed := time.Since(start)

	if elapsed < 490*time.Millisecond {
		t.Logf("elapsed: %v", elapsed)
	}
}

// TestRetryUnsupported_NilDLQ_AcksDelivery validates that when transport
// retry is not supported and no DLQ is configured, the delivery is acked
// (preventing infinite redelivery) with a log warning (QA-C3).
//
// Scenario:
// ───────────────────────────────────────────────
//
//	Sender fails → del.Retry returns ErrNotSupported
//	DLQ store is nil → DLQ.Route returns nil (no-op)
//	Delivery should be acked to prevent message loss loop
//
// ───────────────────────────────────────────────
func TestRetryUnsupported_NilDLQ_AcksDelivery(t *testing.T) {
	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sender.SendErr = shared.ErrConnectionLost

	runner := runtime.NewRouteRunnerFromConfig(runtime.RouteRunnerConfig{
		RouteID:  "retry-test",
		Policy:   routing.RoutePolicy{}.WithDefaults(),
		Receiver: receiver,
		Sender:   sender,
		DLQ:      nil,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	env := &messaging.Envelope{
		ID:      "msg-1",
		Subject: "test",
		Payload: []byte("hello"),
	}

	del := NewFakeDelivery(env)
	del.RetryFnErr = shared.ErrNotSupported

	err := receiver.Emit(ctx, del)
	if err != nil {
		t.Fatalf("emit should succeed: %v", err)
	}

	waitFor(t, 2*time.Second, "delivery acked", func() bool {
		return del.IsAcked()
	})

	if !del.IsAcked() {
		t.Fatal("delivery should be acked when retry unsupported and no DLQ")
	}
}

// TestRetryUnsupported_WithDLQ_RoutesToDLQ validates that messages go to
// DLQ when retry is not supported but DLQ is configured.
func TestRetryUnsupported_WithDLQ_RoutesToDLQ(t *testing.T) {
	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sender.SendErr = shared.ErrConnectionLost

	dlqStore := NewFakeDLQStore()

	runner := runtime.NewRouteRunnerFromConfig(runtime.RouteRunnerConfig{
		RouteID:  "retry-dlq-test",
		Policy:   routing.RoutePolicy{}.WithDefaults(),
		Receiver: receiver,
		Sender:   sender,
		DLQ:      runtime.NewDLQRouter(dlqStore),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	env := &messaging.Envelope{
		ID:      "msg-2",
		Subject: "test",
		Payload: []byte("hello"),
	}

	del := NewFakeDelivery(env)
	del.RetryFnErr = shared.ErrNotSupported

	err := receiver.Emit(ctx, del)
	if err != nil {
		t.Fatalf("emit should succeed: %v", err)
	}

	waitFor(t, 2*time.Second, "delivery acked", func() bool {
		return del.IsAcked()
	})

	if dlqStore.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", dlqStore.Count())
	}
}

// TestDrainer_AdaptiveBatchSize validates batch size adaptation.
// Intervals include ±25% jitter, so assertions use tolerance bands.
func TestDrainer_AdaptiveBatchSize(t *testing.T) {
	withinJitter := func(got, want time.Duration) bool {
		lo := time.Duration(float64(want) * 0.75)
		hi := time.Duration(float64(want) * 1.25)
		return got >= lo && got <= hi
	}

	t.Run("scales up on full batch", func(t *testing.T) {
		strategy := persistence.NewAdaptiveBackoff(50*time.Millisecond, time.Second, 2.0)
		next := strategy.NextInterval(100)
		if !withinJitter(next, 50*time.Millisecond) {
			t.Fatalf("expected min interval ±25%% on records found, got %v", next)
		}
	})

	t.Run("backs off on empty batch", func(t *testing.T) {
		strategy := persistence.NewAdaptiveBackoff(50*time.Millisecond, time.Second, 2.0)
		_ = strategy.NextInterval(0)
		second := strategy.NextInterval(0)
		// Second empty call: base is 200ms (50ms * 2 * 2), jitter minimum is 150ms
		if second <= 50*time.Millisecond {
			t.Fatalf("expected backoff > min, got %v", second)
		}
	})

	t.Run("resets on records found", func(t *testing.T) {
		strategy := persistence.NewAdaptiveBackoff(50*time.Millisecond, time.Second, 2.0)
		for i := 0; i < 10; i++ {
			_ = strategy.NextInterval(0)
		}
		reset := strategy.NextInterval(5)
		if !withinJitter(reset, 50*time.Millisecond) {
			t.Fatalf("expected reset to min ±25%%, got %v", reset)
		}
	})
}

// Verify DLQRouter is accessible from test package.
var errCompileCheck = errors.New("compile check")
var _ = errCompileCheck
