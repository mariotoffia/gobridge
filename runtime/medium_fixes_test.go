package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// ═══════════════════════════════════════════════════════════════════════════
// Medium Fixes Part 1: T9–T14
//
// Tests for depth cache eviction (T9), DepthCacheTTL wiring (T10),
// drain config wiring (T11), fail-closed depth check (T12),
// batch size clamping (T13), and record failure metrics (T14).
// ═══════════════════════════════════════════════════════════════════════════

// ---------------------------------------------------------------------------
// T9: Depth cache eviction clears map on burst overflow
// ---------------------------------------------------------------------------

// varyingResolver returns a different BindingID (and thus partition key)
// for each envelope, generating distinct cache entries to exercise eviction.
type varyingResolver struct {
	counter int32
}

func (r *varyingResolver) Resolve(_ context.Context, _ *messaging.Envelope) ([]domain.DispatchPlan, error) {
	n := atomic.AddInt32(&r.counter, 1)
	return []domain.DispatchPlan{{
		BindingID: fmt.Sprintf("bind-%d", n),
		Address:   "topic/test",
	}}, nil
}

// TestDepthCache_EvictionClearsOnBurst validates that when more than
// depthCacheMaxEntries (1000) distinct partition keys arrive within the
// eviction window, the cache clears itself to prevent unbounded growth.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────
//
//	Use a resolver that returns a unique BindingID per message.
//	Each unique BindingID produces a unique partition key in the cache.
//	After 1001 distinct keys, time-based eviction can't help (TTL=1m),
//	so the cache clears the map entirely, keeping only the latest entry.
//
// ───────────────────────────────────────────────────────────────────────
func TestDepthCache_EvictionClearsOnBurst(t *testing.T) {
	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	countingOutbox := NewQueryCountingOutboxStore()
	resolver := &varyingResolver{}

	runner := goruntime.NewRouteRunnerFromConfig(goruntime.RouteRunnerConfig{
		RouteID:       "burst-route",
		Policy:        domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox, MaxOutboxDepth: 100000},
		Receiver:      receiver,
		Sender:        sender,
		OutboxStore:   countingOutbox,
		Resolver:      resolver,
		Bindings:      []domain.DestinationBinding{{ID: "b1", SessionID: "burst-sess"}},
		DepthCacheTTL: time.Minute,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	for i := 0; i < 1050; i++ {
		env := &messaging.Envelope{
			ID:      fmt.Sprintf("burst-msg-%d", i),
			Payload: []byte("x"),
		}
		del := NewFakeDelivery(env)
		_ = receiver.Emit(ctx, del)
		waitFor(t, time.Second, "acked", func() bool { return del.IsAcked() })
	}

	qc := countingOutbox.GetQueryCount()
	if qc < 1001 {
		t.Errorf("expected at least 1001 QueryPending calls (one per distinct partition key), got %d", qc)
	}
}

// ---------------------------------------------------------------------------
// T10: DepthCacheTTL wired from RoutePolicy
// ---------------------------------------------------------------------------

// TestDepthCacheTTL_WiredFromPolicy validates that DepthCacheTTL in
// RoutePolicy is propagated to the RouteRunner's depth cache via
// Runtime.Start, so the configured TTL takes effect.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────
//
//	DepthCacheTTL = 50ms, send 2 messages 100ms apart
//	Both trigger QueryPending (cache expired between sends)
//
// ───────────────────────────────────────────────────────────────────────
func TestDepthCacheTTL_WiredFromPolicy(t *testing.T) {
	countingOutbox := NewQueryCountingOutboxStore()
	lease := NewFakeLeaseStore()

	rt := goruntime.New(
		goruntime.WithInstanceID("bridge-ttl-wire"),
		goruntime.WithOutboxStore(countingOutbox),
		goruntime.WithLeaseStore(lease),
		goruntime.WithDLQStore(NewFakeDLQStore()),
	)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	session := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-ttlwire")

	cfg := goruntime.RouteConfig{
		ID: "ttlwire-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:   domain.DeliverySharedOutbox,
			MaxOutboxDepth: 10000,
			DepthCacheTTL:  50 * time.Millisecond,
		},
		Bindings: []domain.DestinationBinding{
			{ID: "b1", SessionID: "mqtt-ttlwire"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, session, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "session started", func() bool {
		return session.IsStarted()
	})

	env1 := &messaging.Envelope{ID: "ttlwire-1", Payload: []byte("x")}
	del1 := NewFakeDelivery(env1)
	_ = receiver.Emit(ctx, del1)
	waitFor(t, time.Second, "first acked", func() bool { return del1.IsAcked() })

	countAfterFirst := countingOutbox.GetQueryCount()

	time.Sleep(100 * time.Millisecond) // FIXED: wait for DepthCacheTTL (50ms) to expire

	env2 := &messaging.Envelope{ID: "ttlwire-2", Payload: []byte("x")}
	del2 := NewFakeDelivery(env2)
	_ = receiver.Emit(ctx, del2)
	waitFor(t, time.Second, "second acked", func() bool { return del2.IsAcked() })

	countAfterSecond := countingOutbox.GetQueryCount()
	if countAfterSecond <= countAfterFirst {
		t.Errorf("expected cache to expire (TTL=50ms, wait=100ms), QueryPending before=%d after=%d",
			countAfterFirst, countAfterSecond)
	}
}

// ---------------------------------------------------------------------------
// T11: DrainMaxBatchSize/DrainMaxConcurrency wired from config
// ---------------------------------------------------------------------------

// TestDrainConfig_WiredFromSessionConfig validates that DrainMaxBatchSize
// and DrainMaxConcurrency values from SessionConfig are propagated to
// the OutboxDrainer via Runtime.Start.
func TestDrainConfig_WiredFromSessionConfig(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()

	rt := goruntime.New(
		goruntime.WithInstanceID("bridge-drain-config"),
		goruntime.WithOutboxStore(outbox),
		goruntime.WithLeaseStore(lease),
		goruntime.WithDLQStore(NewFakeDLQStore()),
	)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	session := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-drainwire")
	sessCfg.DrainMaxBatchSize = 2
	sessCfg.DrainMaxConcurrency = 1

	cfg := goruntime.RouteConfig{
		ID: "drainwire-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Bindings: []domain.DestinationBinding{
			{ID: "b1", SessionID: "mqtt-drainwire"},
		},
	}

	if err := rt.AddRoute(cfg, receiver, sender, session, &sessCfg); err != nil {
		t.Fatalf("AddRoute failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "session started", func() bool {
		return session.IsStarted()
	})

	for i := 0; i < 5; i++ {
		env := &messaging.Envelope{ID: fmt.Sprintf("drainwire-%d", i), Payload: []byte("x")}
		del := NewFakeDelivery(env)
		_ = receiver.Emit(ctx, del)
		waitFor(t, time.Second, "acked", func() bool { return del.IsAcked() })
	}

	waitFor(t, 3*time.Second, "all records completed", func() bool {
		return outbox.CompletedCount() >= 5
	})

	if sender.SentCount() < 5 {
		t.Fatalf("expected at least 5 sends (wired config should process all), got %d", sender.SentCount())
	}
}

// ---------------------------------------------------------------------------
// T12: QueryPending error fails delivery (fail-closed)
// ---------------------------------------------------------------------------

// ErrorOutboxStore wraps FakeOutboxStore and injects QueryPending errors.
type ErrorOutboxStore struct {
	*FakeOutboxStore
	queryErr error
}

func NewErrorOutboxStore(qErr error) *ErrorOutboxStore {
	return &ErrorOutboxStore{
		FakeOutboxStore: NewFakeOutboxStore(),
		queryErr:        qErr,
	}
}

func (s *ErrorOutboxStore) QueryPending(_ context.Context, _ string, _ int) ([]domain.OutboxRecord, error) {
	if s.queryErr != nil {
		return nil, s.queryErr
	}
	return nil, nil
}

// TestQueryPendingError_FailsClosed validates that when QueryPending
// returns an error, the delivery is retried (fail-closed).
func TestQueryPendingError_FailsClosed(t *testing.T) {
	errOutbox := NewErrorOutboxStore(errors.New("db connection lost"))
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	runner := goruntime.NewRouteRunnerFromConfig(goruntime.RouteRunnerConfig{
		RouteID:     "failclosed-route",
		Policy:      domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox, MaxOutboxDepth: 100},
		Receiver:    receiver,
		Sender:      sender,
		OutboxStore: errOutbox,
		Bindings:    []domain.DestinationBinding{{ID: "b1", SessionID: "failclosed-sess"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	env := &messaging.Envelope{ID: "failclosed-1", Payload: []byte("x")}
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	waitFor(t, time.Second, "retried", func() bool { return del.IsRetried() })

	if del.IsAcked() {
		t.Fatal("expected delivery to be retried, not acked")
	}
}

// ---------------------------------------------------------------------------
// T13: absoluteMaxBatchSize clamps excessive MaxBatchSize
// ---------------------------------------------------------------------------

// TestAbsoluteMaxBatchSize_Clamps validates that MaxBatchSize values
// exceeding absoluteMaxBatchSize (10000) are clamped.
func TestAbsoluteMaxBatchSize_Clamps(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox := NewFakeOutboxStore()
	sender := NewFakeSender()
	lease := NewFakeLeaseStore()
	pk := domain.OutboxPartitionKey("sess-1", "")
	_, _ = lease.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	drainer := goruntime.NewOutboxDrainerFromConfig(goruntime.OutboxDrainerConfig{
		OutboxStore:         outbox,
		LeaseStore:          lease,
		Sender:              sender,
		DLQ:                 goruntime.NewDLQRouter(nil),
		RouteID:             "clamp-route",
		PartitionKey:        pk,
		LeaseID:             "sess-1",
		OwnerID:             token.Owner,
		Policy:              domain.RoutePolicy{}.WithDefaults(),
		Strategy:            domain.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize:      100,
		DrainMaxBatchSize:   1<<31 - 1,
		DrainMaxConcurrency: 10,
		TokenFn: func() (domain.LeaseToken, bool) {
			return token, true
		},
	})

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		rec := domain.OutboxRecord{
			ID: fmt.Sprintf("clamp-%d", i), RouteID: "clamp-route",
			EnvelopeID: fmt.Sprintf("env-clamp-%d", i), BindingID: "bind-1",
			SessionID: "sess-1",
			Envelope:  messaging.Envelope{ID: fmt.Sprintf("env-clamp-%d", i), Payload: []byte("data")},
			Status:    domain.OutboxPending,
		}
		_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})
	}

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 5 {
		t.Fatalf("expected 5 sent (drainer should work with clamped batch size), got %d", sender.SentCount())
	}
}

// ---------------------------------------------------------------------------
// T14: MetricOutboxRecordFailures emitted on failed records
// ---------------------------------------------------------------------------

// TestOutboxDrainer_EmitsRecordFailureMetric validates that when a
// record fails to process (Complete returns a non-stale error),
// MetricOutboxRecordFailures is emitted.
func TestOutboxDrainer_EmitsRecordFailureMetric(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	rec := &ports.RecordingExporter{}

	outbox := NewFakeOutboxStore()
	outbox.CompleteFn = func(_ []string, _ domain.LeaseToken) error {
		return errors.New("storage unavailable")
	}
	sender := NewFakeSender()

	lease := NewFakeLeaseStore()
	pk := domain.OutboxPartitionKey("sess-1", "")
	_, _ = lease.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	drainer := goruntime.NewOutboxDrainerFromConfig(goruntime.OutboxDrainerConfig{
		OutboxStore:    outbox,
		LeaseStore:     lease,
		Sender:         sender,
		DLQ:            goruntime.NewDLQRouter(nil),
		RouteID:        "metric-route",
		PartitionKey:   pk,
		LeaseID:        "sess-1",
		OwnerID:        token.Owner,
		Policy:         domain.RoutePolicy{}.WithDefaults(),
		Strategy:       domain.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize: 10,
		Metrics:        rec,
		TokenFn: func() (domain.LeaseToken, bool) {
			return token, true
		},
	})

	ctx := context.Background()
	outboxRec := domain.OutboxRecord{
		ID: "rec-fail", RouteID: "metric-route",
		EnvelopeID: "env-fail", BindingID: "bind-1",
		SessionID: "sess-1",
		Envelope:  messaging.Envelope{ID: "env-fail", Payload: []byte("data")},
		Status:    domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{outboxRec})

	drainCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	failures := rec.FindEntries(shared.MetricOutboxRecordFailures)
	if len(failures) == 0 {
		t.Fatal("expected MetricOutboxRecordFailures to be emitted on record processing failure")
	}

	found := false
	for _, tag := range failures[0].Tags {
		if tag.Key == shared.TagKeyRouteID && tag.Value == "metric-route" {
			found = true
		}
	}
	if !found {
		t.Error("expected route_id tag on OutboxRecordFailures metric")
	}
}
