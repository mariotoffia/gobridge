package runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// idempotentOutboxStore is a test double implementing the contract: the
// stores agent is making ports.OutboxStore.Persist idempotent PER RECORD.
// Duplicate envelope records (keyed by envelope-id + binding-id) are skipped,
// genuinely new records are persisted, and ErrDuplicateRecord is returned ONLY
// when EVERY record in the call already existed. This is the semantics against
// which dispatch.go's ack-on-ErrDuplicateRecord (dispatch.go:686) is correct:
// a partial-overlap redelivery persists the missing legs and returns nil (so
// the delivery is ACKed after the new legs are durable), and a full-overlap
// redelivery returns ErrDuplicateRecord (so the already-durable delivery is
// ACKed without a spurious retry).
type idempotentOutboxStore struct {
	mu           sync.Mutex
	records      map[string]*persistence.OutboxRecord // dedup key -> record
	persistCalls int
}

func newIdempotentOutboxStore() *idempotentOutboxStore {
	return &idempotentOutboxStore{records: make(map[string]*persistence.OutboxRecord)}
}

func dedupKey(rec *persistence.OutboxRecord) string {
	return rec.EnvelopeID() + ":" + rec.BindingID()
}

func (s *idempotentOutboxStore) Persist(_ context.Context, records []*persistence.OutboxRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persistCalls++

	persistedNew := 0
	for _, rec := range records {
		key := dedupKey(rec)
		if _, exists := s.records[key]; exists {
			continue // per-record idempotency: skip the duplicate leg
		}
		s.records[key] = persistence.RehydrateFromSnapshot(rec.PersistenceSnapshot())
		persistedNew++
	}

	// ErrDuplicateRecord ONLY when nothing new was persisted (every record
	// already existed). Any new record means the call did useful work and
	// must report success so the source is ACKed with the new legs durable.
	if persistedNew == 0 && len(records) > 0 {
		return shared.ErrDuplicateRecord
	}
	return nil
}

func (s *idempotentOutboxStore) Claim(_ context.Context, _ string, _ persistence.LeaseToken, _ int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

func (s *idempotentOutboxStore) Complete(_ context.Context, _ []string, _ persistence.LeaseToken) error {
	return nil
}

func (s *idempotentOutboxStore) Expire(_ context.Context, _ time.Time, _ string, _ persistence.LeaseToken) (int, error) {
	return 0, nil
}

func (s *idempotentOutboxStore) QueryPending(_ context.Context, _ string, _ int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

func (s *idempotentOutboxStore) has(envID, bindingID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.records[envID+":"+bindingID]
	return ok
}

func (s *idempotentOutboxStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

func (s *idempotentOutboxStore) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistCalls
}

// sequenceResolver returns a different plan set on each Resolve call, driving
// the partial-overlap redelivery scenario: first pass resolves {A}, the
// redelivery resolves {A, B}.
type sequenceResolver struct {
	mu  sync.Mutex
	seq [][]routing.DispatchPlan
	i   int
}

func (r *sequenceResolver) Resolve(_ context.Context, _ *messaging.Envelope) ([]routing.DispatchPlan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := r.i
	if idx >= len(r.seq) {
		idx = len(r.seq) - 1
	}
	r.i++
	return r.seq[idx], nil
}

// TestRouteRunner_SharedOutbox_PartialOverlapRedelivery_PersistsMissingLeg is
// the verification. It proves that with the new per-record-idempotent
// Persist contract, a redelivery whose fan-out partially overlaps an
// already-persisted set does NOT lose the new leg:
//
//	delivery 1: resolver -> {A}          -> Persist({A}) ok, ack fails
//	delivery 2: resolver -> {A, B}       -> Persist({A,B}): A skipped, B new
//	                                        -> returns nil -> delivery ACKed
//
// Under the OLD "any duplicate => ErrDuplicateRecord (and stop)" semantics the
// second Persist would have returned ErrDuplicateRecord, dispatch would ACK,
// and B would be silently lost. This test locks in that B is persisted and the
// redelivery is ACKed exactly per the contract.
func TestRouteRunner_SharedOutbox_PartialOverlapRedelivery_PersistsMissingLeg(t *testing.T) {
	store := newIdempotentOutboxStore()
	resolver := &sequenceResolver{seq: [][]routing.DispatchPlan{
		{{BindingID: "bind-A", Address: "topic/a"}},
		{{BindingID: "bind-A", Address: "topic/a"}, {BindingID: "bind-B", Address: "topic/b"}},
	}}

	receiver, _, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliverySharedOutbox
		cfg.Policy.DispatchMode = routing.DispatchFanOut
		cfg.OutboxStore = store
		cfg.Bindings = []routing.DestinationBinding{
			{ID: "bind-A", SessionID: "sess-c3", Address: "topic/a"},
			{ID: "bind-B", SessionID: "sess-c3", Address: "topic/b"},
		}
		cfg.Resolver = resolver
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-c3", Payload: []byte("data")})

	// Delivery 1: resolves {A}. The ack "fails" (broker never sees it), which
	// is what triggers the redelivery below. FakeDelivery still marks itself
	// Acked, so we synchronize on the store having leg A instead.
	del1 := NewFakeDelivery(env)
	del1.AckErr = context.DeadlineExceeded
	if err := receiver.Emit(ctx, del1); err != nil {
		t.Fatalf("emit del1: %v", err)
	}
	waitFor(t, time.Second, "leg A persisted on first delivery", func() bool {
		return store.has("msg-c3", "bind-A")
	})
	if store.count() != 1 {
		t.Fatalf("after first delivery expected 1 record (A), got %d", store.count())
	}

	// Delivery 2 (redelivery of the same envelope): resolves {A, B}. A is a
	// duplicate and skipped; B is new and persisted; Persist returns nil so
	// the delivery is ACKed.
	del2 := NewFakeDelivery(env)
	if err := receiver.Emit(ctx, del2); err != nil {
		t.Fatalf("emit del2: %v", err)
	}
	waitFor(t, time.Second, "leg B persisted and redelivery acked", func() bool {
		return store.has("msg-c3", "bind-B") && del2.IsAcked()
	})

	if !store.has("msg-c3", "bind-A") || !store.has("msg-c3", "bind-B") {
		t.Fatalf("both legs must be durable: A=%v B=%v",
			store.has("msg-c3", "bind-A"), store.has("msg-c3", "bind-B"))
	}
	if store.count() != 2 {
		t.Fatalf("expected exactly 2 records (A once, B once), got %d", store.count())
	}
	if !del2.IsAcked() {
		t.Fatal("redelivery must be ACKed once the missing leg is durable")
	}
	if got := store.calls(); got != 2 {
		t.Fatalf("expected 2 Persist calls, got %d", got)
	}
}

// TestRouteRunner_SharedOutbox_FullOverlapRedelivery_AcksWithoutRetry is the
// companion to the partial-overlap case: when the redelivery resolves the
// exact same set that is already durable, Persist returns ErrDuplicateRecord
// and dispatch ACKs the delivery without a spurious retry (dispatch.go:686).
func TestRouteRunner_SharedOutbox_FullOverlapRedelivery_AcksWithoutRetry(t *testing.T) {
	store := newIdempotentOutboxStore()
	resolver := &sequenceResolver{seq: [][]routing.DispatchPlan{
		{{BindingID: "bind-A", Address: "topic/a"}},
	}}

	receiver, _, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliverySharedOutbox
		cfg.OutboxStore = store
		cfg.Bindings = []routing.DestinationBinding{
			{ID: "bind-A", SessionID: "sess-c3", Address: "topic/a"},
		}
		cfg.Resolver = resolver
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-c3-full", Payload: []byte("data")})

	del1 := NewFakeDelivery(env)
	if err := receiver.Emit(ctx, del1); err != nil {
		t.Fatalf("emit del1: %v", err)
	}
	waitFor(t, time.Second, "leg A persisted", func() bool { return store.has("msg-c3-full", "bind-A") })

	del2 := NewFakeDelivery(env)
	if err := receiver.Emit(ctx, del2); err != nil {
		t.Fatalf("emit del2: %v", err)
	}
	waitFor(t, time.Second, "duplicate redelivery acked", del2.IsAcked)

	if store.count() != 1 {
		t.Fatalf("full-overlap redelivery must not create a second record, got %d", store.count())
	}
	if del2.IsRetried() {
		t.Fatal("full-overlap redelivery must ACK (ErrDuplicateRecord), not retry")
	}
}
