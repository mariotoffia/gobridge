package runtime_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	outboxpkg "github.com/mariotoffia/gobridge/runtime/outbox"
)

// ═══════════════════════════════════════════════════════════════════════
// Shared-Outbox Transient Egress-Failure Recovery (Scenario #3 unit)
//
// The broker-crash case is exercised end-to-end in
// tests/longrunning/gap_broker_crash_test.go with a real Mosquitto
// container. This file complements that with a deterministic, no-Docker,
// no-sleep test of the core shared_outbox contract under mid-flight egress
// failure, driven entirely by an injected fake clock:
//
//   1. Records are persisted to the outbox as pending. In shared_outbox
//      this is the post-ack state — the source is acknowledged the moment
//      the record is durably persisted, decoupling delivery from egress
//      health.
//   2. The drainer claims the records and invokes the egress sender.
//   3. The sender fails with a transient BridgeError (broker disconnect).
//   4. NO record is completed; instead each is RELEASED back to pending
//      via the OutboxReleaser fast path, so the same live owner can
//      re-claim it on the next drain — no fencing-version bump and no
//      wall-clock stale-claim wait.
//   5. The sender recovers (returns nil); the next drain re-claims the
//      now-pending records, sends them, and completes them.
//
//   outbox(Pending) ──claim──▶ drainer.Send ──✗ transient ──▶ Release
//        ▲                                                       │
//        └───────────────── back to Pending ◀───────────────────┘
//        │
//      claim ──▶ drainer.Send ──✓──▶ Complete
//
//  ┌──────┬───────────────────────────────────────────────────┬───────┐
//  │ ID   │ Description                                        │ Type  │
//  ├──────┼───────────────────────────────────────────────────┼───────┤
//  │ TR1  │ Transient send → record released, retry completes  │ unit  │
//  │ TR2  │ Completion count bounded (no duplicate Complete)   │ unit  │
//  └──────┴───────────────────────────────────────────────────┴───────┘
//
// Recovery here is driven by the Release fast path, NOT by a wall-clock
// stale-claim timeout: the drain loop is stepped one cycle at a time with
// fake.Advance, so the sender down→up transition is sequenced exactly
// instead of raced against real time.
// ═══════════════════════════════════════════════════════════════════════

// TestSharedOutbox_TransientSenderFailure_RecoversOnRetry validates:
// the egress sender fails transiently (simulating a broker disconnect),
// the drainer releases each claimed record back to pending instead of
// completing it, and after the sender recovers the very next drain
// re-claims, sends, and completes all records — with the same owner at
// the same fencing version. No Docker, no sleeps, no wall-clock.
func TestSharedOutbox_TransientSenderFailure_RecoversOnRetry(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	const (
		interval  = 1 * time.Second
		ownerID   = "bridge-transient"
		sessionID = "mqtt-sess-transient"
		msgCount  = 3
	)
	token := persistence.LeaseToken{Version: 1, Owner: ownerID}
	pk := persistence.OutboxPartitionKey(sessionID, "")

	outbox := NewFakeOutboxStore()
	leaseStore := NewFakeLeaseStore()
	dlqStore := NewFakeDLQStore()

	if _, err := leaseStore.Acquire(context.Background(), sessionID, ownerID, 30*time.Second, nil); err != nil {
		t.Fatalf("acquire lease: %v", err)
	}

	// Persist msgCount records as pending (the post-ack outbox state).
	for i := 0; i < msgCount; i++ {
		rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
			ID:         "rec-" + envID(i),
			RouteID:    "transient-route",
			EnvelopeID: envID(i),
			BindingID:  "b1",
			SessionID:  sessionID,
			Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: envID(i), Payload: []byte("payload")}),
			Status:     persistence.OutboxPending,
		})
		if err := outbox.Persist(context.Background(), []*persistence.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist %d: %v", i, err)
		}
	}

	var senderUp atomic.Bool
	var sendAttempts atomic.Int64
	var (
		mu       sync.Mutex
		attempts []string // order-preserving list of envelope IDs attempted
	)
	sender := NewFakeSender()
	sender.SendFn = func(env *messaging.Envelope) error {
		sendAttempts.Add(1)
		mu.Lock()
		attempts = append(attempts, env.ID())
		mu.Unlock()
		if !senderUp.Load() {
			return shared.NewBridgeError(
				"BROKER_DISCONNECTED",
				shared.ErrorTransient,
				"simulated broker disconnect",
			)
		}
		return nil
	}

	batchCh := make(chan int, 8)
	drainer := outboxpkg.New(outboxpkg.Config{
		OutboxStore:  outbox,
		LeaseStore:   leaseStore,
		Sender:       sender,
		DLQ:          dlq.New(dlqStore),
		RouteID:      "transient-route",
		PartitionKey: pk,
		LeaseID:      sessionID,
		// Default replay cap: the release fast path plus the drainer's
		// transient backoff floor mean each record is re-claimed only once
		// per recovery cycle (replay_count tops out at 2 here), so the
		// default cap of DefaultMaxReplayAttempts is never approached.
		Policy:              routing.RoutePolicy{}.WithDefaults(),
		Strategy:            fixedNoJitterStrategy{d: interval},
		DrainBatchSize:      50,
		DrainMaxBatchSize:   50,
		DrainMaxConcurrency: 4,
		MaxDrainTimeout:     5 * time.Second,
		Clock:               fake,
		TokenFn:             func() (persistence.LeaseToken, bool) { return token, true },
		OnBatchComplete:     func(n int) { batchCh <- n },
	})

	runCtx, cancel := context.WithCancel(context.Background())
	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() {
		defer runWG.Done()
		_ = drainer.Run(runCtx)
	}()
	defer func() {
		cancel()
		runWG.Wait()
	}()

	// ── Drain #1 — sender is DOWN ─────────────────────────────────────────
	waitForFakeTimers(t, fake, 1)
	fake.Advance(interval)
	require.Zero(t, <-batchCh,
		"a batch of purely transient-released records must report 0 successes "+
			"(releases must not be counted as successful drains)")

	// Every record was attempted, but the transient failure must complete
	// or DLQ nothing.
	require.GreaterOrEqual(t, sendAttempts.Load(), int64(msgCount),
		"drainer must attempt each record while the sender is down")
	require.Equal(t, 0, outbox.CompletedCount(),
		"no record may be completed while the sender is transient-failing")
	require.Zero(t, dlqStore.Count(),
		"transient failures must not route to DLQ")

	// each record was RELEASED back to pending (not left claimed), so
	// the same owner re-claims it on the next drain — the whole point.
	for _, rec := range outbox.Records() {
		require.Equalf(t, persistence.OutboxPending, rec.Status(),
			"record %s must be released back to pending after a transient failure", rec.ID())
	}

	// ── Sender recovers; Drain #2 ─────────────────────────────────────────
	// After a transient drain the loop applies its retry backoff floor (5s)
	// instead of the 1s poll interval, so a down broker is not hammered and
	// the replay/poison budget is not burned. Advance past that floor to
	// fire the recovery drain.
	const transientRetryFloor = 5 * time.Second
	senderUp.Store(true)
	waitForFakeTimers(t, fake, 1)
	fake.Advance(transientRetryFloor)
	<-batchCh

	// All records sent AND completed; still no DLQ. TR2: CompletedCount is
	// exactly msgCount — each record completed once, no duplicate Complete.
	require.Equal(t, msgCount, outbox.CompletedCount(),
		"exactly msgCount records must be completed after recovery")
	require.Equal(t, msgCount, sender.SentCount(),
		"each record must be successfully sent exactly once after recovery")
	require.Zero(t, dlqStore.Count(),
		"transient recovery must not route to DLQ")

	// Each envelope was attempted at least twice: once while down, once
	// after recovery.
	mu.Lock()
	totalAttempts := len(attempts)
	mu.Unlock()
	assert.GreaterOrEqual(t, totalAttempts, 2*msgCount,
		"expected at least one retry per record after recovery")
}

// envID returns a deterministic envelope ID for the given index.
func envID(i int) string {
	return "transient-msg-" + string(rune('a'+i))
}

// TestSharedOutbox_OrderingGroup_TransientFailure_NoOvertake is the
// ordering-overtake regression (adversarial review finding 1). Records A and
// B share one ordering key, so they form a single ordering group processed
// in persisted order A→B. The egress sender fails A transiently on its first
// attempt and succeeds on retry; B always succeeds. The drainer MUST stop
// the ordering group the moment A is released for retry, so B is never sent
// or completed ahead of A.
//
//	┌──────────── ordering group (key=ORDER-KEY-1) ────────────┐
//	│  A (msg-a)  ──send ✗ transient──▶ Release A ──▶ STOP group│
//	│  B (msg-b)  ──── never attempted this cycle ───▶ Release B│
//	└──────────────────────────────────────────────────────────┘
//	          next cycle re-claims [A,B] in order ─▶ A ✓ then B ✓
//
// Invariant: B must never reach Completed (or even be sent) while A is still
// pending/unsent. Against the pre-fix code — processRecord returned nil on
// the transient path, so the group loop continued to B — B overtakes A: it
// is sent and completed in the very cycle that A failed. This test fails
// there and passes once the errReleasedForRetry sentinel stops the group.
func TestSharedOutbox_OrderingGroup_TransientFailure_NoOvertake(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	const (
		interval    = 1 * time.Second
		floor       = 5 * time.Second // matches the drainer's transientRetryFloor
		ownerID     = "bridge-ordering"
		sessionID   = "mqtt-sess-ordering"
		orderingKey = "ORDER-KEY-1"
		idA         = "msg-a"
		idB         = "msg-b"
	)
	recA, recB := "rec-"+idA, "rec-"+idB
	token := persistence.LeaseToken{Version: 1, Owner: ownerID}
	pk := persistence.OutboxPartitionKey(sessionID, "")

	outbox := NewFakeOutboxStore()
	leaseStore := NewFakeLeaseStore()
	dlqStore := NewFakeDLQStore()

	if _, err := leaseStore.Acquire(context.Background(), sessionID, ownerID, 30*time.Second, nil); err != nil {
		t.Fatalf("acquire lease: %v", err)
	}

	base := fake.Now()
	persist := func(id string, createdAt time.Time) {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:          id,
			Payload:     []byte("payload"),
			OrderingKey: orderingKey,
		})
		rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
			ID:         "rec-" + id,
			RouteID:    "ordering-route",
			EnvelopeID: id,
			BindingID:  "b1",
			SessionID:  sessionID,
			Envelope:   *env,
			Status:     persistence.OutboxPending,
			CreatedAt:  createdAt,
		})
		if err := outbox.Persist(context.Background(), []*persistence.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist %s: %v", id, err)
		}
	}
	// A is strictly earlier than B, so the deterministic claim order is A→B.
	persist(idA, base)
	persist(idB, base.Add(time.Millisecond))

	var (
		mu      sync.Mutex
		sendLog []string
		aSeen   int
	)
	sender := NewFakeSender()
	sender.SendFn = func(env *messaging.Envelope) error {
		mu.Lock()
		defer mu.Unlock()
		sendLog = append(sendLog, env.ID())
		if env.ID() == idA {
			aSeen++
			if aSeen == 1 {
				return shared.NewBridgeError(
					"BROKER_DISCONNECTED",
					shared.ErrorTransient,
					"simulated transient failure on first attempt of A",
				)
			}
		}
		return nil
	}

	batchCh := make(chan int, 8)
	drainer := outboxpkg.New(outboxpkg.Config{
		OutboxStore:         outbox,
		LeaseStore:          leaseStore,
		Sender:              sender,
		DLQ:                 dlq.New(dlqStore),
		RouteID:             "ordering-route",
		PartitionKey:        pk,
		LeaseID:             sessionID,
		Policy:              routing.RoutePolicy{}.WithDefaults(),
		Strategy:            fixedNoJitterStrategy{d: interval},
		DrainBatchSize:      50,
		DrainMaxBatchSize:   50,
		DrainMaxConcurrency: 4,
		MaxDrainTimeout:     5 * time.Second,
		Clock:               fake,
		TokenFn:             func() (persistence.LeaseToken, bool) { return token, true },
		OnBatchComplete:     func(n int) { batchCh <- n },
	})

	runCtx, cancel := context.WithCancel(context.Background())
	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() {
		defer runWG.Done()
		_ = drainer.Run(runCtx)
	}()
	defer func() {
		cancel()
		runWG.Wait()
	}()

	statusOf := func(id string) persistence.OutboxStatus {
		t.Helper()
		for _, rec := range outbox.Records() {
			if rec.ID() == id {
				return rec.Status()
			}
		}
		t.Fatalf("record %s not found", id)
		return ""
	}

	// ── Drain #1 — A fails transiently; the group must stop before B ──────
	waitForFakeTimers(t, fake, 1)
	fake.Advance(interval)
	n1 := <-batchCh // synchronize on drain #1 completion

	// The overtake invariant FIRST: B is neither sent nor completed while A
	// is still unsent; both records are back to pending for an in-order
	// retry. Against the pre-fix code the group loop continued past A, so B
	// was sent and completed here — these assertions are the regression.
	mu.Lock()
	log1 := append([]string(nil), sendLog...)
	mu.Unlock()
	require.Equal(t, []string{idA}, log1,
		"only A may be attempted in the failing cycle; B must not overtake it")
	require.Equal(t, persistence.OutboxPending, statusOf(recB),
		"B must be released back to pending (group stopped) so order is preserved")
	require.Equal(t, persistence.OutboxPending, statusOf(recA),
		"A must be released back to pending after its transient failure")
	require.Equal(t, 0, outbox.CompletedCount(),
		"no record may complete while A is transient-failing")
	require.Zero(t, dlqStore.Count(), "transient failure must not DLQ")
	// Releases must not be miscounted as successful drains.
	require.Zero(t, n1, "a transient-only drain must report 0 successes")

	// ── Drain #2 — retry past the backoff floor; A then B both complete ───
	waitForFakeTimers(t, fake, 1)
	fake.Advance(floor)
	require.Equal(t, 2, <-batchCh, "both records complete on the recovery drain")

	require.Equal(t, 2, outbox.CompletedCount(), "A and B both complete after recovery")
	require.Equal(t, persistence.OutboxCompleted, statusOf(recA))
	require.Equal(t, persistence.OutboxCompleted, statusOf(recB))

	// Final send order proves A was delivered (successfully) before B, never
	// the reverse: [A(fail), A(retry-ok), B(ok)].
	mu.Lock()
	finalLog := append([]string(nil), sendLog...)
	mu.Unlock()
	require.Equal(t, []string{idA, idA, idB}, finalLog,
		"delivery order must be A (fail), A (retry), then B — B never before A")
}
