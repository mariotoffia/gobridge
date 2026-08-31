package outbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// An ordering group is delivered by one goroutine, sequentially, and stops at
// the first record that does not reach a terminal state — otherwise a younger
// same-key record overtakes an older one. Stopping is only half the job: the
// records BEHIND the failure were claimed this cycle and never attempted, so
// they must go back to pending. Leaving them Claimed hides them from the
// pending-depth gauge, makes them unreclaimable until the fencing version moves
// or the stale window elapses (never, on the version-only in-memory store), and
// charges each recovery cycle a replay attempt.
//
// The deadline and stale-fence branches already release the tail; this pins the
// remaining branch — an unclassified per-record store error — which counted a
// failure and returned, stranding the whole group.

// TestDrainBatch_CompleteStoreError_ReleasesUnattemptedGroupTail drives the
// post-send Complete failure: the head was sent, so it stays claimed for a
// re-drain (the accepted at-least-once duplicate), but the tail was never
// attempted and must be released.
func TestDrainBatch_CompleteStoreError_ReleasesUnattemptedGroupTail(t *testing.T) {
	head := deferredTestRecord(t, "grp-head", "k-tail")
	tail1 := deferredTestRecord(t, "grp-tail-1", "k-tail")
	tail2 := deferredTestRecord(t, "grp-tail-2", "k-tail")
	store := &deferredFakeStore{
		claimable:   []*persistence.OutboxRecord{head, tail1, tail2},
		completeErr: errors.New("store: complete failed (simulated)"),
	}

	sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }}
	d := New(Config{
		OutboxStore:  store,
		Sender:       sender,
		RouteID:      "r1",
		PartitionKey: "SESSION#sess-deferred",
		Policy:       routing.RoutePolicy{SendTimeout: 5 * time.Second, MaxReplayAttempts: 5},
		TokenFn:      func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})

	success, _, err := d.drainBatch(context.Background(), deferredTestToken())
	if err != nil {
		t.Fatalf("drainBatch: %v", err)
	}
	if success != 0 {
		t.Fatalf("a record whose Complete failed is not a success, got %d", success)
	}
	got := store.releasedIDs()
	if len(got) != 2 || got[0] != "grp-tail-1" || got[1] != "grp-tail-2" {
		t.Fatalf("unattempted group tail must be released in order, got %v, want [grp-tail-1 grp-tail-2]", got)
	}
}

// TestDrainBatch_DLQWriteError_ReleasesUnattemptedGroupTail drives the other
// route into the same branch: a permanent send failure whose DLQ write also
// fails. The head keeps its claim (no durable evidence was written, so it must
// be retried), and the tail — never sent, never evidenced — goes back to
// pending.
func TestDrainBatch_DLQWriteError_ReleasesUnattemptedGroupTail(t *testing.T) {
	head := deferredTestRecord(t, "dlq-head", "k-dlq")
	tail := deferredTestRecord(t, "dlq-tail", "k-dlq")
	store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{head, tail}}

	sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error {
		return permanentSendError()
	}}
	d := New(Config{
		OutboxStore:  store,
		Sender:       sender,
		DLQ:          failingDLQRouter(),
		RouteID:      "r1",
		PartitionKey: "SESSION#sess-deferred",
		Policy: routing.RoutePolicy{
			SendTimeout:        5 * time.Second,
			MaxReplayAttempts:  5,
			OnPermanentFailure: routing.FailureDLQ,
		},
		TokenFn: func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})

	success, _, err := d.drainBatch(context.Background(), deferredTestToken())
	if err != nil {
		t.Fatalf("drainBatch: %v", err)
	}
	if success != 0 {
		t.Fatalf("a record whose DLQ write failed is not a success, got %d", success)
	}
	got := store.releasedIDs()
	if len(got) != 1 || got[0] != "dlq-tail" {
		t.Fatalf("unattempted group tail must be released, got %v, want [dlq-tail]", got)
	}
	if completed := store.completedIDs(); len(completed) != 0 {
		t.Fatalf("no record may be completed when the DLQ write failed, got %v", completed)
	}
}

// TestDrainBatch_SingletonGroupError_ReleasesNothing proves the release is
// scoped to the group's unattempted tail: a keyless record fails alone, so
// there is nothing behind it and no Release is issued. Releasing the failed
// head itself would drop the "sent but not completed" evidence the re-drain
// relies on.
func TestDrainBatch_SingletonGroupError_ReleasesNothing(t *testing.T) {
	only := deferredTestRecord(t, "solo-1", "")
	store := &deferredFakeStore{
		claimable:   []*persistence.OutboxRecord{only},
		completeErr: errors.New("store: complete failed (simulated)"),
	}

	sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }}
	d := New(Config{
		OutboxStore:  store,
		Sender:       sender,
		RouteID:      "r1",
		PartitionKey: "SESSION#sess-deferred",
		Policy:       routing.RoutePolicy{SendTimeout: 5 * time.Second, MaxReplayAttempts: 5},
		TokenFn:      func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})

	if _, _, err := d.drainBatch(context.Background(), deferredTestToken()); err != nil {
		t.Fatalf("drainBatch: %v", err)
	}
	if got := store.releasedIDs(); len(got) != 0 {
		t.Fatalf("a failed singleton has no tail to release, got %v", got)
	}
}

// permanentSendError is a non-transient send failure, which is what routes a
// record to the DLQ instead of a retry.
func permanentSendError() error {
	return shared.NewBridgeError("DENIED", shared.ErrorPermanent, "not authorized")
}

// failingDLQRouter is a router whose store always refuses the write, on a
// single attempt so the test never waits on a retry backoff. HasStore() is
// true, so the drainer takes the DLQ path rather than the drop path.
func failingDLQRouter() *dlq.Router {
	return dlq.NewFromConfig(dlq.Config{
		Store:            &fakeDLQStore{writeErr: errors.New("dlq: store unavailable (simulated)")},
		WriteMaxAttempts: 1,
	})
}
