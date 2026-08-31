package dlq_test

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// A DLQ write happens BEFORE the source delivery is settled, so the settle can
// fail after the evidence is durable: the message is redelivered, fails the same
// way, and is written again. With a random entry ID every such round produced a
// distinct row, so one terminal event accumulated duplicates for as long as the
// settle kept failing. Deriving the ID from the message and the delivery leg
// makes the repeat write land on the SAME entry, which the stores refuse as a
// duplicate — a refusal that means "already durably recorded", i.e. success.

// dedupingStore mirrors the real DLQ backends: an entry ID is a primary key, so
// a second write of the same ID is rejected with shared.ErrDuplicateRecord
// (memorydlq's map check, sqlitedlq's unique violation, dynamodbdlq's
// attribute_not_exists condition all produce exactly this).
type dedupingStore struct {
	*FakeStore
}

func newDedupingStore() *dedupingStore { return &dedupingStore{FakeStore: NewFakeStore()} }

func (s *dedupingStore) Write(ctx context.Context, entry routing.DLQEntry) error {
	for _, existing := range s.Entries {
		if existing.ID() == entry.ID() {
			return shared.ErrDuplicateRecord.With("entryID", entry.ID())
		}
	}
	return s.FakeStore.Write(ctx, entry)
}

func routeTwice(t *testing.T, store ports.DLQStore, env *messaging.Envelope, attempts1, attempts2 int) {
	t.Helper()
	r := dlq.New(store)
	ctx := context.Background()
	if err := r.Route(ctx, env, "route-1", "bind-1", "topic/a", "sess-1", "src-1", shared.ErrNotFound, attempts1); err != nil {
		t.Fatalf("first route: %v", err)
	}
	if err := r.Route(ctx, env, "route-1", "bind-1", "topic/a", "sess-1", "src-1", shared.ErrNotFound, attempts2); err != nil {
		t.Fatalf("second route after a failed settle must succeed, got %v", err)
	}
}

// TestRouter_RepeatedWriteOfOneTerminalEventIsOneEntry is the duplicate-DLQ
// regression: the same message failing the same way on the same delivery leg is
// ONE terminal event, whatever the attempt counter says, so a redelivery after a
// failed settle must not add a second row.
func TestRouter_RepeatedWriteOfOneTerminalEventIsOneEntry(t *testing.T) {
	store := newDedupingStore()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-dup-1"})

	// Different attempt counts: the redelivery genuinely IS a later attempt, so
	// the identity must not depend on the counter or nothing would ever collapse.
	routeTwice(t, store, env, 3, 4)

	if store.Count() != 1 {
		t.Fatalf("one terminal event must produce one DLQ entry, got %d", store.Count())
	}
}

// A duplicate refusal means the evidence is already durable, so Route must
// report success — otherwise the caller refuses to settle the source and the
// message redelivers forever, turning a benign duplicate into a stuck route.
func TestRouter_DuplicateRefusalIsDurableSuccess(t *testing.T) {
	store := newDedupingStore()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-dup-2"})
	metrics := &ports.RecordingExporter{}
	r := dlq.NewFromConfig(dlq.Config{Store: store, Metrics: metrics})

	ctx := context.Background()
	for range 3 {
		if err := r.Route(ctx, env, "route-1", "bind-1", "topic/a", "sess-1", "src-1", shared.ErrNotFound, 1); err != nil {
			t.Fatalf("a duplicate refusal must be reported as durable success, got %v", err)
		}
	}
	if store.Count() != 1 {
		t.Fatalf("expected one entry, got %d", store.Count())
	}
	if got := len(metrics.FindEntries(shared.MetricDLQDuplicateSuppressed)); got != 2 {
		t.Fatalf("each collapsed repeat must be counted once, got %d", got)
	}
}

// Identity is scoped to the delivery leg: a fan-out route that DLQs the same
// envelope on two bindings records two distinct failures, and two different
// messages never collide.
func TestRouter_EntryIdentityIsScopedToTheDeliveryLeg(t *testing.T) {
	store := newDedupingStore()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-fanout"})
	other := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-other"})
	r := dlq.New(store)
	ctx := context.Background()

	if err := r.Route(ctx, env, "route-1", "bind-a", "topic/a", "sess-1", "src-1", shared.ErrNotFound, 1); err != nil {
		t.Fatalf("binding a: %v", err)
	}
	if err := r.Route(ctx, env, "route-1", "bind-b", "topic/b", "sess-1", "src-1", shared.ErrNotFound, 1); err != nil {
		t.Fatalf("binding b: %v", err)
	}
	if err := r.Route(ctx, other, "route-1", "bind-a", "topic/a", "sess-1", "src-1", shared.ErrNotFound, 1); err != nil {
		t.Fatalf("other envelope: %v", err)
	}

	if store.Count() != 3 {
		t.Fatalf("distinct delivery legs and messages must produce distinct entries, got %d", store.Count())
	}
	seen := map[string]bool{}
	for _, e := range store.Entries {
		if seen[e.ID()] {
			t.Fatalf("entry IDs collided: %q", e.ID())
		}
		seen[e.ID()] = true
	}
}

// An envelope with no ID can never be identified, so the router falls back to a
// random ID rather than collapsing unrelated failures onto one row. Envelope IDs
// are required by construction, so this is a defensive floor, not a live path.
func TestRouter_EntryIDStaysUniqueWithoutAnEnvelopeID(t *testing.T) {
	store := NewFakeStore()
	r := dlq.New(store)
	ctx := context.Background()
	env := &messaging.Envelope{}

	for range 2 {
		if err := r.Route(ctx, env, "", "", "", "", "", shared.ErrNotFound, 1); err != nil {
			t.Fatalf("route: %v", err)
		}
	}
	if store.Count() != 2 {
		t.Fatalf("expected 2 entries, got %d", store.Count())
	}
	if store.Entries[0].ID() == store.Entries[1].ID() {
		t.Fatalf("identity-less envelopes must not share an entry ID, got %q twice", store.Entries[0].ID())
	}
}
