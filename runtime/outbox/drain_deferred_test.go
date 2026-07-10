package outbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Coverage for finding 9 (audit D2): a batch-deadline/cancel abort mid-batch
// must Release every claimed-but-undelivered record back to pending and count
// it as DEFERRED — never as a success. Both drain-loop deferral sites are
// pinned:
//
//  1. the group loop's per-iteration ctx check (loop.go) that releases the
//     unattempted tail of an ordering group when the caller ctx dies, and
//  2. processRecord's ctx.Err() branch (retry.go) that releases the record
//     whose in-flight send was aborted by the batch ctx and returns
//     errBatchDeadlineDeferred.
//
// White-box: drainBatch is exercised directly so the abort can be sequenced
// deterministically via channels — no wall-clock waits.

// deferredFakeStore implements ports.OutboxStore + ports.OutboxReleaser,
// recording Complete/Release calls. Claim returns a fixed record set.
type deferredFakeStore struct {
	mu        sync.Mutex
	claimable []*persistence.OutboxRecord
	completed []string
	released  []string

	// expireCalls records the partition each Expire call was scoped to, so
	// H1 tests can assert the bulk sweep is deferred for dlq-policy drainers
	// and runs (scoped to the drainer's partition) for drop-policy drainers.
	expireCalls []string
	// expireCount is the count Expire returns (default 0).
	expireCount int

	// completeGate, when non-nil, is received from before Complete returns;
	// completeErr is then returned. Used to sequence the stale-cancel subtest.
	completeGate <-chan struct{}
	completeErr  error

	// releaseErr, when non-nil, is returned by Release. Used by the M4 test to
	// simulate a store I/O error on Release (distinct from a stale token).
	releaseErr error

	// completeSignal, when non-nil, receives each id passed to Complete
	// (non-blocking), so a test can deterministically observe WHETHER Complete
	// ran — used by the HIGH-2 post-send-fence test to prove a watchdog-abandoned
	// send never completes.
	completeSignal chan string
}

func (s *deferredFakeStore) Persist(context.Context, []*persistence.OutboxRecord) error {
	return nil
}

func (s *deferredFakeStore) Claim(_ context.Context, _ string, _ persistence.LeaseToken, _ int) ([]*persistence.OutboxRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := s.claimable
	s.claimable = nil
	return recs, nil
}

func (s *deferredFakeStore) Complete(_ context.Context, ids []string, _ persistence.LeaseToken) error {
	if s.completeGate != nil {
		<-s.completeGate
	}
	s.mu.Lock()
	s.completed = append(s.completed, ids...)
	s.mu.Unlock()
	if s.completeSignal != nil {
		for _, id := range ids {
			select {
			case s.completeSignal <- id:
			default:
			}
		}
	}
	return s.completeErr
}

func (s *deferredFakeStore) Expire(_ context.Context, _ time.Time, partition string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireCalls = append(s.expireCalls, partition)
	return s.expireCount, nil
}

func (s *deferredFakeStore) expiredPartitions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.expireCalls...)
}

func (s *deferredFakeStore) QueryPending(context.Context, string, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

func (s *deferredFakeStore) Release(_ context.Context, ids []string, _ persistence.LeaseToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.releaseErr != nil {
		// A failed Release leaves the record claimed; do not record it as
		// released. Mirrors the real stores' fail-closed behavior (M4).
		return s.releaseErr
	}
	s.released = append(s.released, ids...)
	return nil
}

func (s *deferredFakeStore) releasedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.released...)
}

func (s *deferredFakeStore) completedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.completed...)
}

// fnSender adapts a func to ports.Sender.
type fnSender struct {
	send func(ctx context.Context, msg ports.OutboundMessage) error
}

func (f *fnSender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	return f.send(ctx, msg)
}

func deferredTestRecord(t *testing.T, id, orderingKey string) *persistence.OutboxRecord {
	t.Helper()
	rec, err := persistence.NewOutboxRecord(persistence.OutboxSpec{
		ID:         id,
		RouteID:    "r1",
		EnvelopeID: "env-" + id,
		BindingID:  "b1",
		SessionID:  "sess-deferred",
		Address:    "test/topic",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:          "env-" + id,
			Subject:     "s",
			Payload:     []byte(`{}`),
			OrderingKey: orderingKey,
		}),
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("NewOutboxRecord(%s): %v", id, err)
	}
	return rec
}

func deferredTestToken() persistence.LeaseToken {
	return persistence.LeaseToken{Owner: "owner-1", Version: 1}
}

// TestDrainBatch_CtxCancelMidGroup_ReleasesUnattemptedTailAsDeferred: one
// ordering group of three records; the caller ctx is cancelled while record 1
// is in-flight. Record 1 completes (success=1); records 2 and 3 were claimed
// but never attempted, so they must be Released and reported as deferred —
// not counted successes, not stranded claimed.
func TestDrainBatch_CtxCancelMidGroup_ReleasesUnattemptedTailAsDeferred(t *testing.T) {
	rec1 := deferredTestRecord(t, "rec-1", "k1")
	rec2 := deferredTestRecord(t, "rec-2", "k1")
	rec3 := deferredTestRecord(t, "rec-3", "k1")
	store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{rec1, rec2, rec3}}

	sendStarted := make(chan struct{})
	releaseSend := make(chan struct{})
	sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error {
		close(sendStarted)
		<-releaseSend
		return nil
	}}

	d := New(Config{
		OutboxStore:  store,
		Sender:       sender,
		RouteID:      "r1",
		PartitionKey: "SESSION#sess-deferred",
		Policy:       routing.RoutePolicy{SendTimeout: 5 * time.Second, MaxReplayAttempts: 5},
		TokenFn:      func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		success, deferred int
		err               error
	}
	resCh := make(chan result, 1)
	go func() {
		s, def, err := d.drainBatch(ctx, deferredTestToken())
		resCh <- result{s, def, err}
	}()

	<-sendStarted // record 1 in-flight
	cancel()      // caller ctx dies before releaseSend: tail must defer
	close(releaseSend)

	res := <-resCh
	if res.err != nil {
		t.Fatalf("drainBatch error: %v", res.err)
	}
	if res.success != 1 {
		t.Fatalf("finding 9: success count got %d, want 1 (only rec-1 was sent+completed)", res.success)
	}
	if res.deferred != 2 {
		t.Fatalf("finding 9: deferred count got %d, want 2 (unattempted tail must not count as success)", res.deferred)
	}
	if got := store.completedIDs(); len(got) != 1 || got[0] != "rec-1" {
		t.Fatalf("Complete calls got %v, want [rec-1]", got)
	}
	got := store.releasedIDs()
	if len(got) != 2 || got[0] != "rec-2" || got[1] != "rec-3" {
		t.Fatalf("finding 9: unattempted tail must be Released in order, got %v, want [rec-2 rec-3]", got)
	}
}

// TestDrainBatch_BatchCancelMidSend_ReleasesAbortedRecordAsDeferred: two
// singleton groups drain concurrently. Group A's Complete reports a stale
// fencing token, which cancels the shared batch ctx; group B's in-flight send
// is aborted by that cancellation. B was never delivered, so processRecord
// must Release it (errBatchDeadlineDeferred path) and the batch must count it
// deferred — the pre-fix behavior returned nil and counted it a success.
func TestDrainBatch_BatchCancelMidSend_ReleasesAbortedRecordAsDeferred(t *testing.T) {
	recA := deferredTestRecord(t, "rec-a", "ka")
	recB := deferredTestRecord(t, "rec-b", "kb")

	bInSend := make(chan struct{})
	store := &deferredFakeStore{
		claimable:    []*persistence.OutboxRecord{recA, recB},
		completeGate: bInSend, // A's Complete stalls until B's send is in-flight
		completeErr:  shared.ErrStaleFencingToken,
	}

	sender := &fnSender{send: func(ctx context.Context, msg ports.OutboundMessage) error {
		if msg.Envelope.ID() == "env-rec-a" {
			return nil // A sends instantly; its Complete then reports stale
		}
		close(bInSend) // B in-flight: unblock A's Complete
		<-ctx.Done()   // aborted by the batch cancel that stale triggers
		return ctx.Err()
	}}

	d := New(Config{
		OutboxStore:         store,
		Sender:              sender,
		RouteID:             "r1",
		PartitionKey:        "SESSION#sess-deferred",
		Policy:              routing.RoutePolicy{SendTimeout: 5 * time.Second, MaxReplayAttempts: 5},
		DrainMaxConcurrency: 2,
		TokenFn:             func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})

	success, deferred, err := d.drainBatch(context.Background(), deferredTestToken())
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("drainBatch error got %v, want ErrStaleFencingToken", err)
	}
	if success != 0 {
		t.Fatalf("finding 9: success count got %d, want 0 (aborted send must not count as success)", success)
	}
	if deferred != 1 {
		t.Fatalf("finding 9: deferred count got %d, want 1 (mid-send abort must be deferred)", deferred)
	}
	got := store.releasedIDs()
	if len(got) != 1 || got[0] != "rec-b" {
		t.Fatalf("finding 9: aborted record must be Released back to pending, got %v, want [rec-b]", got)
	}
}
