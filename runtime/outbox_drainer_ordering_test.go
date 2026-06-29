package runtime_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
	outboxpkg "github.com/mariotoffia/gobridge/runtime/outbox"
)

// orderingSender is a gated recording sender for the per-ordering-key
// serialization test (finding #28). Each Send records the envelope ID in
// invocation order, tracks per-key concurrent in-flight (and its peak),
// then blocks on a shared release channel so the test can hold a known
// set of sends simultaneously in flight and observe whether same-key
// sends ever overlap. Releasing the channel lets every blocked (and any
// subsequent) send drain.
type orderingSender struct {
	mu          sync.Mutex
	invocations []string
	keyInflight map[string]int
	keyMax      map[string]int
	blocked     int
	peakBlocked int
	released    bool
	releaseCh   chan struct{}
}

func newOrderingSender() *orderingSender {
	return &orderingSender{
		keyInflight: make(map[string]int),
		keyMax:      make(map[string]int),
		releaseCh:   make(chan struct{}),
	}
}

func (s *orderingSender) Send(_ context.Context, msg ports.OutboundMessage) error {
	key, _ := msg.Envelope.Headers().GetString(messaging.HeaderOrderingKey)

	s.mu.Lock()
	s.invocations = append(s.invocations, msg.Envelope.ID())
	if key != "" {
		s.keyInflight[key]++
		if s.keyInflight[key] > s.keyMax[key] {
			s.keyMax[key] = s.keyInflight[key]
		}
	}
	s.blocked++
	if s.blocked > s.peakBlocked {
		s.peakBlocked = s.blocked
	}
	ch := s.releaseCh
	s.mu.Unlock()

	<-ch // hold the send in flight until the test releases it

	s.mu.Lock()
	s.blocked--
	if key != "" {
		s.keyInflight[key]--
	}
	s.mu.Unlock()
	return nil
}

func (s *orderingSender) ReleaseAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.released {
		s.released = true
		close(s.releaseCh)
	}
}

func (s *orderingSender) Blocked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocked
}

func (s *orderingSender) PeakBlocked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peakBlocked
}

func (s *orderingSender) KeyMax(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keyMax[key]
}

func (s *orderingSender) InvokedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.invocations)
}

// InvocationsFor returns the recorded send order filtered to ids in want,
// preserving the order they were sent.
func (s *orderingSender) InvocationsFor(want map[string]struct{}) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(want))
	for _, id := range s.invocations {
		if _, ok := want[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

func makeOrderingRecord(t *testing.T, id, orderingKey string) *persistence.OutboxRecord {
	t.Helper()
	in := messaging.EnvelopeInput{ID: id}
	if orderingKey != "" {
		in.Headers = map[string]any{messaging.HeaderOrderingKey: orderingKey}
	}
	env := messaging.MustEnvelopeWithReserved(in)
	rec, err := persistence.NewOutboxRecord(persistence.OutboxSpec{
		ID:         id,
		RouteID:    "route-1",
		EnvelopeID: id,
		BindingID:  "bind-" + id,
		SessionID:  "sess-1",
		Envelope:   *env,
	})
	if err != nil {
		t.Fatalf("NewOutboxRecord(%s): %v", id, err)
	}
	return rec
}

// TestOutboxDrainer_PerOrderingKeySerialization verifies finding #28:
// records sharing a non-empty ordering key are delivered SEQUENTIALLY in
// persisted order and never overlap in time, while records with distinct
// keys (and keyless records) drain concurrently.
//
// The claim order is [a0,a1,a2,n0,b0,n1] where a* share key A, b0 has
// key B, and n* have no key. With DrainMaxConcurrency=4, a buggy
// per-record drainer would put a0,a1,a2 (the first three claimed) in
// flight together, driving same-key concurrency to 3; the correct
// per-group drainer keeps key A at concurrency 1 while still running the
// four groups (A, n0, B, n1) in parallel.
func TestOutboxDrainer_PerOrderingKeySerialization(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}

	const keyA = "order-key-A"
	const keyB = "order-key-B"

	records := []*persistence.OutboxRecord{
		makeOrderingRecord(t, "a0", keyA),
		makeOrderingRecord(t, "a1", keyA),
		makeOrderingRecord(t, "a2", keyA),
		makeOrderingRecord(t, "n0", ""),
		makeOrderingRecord(t, "b0", keyB),
		makeOrderingRecord(t, "n1", ""),
	}

	sender := newOrderingSender()

	outbox, _, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.Sender = sender
		cfg.DrainMaxConcurrency = 4
		cfg.DrainBatchSize = 100
	})

	var claimed atomic.Bool
	outbox.ClaimFn = func(_ string, _ persistence.LeaseToken, _ int) ([]*persistence.OutboxRecord, error) {
		if claimed.Swap(true) {
			return nil, nil // single batch; subsequent cycles are empty
		}
		return records, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = drainer.Run(ctx) }()

	// Wait until the concurrency peak (four in-flight sends) is reached.
	// This is the moment a buggy per-record drainer would have a0,a1,a2
	// simultaneously in flight.
	waitFor(t, 5*time.Second, "four concurrent in-flight sends", func() bool {
		return sender.Blocked() == 4
	})

	// PRIMARY invariant: key A sends never overlap.
	if got := sender.KeyMax(keyA); got != 1 {
		t.Fatalf("key %q peak concurrency = %d, want 1 (same-key sends must be serialized)", keyA, got)
	}
	// Distinct groups ran in parallel.
	if peak := sender.PeakBlocked(); peak < 2 {
		t.Fatalf("peak in-flight = %d, want >= 2 (distinct keys must parallelize)", peak)
	}

	sender.ReleaseAll()

	waitFor(t, 5*time.Second, "all six records sent", func() bool {
		return sender.InvokedCount() == 6
	})

	// Key A delivered strictly in persisted order a0 -> a1 -> a2.
	keyAIDs := map[string]struct{}{"a0": {}, "a1": {}, "a2": {}}
	gotOrder := sender.InvocationsFor(keyAIDs)
	want := []string{"a0", "a1", "a2"}
	if len(gotOrder) != len(want) {
		t.Fatalf("key %q delivery order = %v, want %v", keyA, gotOrder, want)
	}
	for i := range want {
		if gotOrder[i] != want[i] {
			t.Fatalf("key %q delivery order = %v, want %v", keyA, gotOrder, want)
		}
	}

	// Final check: neither ordering key ever overlapped.
	if got := sender.KeyMax(keyA); got != 1 {
		t.Fatalf("key %q final peak concurrency = %d, want 1", keyA, got)
	}
	if got := sender.KeyMax(keyB); got > 1 {
		t.Fatalf("key %q final peak concurrency = %d, want <= 1", keyB, got)
	}
}
