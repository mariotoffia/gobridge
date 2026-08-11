package route

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// duplicateOutboxStore rejects every Persist as an already-durable record,
// modelling a source that redelivered an envelope ID the outbox already holds —
// or a producer that reused one.
type duplicateOutboxStore struct{ persists atomic.Int32 }

func (s *duplicateOutboxStore) Persist(context.Context, []*persistence.OutboxRecord) error {
	s.persists.Add(1)
	return shared.ErrDuplicateRecord
}

func (s *duplicateOutboxStore) Claim(context.Context, string, persistence.LeaseToken, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

func (s *duplicateOutboxStore) Complete(context.Context, []string, persistence.LeaseToken) error {
	return nil
}

func (s *duplicateOutboxStore) Expire(context.Context, time.Time, string) (int, error) {
	return 0, nil
}

func (s *duplicateOutboxStore) QueryPending(context.Context, string, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

// TestDuplicateIdentitySuppression_IsCountedNotSilent proves outbox duplicate
// suppression is an observable event.
//
// A duplicate persist is settled as success: the record is already durable, so
// the source may be acked. That is correct for a redelivery and indistinguishable
// — from the runtime's side — from a producer reusing an envelope ID for a
// DIFFERENT message, which the suppression silently discards. The ack stays, but
// it must leave a counted signal so an operator can see a source whose identity
// namespace is colliding instead of inferring it from missing messages.
//
// Mutation check: drop the counter from the ErrDuplicateRecord branch and the
// ack still happens with nothing recorded — exactly the silent case.
func TestDuplicateIdentitySuppression_IsCountedNotSilent(t *testing.T) {
	store := &duplicateOutboxStore{}
	metrics := &ports.RecordingExporter{}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID:     "dup-route",
		Policy:      routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox, MaxReplayAttempts: 3},
		Metrics:     metrics,
		OutboxStore: store,
		Bindings:    []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
		Resolver:    fixedResolver{plans: []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}},
	})

	del := &stubDelivery{env: countLessEnv("reused-producer-id")}
	if err := r.HandleDelivery(context.Background(), del); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	if store.persists.Load() != 1 {
		t.Fatalf("outbox persists = %d, want 1", store.persists.Load())
	}
	if !del.acked || del.retried {
		t.Fatalf("a duplicate record is already durable and must settle as success (acked=%v retried=%v)", del.acked, del.retried)
	}

	entries := metrics.FindEntries(shared.MetricOutboxDuplicateSuppressed)
	if len(entries) != 1 {
		t.Fatalf("%s emitted %d times, want 1 — suppression is invisible to operators",
			shared.MetricOutboxDuplicateSuppressed, len(entries))
	}
	var routeTagged bool
	for _, tag := range entries[0].Tags {
		if tag.Key == shared.TagKeyRouteID && tag.Value == "dup-route" {
			routeTagged = true
		}
	}
	if !routeTagged {
		t.Fatalf("%s tags = %v, want the colliding route identified", shared.MetricOutboxDuplicateSuppressed, entries[0].Tags)
	}
}
