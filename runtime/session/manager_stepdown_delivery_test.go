package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// fencedSink models the version-fenced Complete of the durable outbox store
// (adapters/*/store/*outbox): a record commits at most once, and a Complete
// carrying a lease version below the highest already-committed ("fenced")
// version is rejected as stale. This is the exact backstop that stops a
// stepped-down owner's in-flight delivery from double-committing a record the
// new owner already settled. The session Manager's only job in that dance is
// to hand every delivery a MONOTONIC fencing version (Token) behind an ATOMIC
// ownership gate (hasLease); this sink asserts the Manager upholds that.
type fencedSink struct {
	mu         sync.Mutex
	committed  map[string]uint64 // recordID -> lease version it committed under
	fence      uint64            // highest lease version that has committed
	doubleAcks int               // fence-defeating re-commits caught by the record guard
}

func newFencedSink() *fencedSink { return &fencedSink{committed: map[string]uint64{}} }

// complete settles recordID under lease version v. It returns true only when
// this call is the one that commits the record; a stale version (below the
// fence) or an already-settled record yields false without mutating state.
func (s *fencedSink) complete(recordID string, v uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, done := s.committed[recordID]; done {
		// The record is already committed. If this settlement is NOT below the
		// fence, only the record-level guard stopped a second commit — meaning
		// the owner handed out a non-monotonic fencing version. Flag it.
		if v >= s.fence {
			s.doubleAcks++
		}
		return false
	}
	if v < s.fence {
		return false // stale fencing token: a newer owner already advanced fence
	}
	s.committed[recordID] = v
	s.fence = v
	return true
}

func (s *fencedSink) committedVersion(recordID string) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.committed[recordID]
	return v, ok
}

func (s *fencedSink) doubleAckCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doubleAcks
}

// TestSessionManager_StepDownConcurrentWithInflightDelivery hardens the
// three-phase step-down against an outbox delivery that is IN FLIGHT across a
// failover (adversarial Probe 5 / C3-FU4). Prior coverage exercises the fakes
// and the single-use regression but never a delivery straddling the step-down.
//
// A delivery snapshots the owner's lease token — exactly as
// runtime/outbox/loop.go does via tokenFn — and then stalls mid-settle. The
// lease is lost: the Manager steps down (closing the source session, clearing
// the ownership gate, releasing the lease) and a fresh term re-acquires with a
// STRICTLY higher fencing version. The new term re-delivers and settles the
// same record. When the stalled in-flight delivery finally settles under the
// OLD version it must be fenced out, so the record is committed exactly once —
// no double-ack, no lost or duplicated settlement — and the Manager shuts down
// cleanly. Run with -race to also cover concurrent Token()/step-down access.
func TestSessionManager_StepDownConcurrentWithInflightDelivery(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	renewCh := make(chan struct{}, 8)
	// Succeed the first renew, then fail every subsequent one; with
	// MaxRenewFails=1 the second (failing) renew forces step-down, after which
	// Acquire grants the next version (v2).
	store := newLeaseLossStore(1, renewCh)
	sess := newCountingSession()
	sink := newFencedSink()

	const (
		recordID      = "outbox-record-R"
		renewInterval = 500 * time.Millisecond
	)
	cfg := Config{
		SessionID:     "sess-stepdown-delivery",
		Exclusive:     true,
		LeaseTTL:      5 * time.Second,
		RenewInterval: renewInterval,
		RenewJitter:   0,
		MaxRenewFails: 1,
		// Step-down grace/release run on the real clock (not the fake); keep it
		// small but comfortably non-zero so the in-flight delivery genuinely
		// overlaps the step-down window and the gate check below is not racy.
		StepDownGrace: 100 * time.Millisecond,
	}
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() {
		defer runWG.Done()
		_ = mgr.Run(ctx)
	}()
	defer func() {
		cancel()
		runWG.Wait()
		_ = mgr.Close(context.Background())
	}()

	// ── Term 1: acquire v1, then start an in-flight delivery under it ────
	wait.Until(t, 2*time.Second, "term-1 lease acquired", func() bool {
		tok, held := mgr.Token()
		return held && tok.Version == 1
	})
	tok1, held1 := mgr.Token()
	if !held1 || tok1.Version != 1 {
		t.Fatalf("expected held v1 token, got held=%v version=%d", held1, tok1.Version)
	}

	// The delivery has claimed recordID under v1 and is mid-settle (blocked on
	// inflightProceed) for the entire step-down + re-acquire, i.e. in flight.
	inflightProceed := make(chan struct{})
	inflightResult := make(chan bool, 1)
	go func() {
		<-inflightProceed
		inflightResult <- sink.complete(recordID, tok1.Version)
	}()

	// ── Force step-down: renew #1 succeeds, renew #2 fails ───────────────
	wait.RequireReceive(t, sess.reconciledCh, 2*time.Second)
	wait.RequireReceive(t, sess.eventsReadCh, 2*time.Second)
	wait.Until(t, 2*time.Second, "renew timer registered", func() bool {
		return fake.TimerCount() >= 1
	})
	fake.Advance(renewInterval)
	wait.RequireReceive(t, renewCh, 2*time.Second) // renew #1 (success)
	wait.Until(t, 2*time.Second, "renew timer reset", func() bool {
		return fake.TimerCount() >= 1
	})
	fake.Advance(renewInterval)
	wait.RequireReceive(t, renewCh, 2*time.Second) // renew #2 (fail -> step-down)

	// Step-down must close the source session (stop consuming during failover,
	// the split-brain guard) ...
	wait.RequireReceive(t, sess.closedCh, 3*time.Second)
	// ... and must clear the ownership gate so NEW deliveries stop claiming.
	// hasLease is cleared before Close, and re-acquire cannot happen until the
	// full StepDownGrace has elapsed, so this read is not racy.
	if _, held := mgr.Token(); held {
		t.Fatal("step-down must clear the ownership gate (hasLease) so new " +
			"deliveries stop claiming while a new owner takes over")
	}

	// ── Term 2: the Manager re-acquires with a STRICTLY higher fence ─────
	wait.Until(t, 5*time.Second, "term-2 lease re-acquired", func() bool {
		tok, held := mgr.Token()
		return held && tok.Version == 2
	})
	tok2, held2 := mgr.Token()
	if !held2 || tok2.Version != 2 {
		t.Fatalf("expected held v2 token after re-acquire, got held=%v version=%d", held2, tok2.Version)
	}
	if tok2.Version <= tok1.Version {
		t.Fatalf("fencing version must advance across step-down: v1=%d v2=%d",
			tok1.Version, tok2.Version)
	}

	// The new term re-claims and settles the same record under v2.
	if !sink.complete(recordID, tok2.Version) {
		t.Fatal("new term (v2) must settle the re-claimed record")
	}

	// ── The stalled in-flight delivery finally settles under stale v1 ────
	close(inflightProceed)
	if got := wait.RequireReceive(t, inflightResult, 2*time.Second); got {
		t.Fatal("in-flight delivery settled under the stale v1 token: double-ack " +
			"(the record was already committed by the new v2 owner)")
	}

	// ── Exactly-once settlement + no double-ack ──────────────────────────
	if v, ok := sink.committedVersion(recordID); !ok || v != tok2.Version {
		t.Fatalf("record must be committed exactly once under v2 (%d), got version=%d ok=%v",
			tok2.Version, v, ok)
	}
	if n := sink.doubleAckCount(); n != 0 {
		t.Fatalf("no record may be committed twice; got %d fence-defeating re-commits", n)
	}
}
