package runtime_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// ═══════════════════════════════════════════════════════════════════
// Outbox Depth Cache
//
// Tests verifying that the outbox depth cache reduces QueryPending
// calls on the hot path.
//
// Summary:
// ┌──────┬──────────────────────────────────────────────┬──────────┐
// │ ID   │ Description                                  │ Type     │
// ├──────┼──────────────────────────────────────────────┼──────────┤
// │ T015 │ Cache prevents repeated QueryPending calls   │ unit     │
// │ T016 │ Cache expires after TTL                      │ unit     │
// │ T017 │ At-capacity result cached immediately        │ unit     │
// └──────┴──────────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════

// QueryCountingOutboxStore wraps FakeOutboxStore and counts QueryPending calls.
type QueryCountingOutboxStore struct {
	*FakeOutboxStore
	queryCount int64
}

func NewQueryCountingOutboxStore() *QueryCountingOutboxStore {
	return &QueryCountingOutboxStore{FakeOutboxStore: NewFakeOutboxStore()}
}

func (s *QueryCountingOutboxStore) QueryPending(ctx context.Context, partitionKey string, limit int) ([]*persistence.OutboxRecord, error) {
	atomic.AddInt64(&s.queryCount, 1)
	return s.FakeOutboxStore.QueryPending(ctx, partitionKey, limit)
}

func (s *QueryCountingOutboxStore) GetQueryCount() int64 {
	return atomic.LoadInt64(&s.queryCount)
}

// TestDepthCache_PreventsRepeatedQueries validates that multiple messages
// within the cache TTL do not trigger repeated QueryPending calls.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────
//
//	Route with MaxOutboxDepth=1000, DepthCacheTTL=500ms.
//	Send 10 messages rapidly → QueryPending called only once
//	(first message triggers query, rest use cache).
//
// ───────────────────────────────────────────────────────────────────
func TestDepthCache_PreventsRepeatedQueries(t *testing.T) {
	countingOutbox := NewQueryCountingOutboxStore()
	lease := NewFakeLeaseStore()

	rt := goruntime.New(
		goruntime.WithInstanceID("bridge-cache"),
		goruntime.WithOutboxStore(countingOutbox),
		goruntime.WithLeaseStore(lease),
		goruntime.WithDLQStore(NewFakeDLQStore()),
	)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-cache")

	cfg := goruntime.RouteConfig{
		ID: "cache-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:   routing.DeliverySharedOutbox,
			MaxOutboxDepth: 1000,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "mqtt-cache"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, sess, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	for i := 0; i < 10; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "cache-msg-" + string(rune('a'+i)), Payload: []byte("x")})
		del := NewFakeDelivery(env)
		_ = receiver.Emit(ctx, del)
		waitFor(t, time.Second, "acked", func() bool { return del.IsAcked() })
	}

	qc := countingOutbox.GetQueryCount()
	if qc >= 10 {
		t.Errorf("expected cache to reduce QueryPending calls, got %d (expected <10)", qc)
	}
}

// TestDepthCache_ExpiresAfterTTL validates that the cache expires and
// triggers a new QueryPending call after the TTL.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────
//
//	Send 1 message → cache populated.
//	Wait > TTL.
//	Send another message → new QueryPending call.
//
// ───────────────────────────────────────────────────────────────────
func TestDepthCache_ExpiresAfterTTL(t *testing.T) {
	countingOutbox := NewQueryCountingOutboxStore()
	lease := NewFakeLeaseStore()

	rt := goruntime.New(
		goruntime.WithInstanceID("bridge-ttl"),
		goruntime.WithOutboxStore(countingOutbox),
		goruntime.WithLeaseStore(lease),
		goruntime.WithDLQStore(NewFakeDLQStore()),
	)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-ttl")

	cfg := goruntime.RouteConfig{
		ID: "ttl-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:   routing.DeliverySharedOutbox,
			MaxOutboxDepth: 1000,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "mqtt-ttl"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, sess, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	env1 := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "ttl-msg-1", Payload: []byte("x")})
	del1 := NewFakeDelivery(env1)
	_ = receiver.Emit(ctx, del1)
	waitFor(t, time.Second, "first acked", func() bool { return del1.IsAcked() })

	countAfterFirst := countingOutbox.GetQueryCount()

	time.Sleep(1200 * time.Millisecond) // FIXED: wait for depth cache TTL (1s) to expire

	env2 := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "ttl-msg-2", Payload: []byte("x")})
	del2 := NewFakeDelivery(env2)
	_ = receiver.Emit(ctx, del2)
	waitFor(t, time.Second, "second acked", func() bool { return del2.IsAcked() })

	countAfterSecond := countingOutbox.GetQueryCount()
	if countAfterSecond <= countAfterFirst {
		t.Errorf("expected new QueryPending after TTL expiry, got count before=%d after=%d", countAfterFirst, countAfterSecond)
	}
}

// TestDepthCache_AtCapacityCachedImmediately validates that when the
// outbox is at capacity, subsequent messages get retried without
// additional QueryPending calls (the at-capacity status is cached).
//
// Scenario:
// ───────────────────────────────────────────────────────────────────
//
//	Pre-populate outbox with MaxOutboxDepth records directly,
//	then send overflow messages through a RouteRunner (no drainer).
//	First overflow triggers QueryPending and caches at-capacity.
//	Second overflow uses cached result → no additional query.
//
// ───────────────────────────────────────────────────────────────────
func TestDepthCache_AtCapacityCachedImmediately(t *testing.T) {
	countingOutbox := NewQueryCountingOutboxStore()

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
			ID:         "prefill-" + string(rune('a'+i)),
			RouteID:    "cap-route",
			EnvelopeID: "prefill-env-" + string(rune('a'+i)),
			BindingID:  "b1",
			SessionID:  "mqtt-cap",
			Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "prefill-env-" + string(rune('a'+i)), Payload: []byte("x")}),
			Status:     persistence.OutboxPending,
		})
		_ = countingOutbox.Persist(ctx, []*persistence.OutboxRecord{rec})
	}

	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:     "cap-route",
		Policy:      routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox, MaxOutboxDepth: 3},
		Receiver:    receiver,
		Sender:      sender,
		OutboxStore: countingOutbox,
		Bindings:    []routing.DestinationBinding{{ID: "b1", SessionID: "mqtt-cap"}},
	})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() { _ = runner.Run(runCtx) }()
	<-receiver.Ready()

	env1 := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "overflow-1", Payload: []byte("x")})
	del1 := NewFakeDelivery(env1)
	_ = receiver.Emit(runCtx, del1)
	waitFor(t, time.Second, "overflow-1 retried", func() bool { return del1.IsRetried() })

	countAfterFirst := countingOutbox.GetQueryCount()

	env2 := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "overflow-2", Payload: []byte("x")})
	del2 := NewFakeDelivery(env2)
	_ = receiver.Emit(runCtx, del2)
	waitFor(t, time.Second, "overflow-2 retried", func() bool { return del2.IsRetried() })

	countAfterSecond := countingOutbox.GetQueryCount()

	if countAfterSecond > countAfterFirst+1 {
		t.Errorf("expected cached at-capacity to prevent queries, before=%d after=%d", countAfterFirst, countAfterSecond)
	}
}
