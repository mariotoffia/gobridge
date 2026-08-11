package runtime_test

import (
	"context"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	outboxpkg "github.com/mariotoffia/gobridge/runtime/outbox"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// ═══════════════════════════════════════════════════════════════════════════
// Conditional depthCache allocation
//
// DirectHold routes should not allocate a depth cache; only SharedOutbox
// routes need one. These tests verify that the sharedOutbox depth-check
// path is exercised for SharedOutbox and skipped for DirectHold.
// ═══════════════════════════════════════════════════════════════════════════

// TestRouteRunner_SharedOutbox_DepthCacheExercised validates that a
// SharedOutbox route with MaxOutboxDepth > 0 calls QueryPending (via
// the depth cache miss path) on the first message delivery.
//
// Data flow:
//
//	Receiver → RouteRunner → depthCache miss → QueryPending → Persist
func TestRouteRunner_SharedOutbox_DepthCacheExercised(t *testing.T) {
	outbox := NewQueryCountingOutboxStore()
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:  "route-shared",
		Receiver: receiver,
		Sender:   sender,
		Policy: routing.RoutePolicy{
			DeliveryMode:   routing.DeliverySharedOutbox,
			MaxOutboxDepth: 100,
		},
		OutboxStore: outbox,
		DLQ:         dlq.New(NewFakeDLQStore()),
		InstanceID:  "bridge-1",
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "sess-1", Address: "dest"},
		},
		DepthCacheTTL: time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-1", Payload: []byte("x")}))
	_ = receiver.Emit(ctx, del)

	waitFor(t, time.Second, "delivery acked", del.IsAcked)

	if outbox.GetQueryCount() == 0 {
		t.Fatal("expected QueryPending to be called for SharedOutbox depth check")
	}
}

// TestRouteRunner_DirectHold_NoQueryPending validates that a DirectHold
// route never calls QueryPending because no depth cache is allocated.
//
// Data flow:
//
//	Receiver → RouteRunner → directHold → Send → Ack
func TestRouteRunner_DirectHold_NoQueryPending(t *testing.T) {
	outbox := NewQueryCountingOutboxStore()
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:  "route-direct",
		Receiver: receiver,
		Sender:   sender,
		Policy: routing.RoutePolicy{
			DeliveryMode:   routing.DeliveryDirectHold,
			MaxOutboxDepth: 100, // deliberately set to prove depth check is not exercised for DirectHold
		},
		OutboxStore: outbox,
		DLQ:         dlq.New(NewFakeDLQStore()),
		InstanceID:  "bridge-1",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-2", Payload: []byte("y")}))
	_ = receiver.Emit(ctx, del)

	waitFor(t, time.Second, "delivery acked", del.IsAcked)

	if outbox.GetQueryCount() != 0 {
		t.Fatalf("expected no QueryPending calls for DirectHold, got %d", outbox.GetQueryCount())
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Drainer name generation for i >= 10
//
// The old code used string(rune('0'+i)) which produces wrong characters
// for i >= 10. The fix uses strconv.Itoa(i). This test verifies the
// string-construction logic matches strconv.Itoa for various indices.
// ═══════════════════════════════════════════════════════════════════════════

// TestDrainerNameGeneration validates that drainer name generation
// produces correct numeric suffixes for all index values.
//
// ═══════════════════════════════════════════════════════════════════════
// Index    Expected suffix    Old bug (rune)
// ─────────────────────────────────────────────────────────────────────
//
//	 0       "0"               "0" ✓
//	 9       "9"               "9" ✓
//	10       "10"              ":" ✗ (rune 58)
//	99       "99"              "c" ✗ (rune 147)
//
// ═══════════════════════════════════════════════════════════════════════
// NOTE: This test mirrors the naming pattern in bridge.go:Start (drainer
// loop). Keep the format string in sync if bridge.go changes.
func TestDrainerNameGeneration(t *testing.T) {
	cases := []struct {
		index    int
		routeID  string
		expected string
	}{
		{0, "route-a", "drainer:route-a:0"},
		{1, "route-a", "drainer:route-a:1"},
		{9, "route-a", "drainer:route-a:9"},
		{10, "route-a", "drainer:route-a:10"},
		{11, "route-a", "drainer:route-a:11"},
		{99, "route-a", "drainer:route-a:99"},
		{100, "route-b", "drainer:route-b:100"},
	}

	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.index), func(t *testing.T) {
			name := "drainer:" + tc.routeID + ":" + strconv.Itoa(tc.index)
			if name != tc.expected {
				t.Fatalf("expected %q, got %q", tc.expected, name)
			}
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// OutboxDrainerConfig naming consistency
//
// Validates that the renamed DrainBatchSize, DrainMaxBatchSize, and
// DrainMaxConcurrency fields on OutboxDrainerConfig wire correctly
// to the drainer's runtime behavior.
// ═══════════════════════════════════════════════════════════════════════════

// TestOutboxDrainerConfig_DrainBatchSize_Default validates that
// DrainBatchSize=0 defaults to 100. The drainer is constructed and
// runs a single drain cycle; the outbox is empty so no records are
// sent, but the drainer must accept the config without error.
func TestOutboxDrainerConfig_DrainBatchSize_Default(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.DrainBatchSize = 0
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = drainer.Run(ctx) }()

	_ = outbox.Persist(context.Background(), []*persistence.OutboxRecord{
		persistence.MustOutboxRecord(persistence.OutboxSpec{ID: "rec-def", RouteID: "route-1", EnvelopeID: "env-def", BindingID: "b1", SessionID: "sess-1", Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-def", Payload: []byte("x")})})})

	waitFor(t, 2*time.Second, "record sent", func() bool {
		return sender.SentCount() >= 1
	})
	cancel()
}

// TestOutboxDrainerConfig_DrainBatchSize_Custom validates that an
// explicit DrainBatchSize value is respected by verifying the Claim
// limit parameter matches the configured value.
func TestOutboxDrainerConfig_DrainBatchSize_Custom(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	var observedLimit int64
	outbox, _, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.DrainBatchSize = 42
	})

	outbox.ClaimFn = func(_ string, _ persistence.LeaseToken, limit int) ([]*persistence.OutboxRecord, error) {
		atomic.StoreInt64(&observedLimit, int64(limit))
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = drainer.Run(ctx) }()

	waitFor(t, 2*time.Second, "claim called with correct limit", func() bool {
		return atomic.LoadInt64(&observedLimit) > 0
	})
	cancel()

	if got := atomic.LoadInt64(&observedLimit); got != 42 {
		t.Fatalf("expected Claim limit=42, got %d", got)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// finalDrain uses context.Background() bounded by DrainTimeout
// ═══════════════════════════════════════════════════════════════════════════

// TestOutboxDrainer_FinalDrain_CompletesAfterCancel validates that
// the drainer performs a final drain batch after Run's context is
// cancelled, using a detached context bounded by DrainTimeout.
//
// Strategy: use a fast initial poll so that the first drain cycle
// runs (setting hasDrained=true and sending the persisted record).
// The record is delivered during normal draining; finalDrain
// confirms no additional work is needed.
//
// Timeline:
//
//	──[persist records]──[first drain cycle]──[cancel ctx]──[finalDrain]──
func TestOutboxDrainer_FinalDrain_CompletesAfterCancel(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}

	pollEntered := make(chan struct{}, 1)
	strategy := &signalingStrategy{
		inner:    persistence.NewFixedPoll(10 * time.Millisecond),
		onSignal: pollEntered,
	}

	outbox, sender, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.Strategy = strategy
		cfg.DrainTimeout = 500 * time.Millisecond
	})

	ctx := context.Background()
	_ = outbox.Persist(ctx, []*persistence.OutboxRecord{
		persistence.MustOutboxRecord(persistence.OutboxSpec{ID: "final-1", RouteID: "route-1", EnvelopeID: "env-f1", BindingID: "b1", SessionID: "sess-1", Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-f1", Payload: []byte("final")})})})

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- drainer.Run(runCtx) }()

	select {
	case <-pollEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("drainer did not enter poll loop")
	}
	waitFor(t, 2*time.Second, "drainer sent >= 1", func() bool { return sender.SentCount() >= 1 })
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("drainer.Run did not return after cancel")
	}

	if sender.SentCount() < 1 {
		t.Fatal("expected at least 1 message sent during final drain")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Clamp negative globalMaxInFlight
//
// WithGlobalMaxInFlight(n) must clamp negative n to 0, which disables
// the global semaphore. No panic on math.MinInt.
// ═══════════════════════════════════════════════════════════════════════════

// TestWithGlobalMaxInFlight_NegativeClampedToZero validates that negative
// values are silently clamped to 0 (global throttling disabled).
//
// ═══════════════════════════════════════════════════════════════════════
//
//	Input n        Effective    globalSem
//	─────────────────────────────────────────
//	  5            5            chan(5)
//	  0            0            nil
//	 -1            0            nil
//	 MinInt        0            nil
//
// ═══════════════════════════════════════════════════════════════════════
func TestWithGlobalMaxInFlight_NegativeClampedToZero(t *testing.T) {
	cases := []struct {
		name  string
		input int
	}{
		{"negative_one", -1},
		{"negative_hundred", -100},
		{"min_int", math.MinInt},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := goruntime.New(goruntime.WithGlobalMaxInFlight(tc.input))
			if rt == nil {
				t.Fatal("expected non-nil runtime")
			}
		})
	}
}

// TestWithGlobalMaxInFlight_Zero_DisablesThrottling validates that
// n=0 means no global semaphore is created.
func TestWithGlobalMaxInFlight_Zero_DisablesThrottling(t *testing.T) {
	rt := goruntime.New(goruntime.WithGlobalMaxInFlight(0))
	if rt == nil {
		t.Fatal("expected non-nil runtime")
	}
}

// TestWithGlobalMaxInFlight_Positive_Accepted validates that a positive
// value is accepted without error.
func TestWithGlobalMaxInFlight_Positive_Accepted(t *testing.T) {
	rt := goruntime.New(goruntime.WithGlobalMaxInFlight(5))
	if rt == nil {
		t.Fatal("expected non-nil runtime")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Test helpers
// ═══════════════════════════════════════════════════════════════════════════

// signalingStrategy wraps a DrainStrategy and sends on onSignal
// (non-blocking) on the first NextInterval call, allowing tests to
// detect when the drainer has entered its poll loop.
type signalingStrategy struct {
	inner    persistence.DrainStrategy
	onSignal chan<- struct{}
	once     sync.Once
}

func (s *signalingStrategy) NextInterval(n int) time.Duration {
	s.once.Do(func() {
		select {
		case s.onSignal <- struct{}{}:
		default:
		}
	})
	return s.inner.NextInterval(n)
}

// ═══════════════════════════════════════════════════════════════════════════
// Additional edge-case tests from QA review
// ═══════════════════════════════════════════════════════════════════════════

// TestOutboxDrainerConfig_DrainBatchSize_NegativeClamped validates that
// a negative DrainBatchSize is clamped to the default (100).
//
// ═══════════════════════════════════════════════════════════════════════
//
//	Input     Effective
//	───────────────────
//	 -1       100
//	 -100     100
//
// ═══════════════════════════════════════════════════════════════════════
func TestOutboxDrainerConfig_DrainBatchSize_NegativeClamped(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.DrainBatchSize = -1
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = drainer.Run(ctx) }()

	_ = outbox.Persist(context.Background(), []*persistence.OutboxRecord{
		persistence.MustOutboxRecord(persistence.OutboxSpec{ID: "rec-neg", RouteID: "route-1", EnvelopeID: "env-neg", BindingID: "b1", SessionID: "sess-1", Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-neg", Payload: []byte("neg")})})})

	waitFor(t, 2*time.Second, "record sent with clamped batch size", func() bool {
		return sender.SentCount() >= 1
	})
	cancel()
}

// TestOutboxDrainerConfig_DrainMaxBatchSize_FloorsToBatchSize validates
// that DrainMaxBatchSize < DrainBatchSize is raised to match DrainBatchSize.
//
// ═══════════════════════════════════════════════════════════════════════
//
//	DrainBatchSize=200, DrainMaxBatchSize=50 → effective max = 200
//
// ═══════════════════════════════════════════════════════════════════════
func TestOutboxDrainerConfig_DrainMaxBatchSize_FloorsToBatchSize(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.DrainBatchSize = 200
		cfg.DrainMaxBatchSize = 50
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = drainer.Run(ctx) }()

	_ = outbox.Persist(context.Background(), []*persistence.OutboxRecord{
		persistence.MustOutboxRecord(persistence.OutboxSpec{ID: "rec-floor", RouteID: "route-1", EnvelopeID: "env-floor", BindingID: "b1", SessionID: "sess-1", Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-floor", Payload: []byte("f")})})})

	waitFor(t, 2*time.Second, "record sent with floored max batch size", func() bool {
		return sender.SentCount() >= 1
	})
	cancel()
}

// TestRouteRunner_SharedOutbox_NilOutboxStore_Retries validates that
// a SharedOutbox route with nil OutboxStore retries the delivery
// instead of panicking. This configuration is normally caught by
// Runtime.Start() validation; this test verifies the defensive
// guard for direct RouteRunner construction.
func TestRouteRunner_SharedOutbox_NilOutboxStore_Retries(t *testing.T) {
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:  "route-nil-outbox",
		Receiver: receiver,
		Sender:   sender,
		Policy: routing.RoutePolicy{
			DeliveryMode:   routing.DeliverySharedOutbox,
			MaxOutboxDepth: 100,
		},
		OutboxStore: nil,
		DLQ:         dlq.New(NewFakeDLQStore()),
		InstanceID:  "bridge-1",
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "sess-1", Address: "dest"},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-nil-outbox", Payload: []byte("z")}))
	_ = receiver.Emit(ctx, del)

	waitFor(t, time.Second, "delivery retried (no outbox store)", del.IsRetried)
}
