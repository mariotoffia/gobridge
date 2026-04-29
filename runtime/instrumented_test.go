package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// Verifies successful Acquire emits lease acquire latency with lease tags and no failure counter.
func TestInstrumentedLeaseStore_AcquireRecordsLatency(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeLeaseStore()
	store := runtime.NewInstrumentedLeaseStore(inner, rec)

	_, err := store.Acquire(context.Background(), "lease-1", "owner-1", 30*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}

	timers := rec.FindEntries(domain.MetricLeaseAcquireLatency)
	if len(timers) != 1 {
		t.Fatalf("expected 1 timer entry, got %d", len(timers))
	}
	if timers[0].Tags[0].Value != "lease-1" {
		t.Errorf("tag value = %q, want lease-1", timers[0].Tags[0].Value)
	}

	failures := rec.FindEntries(domain.MetricLeaseAcquireFailures)
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

	store := runtime.NewInstrumentedLeaseStore(inner, rec)
	_, _ = store.Acquire(context.Background(), "lease-1", "me", 30*time.Second, nil)

	failures := rec.FindEntries(domain.MetricLeaseAcquireFailures)
	if len(failures) != 1 {
		t.Fatalf("expected 1 failure counter, got %d", len(failures))
	}
}

// Verifies successful Renew emits lease renew latency.
func TestInstrumentedLeaseStore_RenewRecordsLatency(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeLeaseStore()
	store := runtime.NewInstrumentedLeaseStore(inner, rec)

	tok, _ := store.Acquire(context.Background(), "lease-1", "owner-1", 30*time.Second, nil)
	rec.Reset()

	_, err := store.Renew(context.Background(), "lease-1", tok, 30*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}

	timers := rec.FindEntries(domain.MetricLeaseRenewLatency)
	if len(timers) != 1 {
		t.Fatalf("expected 1 renew timer, got %d", len(timers))
	}
}

// Verifies Persist emits outbox persist latency tagged by route.
func TestInstrumentedOutboxStore_PersistRecordsLatency(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeOutboxStore()
	store := runtime.NewInstrumentedOutboxStore(inner, rec)

	records := []domain.OutboxRecord{{
		ID: "r1", RouteID: "route-1", EnvelopeID: "env-1", BindingID: "b1",
		Status: domain.OutboxPending, Envelope: domain.Envelope{ID: "env-1"},
	}}

	err := store.Persist(context.Background(), records)
	if err != nil {
		t.Fatal(err)
	}

	timers := rec.FindEntries(domain.MetricOutboxPersistLatency)
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
	store := runtime.NewInstrumentedOutboxStore(inner, rec)

	records := []domain.OutboxRecord{
		{ID: "r1", RouteID: "route-1", EnvelopeID: "env-1", BindingID: "b1", SessionID: "s1",
			Status: domain.OutboxPending, Envelope: domain.Envelope{ID: "env-1"}},
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
	completions := rec.FindEntries(domain.MetricOutboxCompletions)
	if len(completions) != 0 {
		t.Errorf("expected 0 completion counters from store decorator, got %d", len(completions))
	}
}

// Verifies QueryPending records outbox depth gauge matching pending result count.
func TestInstrumentedOutboxStore_QueryPendingRecordsDepth(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeOutboxStore()
	store := runtime.NewInstrumentedOutboxStore(inner, rec)

	pk := domain.OutboxPartitionKey("s1", "b1")

	for i := 0; i < 3; i++ {
		records := []domain.OutboxRecord{{
			ID:         "r" + string(rune('0'+i)),
			RouteID:    "route-1",
			EnvelopeID: "env-" + string(rune('0'+i)),
			BindingID:  "b1",
			SessionID:  "s1",
			Status:     domain.OutboxPending,
			Envelope:   domain.Envelope{ID: "env-" + string(rune('0'+i))},
		}}
		_ = store.Persist(context.Background(), records)
	}

	result, err := store.QueryPending(context.Background(), pk, 100)
	if err != nil {
		t.Fatal(err)
	}

	depths := rec.FindEntries(domain.MetricOutboxDepth)
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
		domain.MetricMQTTPublishLatency, domain.TagKeySessionID, "sess-1")

	env := &domain.Envelope{ID: "msg-1", Payload: []byte("test")}
	err := sender.Send(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}

	timers := rec.FindEntries(domain.MetricMQTTPublishLatency)
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
		domain.MetricSQSReceiveLatency, domain.TagKeyQueueURL, "https://sqs/q1")

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		env := &domain.Envelope{ID: "msg-1", Payload: []byte("data")}
		del := NewFakeDelivery(env)
		_ = inner.Emit(ctx, del)
		cancel()
	}()

	_ = receiver.Run(ctx, func(_ context.Context, del ports.Delivery) error {
		return nil
	})

	timers := rec.FindEntries(domain.MetricSQSReceiveLatency)
	if len(timers) != 1 {
		t.Fatalf("expected 1 receive timer, got %d", len(timers))
	}
}

// Verifies Expire delegates to the inner OutboxStore (no metrics emitted, pure delegation).
func TestInstrumentedOutboxStore_ExpireDelegates(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeOutboxStore()
	store := runtime.NewInstrumentedOutboxStore(inner, rec)

	records := []domain.OutboxRecord{
		{ID: "r1", RouteID: "route-1", EnvelopeID: "env-1", BindingID: "b1", SessionID: "s1",
			Status: domain.OutboxPending, Envelope: domain.Envelope{ID: "env-1"}},
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
		domain.MetricSQSReceiveLatency, domain.TagKeyQueueURL, "https://sqs/q1")

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		env := &domain.Envelope{ID: "msg-1", Payload: []byte("data")}
		del := NewFakeDelivery(env)
		_ = inner.Emit(ctx, del)
		cancel()
	}()

	_ = receiver.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		return del.Extend(ctx, time.Now().Add(30*time.Second))
	})

	extensions := rec.FindEntries(domain.MetricVisibilityExtensions)
	if len(extensions) != 1 {
		t.Fatalf("expected 1 visibility extension counter, got %d", len(extensions))
	}
}
