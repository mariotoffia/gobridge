package route

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// ════════════════════════════════════════════════════════════════════════════
// Chunk-1 — shared-outbox zero-plan fail-closed guard (resolvePlans).
//
// Deterministic: no time.Sleep sequences logic; the delivery pipeline is driven
// synchronously through HandleDelivery.
// ════════════════════════════════════════════════════════════════════════════

// countingOutboxStore counts Persist calls so a test can prove a zero-plan
// resolve NEVER persists (a zero-record Persist would ACK the source with zero
// delivery — silent loss). Every other OutboxStore method is inert.
type countingOutboxStore struct{ persists atomic.Int32 }

func (s *countingOutboxStore) Persist(context.Context, []*persistence.OutboxRecord) error {
	s.persists.Add(1)
	return nil
}

func (s *countingOutboxStore) Claim(context.Context, string, persistence.LeaseToken, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

func (s *countingOutboxStore) Complete(context.Context, []string, persistence.LeaseToken) error {
	return nil
}

func (s *countingOutboxStore) Expire(context.Context, time.Time, string, persistence.LeaseToken) (int, error) {
	return 0, nil
}

func (s *countingOutboxStore) QueryPending(context.Context, string, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

// TestSharedOutboxZeroPlans_NotAckedRoutedToPoison proves the shared-outbox
// fail-closed guard at the resolvePlans choke point. A resolver that returns ZERO
// dispatch plans (with a nil error) must NOT persist zero outbox records and then
// ACK the source as success — that is silent message loss with no DLQ/outbox
// evidence. Instead the empty resolve is a RECOVERABLE fault routed through the
// replay-cap gate: retry below MaxReplayAttempts, then poison to the DLQ at the
// cap — never a zero-record Persist, never a success ack.
//
// Mutation check: delete the len(plans)==0 guard in resolvePlans and this fails —
// buildOutboxRecords runs with zero plans, Persist is invoked (zero records), and
// the source is acked as success (no retry, no DLQ write).
func TestSharedOutboxZeroPlans_NotAckedRoutedToPoison(t *testing.T) {
	store := &countingOutboxStore{}
	dlqStore := &recordingDLQStore{}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "high1",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliverySharedOutbox,
			MaxReplayAttempts: 1, // small cap so the poison outcome is observable quickly
		},
		OutboxStore: store,
		DLQ:         dlq.New(dlqStore),
		Bindings:    []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
		Resolver:    fixedResolver{plans: []routing.DispatchPlan{}}, // ZERO plans, nil error
	})

	env := countLessEnv("high1-zero")

	// Below the cap: the empty resolve is recoverable → retry, never a persist,
	// never a success ack.
	del1 := &stubDelivery{env: env}
	if err := r.HandleDelivery(context.Background(), del1); err != nil {
		t.Fatalf("HandleDelivery(1): %v", err)
	}
	if got := store.persists.Load(); got != 0 {
		t.Fatalf("OutboxStore.Persist called %d times on a zero-plan resolve — a zero-record persist is silent loss", got)
	}
	if del1.acked {
		t.Fatal("source acked on a zero-plan resolve — silent message loss with no delivery")
	}
	if !del1.retried {
		t.Fatal("a below-cap zero-plan resolve must unsettle (retry), never a success ack")
	}

	// At the cap: the recoverable fault poisons to the DLQ, still never persisting.
	del2 := &stubDelivery{env: env}
	if err := r.HandleDelivery(context.Background(), del2); err != nil {
		t.Fatalf("HandleDelivery(2): %v", err)
	}
	if got := store.persists.Load(); got != 0 {
		t.Fatalf("OutboxStore.Persist called %d times at the cap — a zero-record persist is silent loss", got)
	}
	if got := dlqStore.writes.Load(); got != 1 {
		t.Fatalf("DLQ writes = %d, want 1 (a zero-plan resolve poisons per policy at the cap)", got)
	}
	if !del2.acked || del2.retried {
		t.Fatalf("at the cap the zero-plan resolve must settle terminally (acked=%v retried=%v)", del2.acked, del2.retried)
	}
}
