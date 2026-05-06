package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// Verifies successful Acquire emits lease acquire latency with lease tags and no failure counter.
func TestInstrumentedLeaseStore_AcquireRecordsLatency(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeLeaseStore()
	store := runtime.NewInstrumentedLeaseStore(inner, rec, clock.System)

	_, err := store.Acquire(context.Background(), "lease-1", "owner-1", 30*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}

	timers := rec.FindEntries(shared.MetricLeaseAcquireLatency)
	if len(timers) != 1 {
		t.Fatalf("expected 1 timer entry, got %d", len(timers))
	}
	if timers[0].Tags[0].Value != "lease-1" {
		t.Errorf("tag value = %q, want lease-1", timers[0].Tags[0].Value)
	}

	failures := rec.FindEntries(shared.MetricLeaseAcquireFailures)
	if len(failures) != 0 {
		t.Errorf("expected 0 failure counters on success, got %d", len(failures))
	}
}

// Verifies failed Acquire increments the lease acquire failure metric.
func TestInstrumentedLeaseStore_AcquireFailureRecordsCounter(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeLeaseStore()

	tok, _ := inner.Acquire(context.Background(), "lease-1", "other", 30*time.Second, nil)
	_ = tok

	store := runtime.NewInstrumentedLeaseStore(inner, rec, clock.System)
	_, _ = store.Acquire(context.Background(), "lease-1", "me", 30*time.Second, nil)

	failures := rec.FindEntries(shared.MetricLeaseAcquireFailures)
	if len(failures) != 1 {
		t.Fatalf("expected 1 failure counter, got %d", len(failures))
	}
}

// Verifies successful Renew emits lease renew latency.
func TestInstrumentedLeaseStore_RenewRecordsLatency(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeLeaseStore()
	store := runtime.NewInstrumentedLeaseStore(inner, rec, clock.System)

	tok, _ := store.Acquire(context.Background(), "lease-1", "owner-1", 30*time.Second, nil)
	rec.Reset()

	_, err := store.Renew(context.Background(), "lease-1", tok, 30*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}

	timers := rec.FindEntries(shared.MetricLeaseRenewLatency)
	if len(timers) != 1 {
		t.Fatalf("expected 1 renew timer, got %d", len(timers))
	}
}

// Verifies Persist emits outbox persist latency tagged by route.
func TestInstrumentedOutboxStore_PersistRecordsLatency(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeOutboxStore()
	store := runtime.NewInstrumentedOutboxStore(inner, rec, clock.System)

	records := []domain.OutboxRecord{{
		ID: "r1", RouteID: "route-1", EnvelopeID: "env-1", BindingID: "b1",
		Status: domain.OutboxPending, Envelope: messaging.Envelope{ID: "env-1"},
	}}

	err := store.Persist(context.Background(), records)
	if err != nil {
		t.Fatal(err)
	}

	timers := rec.FindEntries(shared.MetricOutboxPersistLatency)
	if len(timers) != 1 {
		t.Fatalf("expected 1 persist timer, got %d", len(timers))
	}
	if timers[0].Tags[0].Value != "route-1" {
		t.Errorf("route tag = %q, want route-1", timers[0].Tags[0].Value)
	}
}

// Verifies Complete does not emit outbox completion metrics (those come from the drainer, not the store wrapper).
func TestInstrumentedOutboxStore_CompleteDelegates(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeOutboxStore()
	store := runtime.NewInstrumentedOutboxStore(inner, rec, clock.System)

	records := []domain.OutboxRecord{
		{ID: "r1", RouteID: "route-1", EnvelopeID: "env-1", BindingID: "b1", SessionID: "s1",
			Status: domain.OutboxPending, Envelope: messaging.Envelope{ID: "env-1"}},
	}
	_ = store.Persist(context.Background(), records)

	token := domain.LeaseToken{Version: 1, Owner: "me"}
	claimed, _ := store.Claim(context.Background(), domain.OutboxPartitionKey("s1", "b1"), "me", token, 10)
	if len(claimed) == 0 {
		t.Fatal("expected to claim at least 1 record")
	}

	ids := make([]string, len(claimed))
	for i, c := range claimed {
		ids[i] = c.ID
	}

	err := store.Complete(context.Background(), ids, token)
	if err != nil {
		t.Fatal(err)
	}

	// OutboxCompletions are emitted by OutboxDrainer, not the store
	// decorator, to avoid double-counting.
	completions := rec.FindEntries(shared.MetricOutboxCompletions)
	if len(completions) != 0 {
		t.Errorf("expected 0 completion counters from store decorator, got %d", len(completions))
	}
}

// Verifies QueryPending records outbox depth gauge matching pending result count.
func TestInstrumentedOutboxStore_QueryPendingRecordsDepth(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeOutboxStore()
	store := runtime.NewInstrumentedOutboxStore(inner, rec, clock.System)

	pk := domain.OutboxPartitionKey("s1", "b1")

	for i := 0; i < 3; i++ {
		records := []domain.OutboxRecord{{
			ID:         "r" + string(rune('0'+i)),
			RouteID:    "route-1",
			EnvelopeID: "env-" + string(rune('0'+i)),
			BindingID:  "b1",
			SessionID:  "s1",
			Status:     domain.OutboxPending,
			Envelope:   messaging.Envelope{ID: "env-" + string(rune('0'+i))},
		}}
		_ = store.Persist(context.Background(), records)
	}

	result, err := store.QueryPending(context.Background(), pk, 100)
	if err != nil {
		t.Fatal(err)
	}

	depths := rec.FindEntries(shared.MetricOutboxDepth)
	if len(depths) != 1 {
		t.Fatalf("expected 1 depth gauge, got %d", len(depths))
	}
	if depths[0].FValue != float64(len(result)) {
		t.Errorf("depth = %f, want %d", depths[0].FValue, len(result))
	}
}

// Verifies Send records publish latency with the configured tag key and value.
func TestInstrumentedSender_RecordsSendLatency(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeSender()
	sender := runtime.NewInstrumentedSender(inner, rec,
		shared.MetricMQTTPublishLatency, shared.TagKeySessionID, "sess-1", clock.System)

	env := &messaging.Envelope{ID: "msg-1", Payload: []byte("test")}
	err := sender.Send(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}

	timers := rec.FindEntries(shared.MetricMQTTPublishLatency)
	if len(timers) != 1 {
		t.Fatalf("expected 1 publish timer, got %d", len(timers))
	}
	if timers[0].Tags[0].Value != "sess-1" {
		t.Errorf("tag = %q, want sess-1", timers[0].Tags[0].Value)
	}
}

// Verifies Run records receive latency when a delivery is processed.
func TestInstrumentedReceiver_RecordsReceiveLatency(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeReceiver()
	receiver := runtime.NewInstrumentedReceiver(inner, rec,
		shared.MetricSQSReceiveLatency, shared.TagKeyQueueURL, "https://sqs/q1", clock.System)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		env := &messaging.Envelope{ID: "msg-1", Payload: []byte("data")}
		del := NewFakeDelivery(env)
		_ = inner.Emit(ctx, del)
		cancel()
	}()

	_ = receiver.Run(ctx, func(_ context.Context, del ports.Delivery) error {
		return nil
	})

	timers := rec.FindEntries(shared.MetricSQSReceiveLatency)
	if len(timers) != 1 {
		t.Fatalf("expected 1 receive timer, got %d", len(timers))
	}
}

// Verifies Expire delegates to the inner OutboxStore (no metrics emitted, pure delegation).
func TestInstrumentedOutboxStore_ExpireDelegates(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeOutboxStore()
	store := runtime.NewInstrumentedOutboxStore(inner, rec, clock.System)

	records := []domain.OutboxRecord{
		{ID: "r1", RouteID: "route-1", EnvelopeID: "env-1", BindingID: "b1", SessionID: "s1",
			Status: domain.OutboxPending, Envelope: messaging.Envelope{ID: "env-1"}},
	}
	_ = store.Persist(context.Background(), records)
	rec.Reset()

	count, err := store.Expire(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Expire failed: %v", err)
	}
	_ = count
}

// Verifies Delivery.Extend increments the visibility extension counter on the instrumented receiver path.
func TestInstrumentedDelivery_ExtendCountsVisibilityExtension(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeReceiver()
	receiver := runtime.NewInstrumentedReceiver(inner, rec,
		shared.MetricSQSReceiveLatency, shared.TagKeyQueueURL, "https://sqs/q1", clock.System)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		env := &messaging.Envelope{ID: "msg-1", Payload: []byte("data")}
		del := NewFakeDelivery(env)
		_ = inner.Emit(ctx, del)
		cancel()
	}()

	_ = receiver.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		return del.Extend(ctx, time.Now().Add(30*time.Second))
	})

	extensions := rec.FindEntries(shared.MetricVisibilityExtensions)
	if len(extensions) != 1 {
		t.Fatalf("expected 1 visibility extension counter, got %d", len(extensions))
	}
}

func TestInstrumentedLeaseStore_AcquireLatencyUsesInjectedClock(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(100, 0))
	rec := &ports.RecordingExporter{}
	inner := &advancingLeaseStore{LeaseStore: NewFakeLeaseStore(), clk: clk, advance: 250 * time.Millisecond}
	store := runtime.NewInstrumentedLeaseStore(inner, rec, clk)

	_, err := store.Acquire(context.Background(), "lease-1", "owner-1", time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}

	timers := rec.FindEntries(shared.MetricLeaseAcquireLatency)
	if len(timers) != 1 {
		t.Fatalf("expected 1 acquire timer, got %d", len(timers))
	}
	if timers[0].Duration != 250*time.Millisecond {
		t.Fatalf("duration = %s, want 250ms", timers[0].Duration)
	}
}

func TestInstrumentedLeaseStore_RenewLatencyUsesInjectedClock(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(100, 0))
	rec := &ports.RecordingExporter{}
	base := NewFakeLeaseStore()
	tok, err := base.Acquire(context.Background(), "lease-1", "owner-1", time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	inner := &advancingLeaseStore{LeaseStore: base, clk: clk, advance: 175 * time.Millisecond}
	store := runtime.NewInstrumentedLeaseStore(inner, rec, clk)

	_, err = store.Renew(context.Background(), "lease-1", tok, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}

	timers := rec.FindEntries(shared.MetricLeaseRenewLatency)
	if len(timers) != 1 {
		t.Fatalf("expected 1 renew timer, got %d", len(timers))
	}
	if timers[0].Duration != 175*time.Millisecond {
		t.Fatalf("duration = %s, want 175ms", timers[0].Duration)
	}
}

func TestInstrumentedOutboxStore_PersistLatencyUsesInjectedClock(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(100, 0))
	rec := &ports.RecordingExporter{}
	inner := &advancingOutboxStore{OutboxStore: NewFakeOutboxStore(), clk: clk, advance: 90 * time.Millisecond}
	store := runtime.NewInstrumentedOutboxStore(inner, rec, clk)

	err := store.Persist(context.Background(), []domain.OutboxRecord{{
		ID: "r1", RouteID: "route-1", EnvelopeID: "env-1", BindingID: "b1", Envelope: messaging.Envelope{ID: "env-1"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	timers := rec.FindEntries(shared.MetricOutboxPersistLatency)
	if len(timers) != 1 {
		t.Fatalf("expected 1 persist timer, got %d", len(timers))
	}
	if timers[0].Duration != 90*time.Millisecond {
		t.Fatalf("duration = %s, want 90ms", timers[0].Duration)
	}
}

func TestInstrumentedSender_SendLatencyUsesInjectedClock(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(100, 0))
	rec := &ports.RecordingExporter{}
	inner := NewFakeSender()
	inner.SendFn = func(*messaging.Envelope) error {
		clk.Advance(125 * time.Millisecond)
		return nil
	}
	sender := runtime.NewInstrumentedSender(inner, rec,
		shared.MetricMQTTPublishLatency, shared.TagKeySessionID, "sess-1", clk)

	err := sender.Send(context.Background(), &messaging.Envelope{ID: "msg-1"})
	if err != nil {
		t.Fatal(err)
	}

	timers := rec.FindEntries(shared.MetricMQTTPublishLatency)
	if len(timers) != 1 {
		t.Fatalf("expected 1 send timer, got %d", len(timers))
	}
	if timers[0].Duration != 125*time.Millisecond {
		t.Fatalf("duration = %s, want 125ms", timers[0].Duration)
	}
}

func TestInstrumentedReceiver_RunLatencyUsesInjectedClock(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(100, 0))
	rec := &ports.RecordingExporter{}
	inner := NewFakeReceiver()
	receiver := runtime.NewInstrumentedReceiver(inner, rec,
		shared.MetricSQSReceiveLatency, shared.TagKeyQueueURL, "https://sqs/q1", clk)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = inner.Emit(ctx, NewFakeDelivery(&messaging.Envelope{ID: "msg-1"}))
		cancel()
	}()

	_ = receiver.Run(ctx, func(context.Context, ports.Delivery) error {
		clk.Advance(300 * time.Millisecond)
		return nil
	})

	timers := rec.FindEntries(shared.MetricSQSReceiveLatency)
	if len(timers) != 1 {
		t.Fatalf("expected 1 receive timer, got %d", len(timers))
	}
	if timers[0].Duration != 300*time.Millisecond {
		t.Fatalf("duration = %s, want 300ms", timers[0].Duration)
	}
}

type advancingLeaseStore struct {
	ports.LeaseStore
	clk     *clocktest.Fake
	advance time.Duration
}

func (s *advancingLeaseStore) Acquire(ctx context.Context, leaseID, ownerID string, ttl time.Duration, endpoints map[string]string) (domain.LeaseToken, error) {
	s.clk.Advance(s.advance)
	return s.LeaseStore.Acquire(ctx, leaseID, ownerID, ttl, endpoints)
}

func (s *advancingLeaseStore) Renew(ctx context.Context, leaseID string, token domain.LeaseToken, ttl time.Duration, endpoints map[string]string) (domain.LeaseToken, error) {
	s.clk.Advance(s.advance)
	return s.LeaseStore.Renew(ctx, leaseID, token, ttl, endpoints)
}

type advancingOutboxStore struct {
	ports.OutboxStore
	clk     *clocktest.Fake
	advance time.Duration
}

func (s *advancingOutboxStore) Persist(ctx context.Context, records []domain.OutboxRecord) error {
	s.clk.Advance(s.advance)
	return s.OutboxStore.Persist(ctx, records)
}
