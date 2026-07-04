package runtime_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	outboxpkg "github.com/mariotoffia/gobridge/runtime/outbox"
)

func makeDrainer(t *testing.T, token persistence.LeaseToken, opts ...func(*outboxpkg.Config)) (*FakeOutboxStore, *FakeSender, *FakeDLQStore, *outboxpkg.Drainer) {
	t.Helper()
	outbox := NewFakeOutboxStore()
	sender := NewFakeSender()
	dlqStore := NewFakeDLQStore()
	leaseStore := NewFakeLeaseStore()

	pk := persistence.OutboxPartitionKey("sess-1", "")
	_, _ = leaseStore.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	cfg := outboxpkg.Config{
		OutboxStore:    outbox,
		LeaseStore:     leaseStore,
		Sender:         sender,
		DLQ:            dlq.New(dlqStore),
		RouteID:        "route-1",
		PartitionKey:   pk,
		LeaseID:        "sess-1",
		Policy:         routing.RoutePolicy{}.WithDefaults(),
		Strategy:       persistence.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize: 100,
		TokenFn: func() (persistence.LeaseToken, bool) {
			return token, true
		},
	}
	for _, o := range opts {
		o(&cfg)
	}
	drainer := outboxpkg.New(cfg)
	return outbox, sender, dlqStore, drainer
}

// TestOutboxDrainer_HappyPath verifies a pending outbox record is sent and marked completed.
func TestOutboxDrainer_HappyPath(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, _, drainer := makeDrainer(t, token)

	ctx := context.Background()
	rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID:         "rec-1",
		RouteID:    "route-1",
		EnvelopeID: "env-1",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-1", Payload: []byte("data")}),
		Status:     persistence.OutboxPending,
	})
	_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent, got %d", sender.SentCount())
	}
	if outbox.CompletedCount() != 1 {
		t.Fatalf("expected 1 completed, got %d", outbox.CompletedCount())
	}
}

// TestOutboxDrainer_ExpiredRecord verifies expired records skip send and are DLQed.
func TestOutboxDrainer_ExpiredRecord(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, dlqStore, drainer := makeDrainer(t, token)

	ctx := context.Background()
	rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID:         "rec-exp",
		RouteID:    "route-1",
		EnvelopeID: "env-exp",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope: func() messaging.Envelope {
			e := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-exp"})
			_ = e.SetExpiry(time.Now().Add(-time.Second))
			return *e
		}(),
		Status: persistence.OutboxPending,
	})
	_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 0 {
		t.Fatal("expired record should not be sent")
	}
	if dlqStore.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", dlqStore.Count())
	}
}

// TestOutboxDrainer_PoisonMessage verifies replay count above max sends to DLQ without sending.
func TestOutboxDrainer_PoisonMessage(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, dlqStore, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.Policy.MaxReplayAttempts = 2
	})

	ctx := context.Background()
	rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID:          "rec-poison",
		RouteID:     "route-1",
		EnvelopeID:  "env-poison",
		BindingID:   "bind-1",
		SessionID:   "sess-1",
		Envelope:    *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-poison", Payload: []byte("bad")}),
		Status:      persistence.OutboxPending,
		ReplayCount: 3,
		// Past the wall-clock ReplayBudget (15m default) so replay exhaustion
		// actually poisons; pre-set (non-zero) so the claim stamp-once keeps it.
		FirstAttemptedAt: time.Now().Add(-time.Hour),
	})
	_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 0 {
		t.Fatal("poison message should not be sent")
	}
	if dlqStore.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry for poison, got %d", dlqStore.Count())
	}
}

// TestOutboxDrainer_StaleFencingToken verifies the drainer handles stale
// fencing tokens gracefully by continuing to poll rather than crashing.
func TestOutboxDrainer_StaleFencingToken(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, _, drainer := makeDrainer(t, token)
	outbox.SetClaimErr(shared.ErrStaleFencingToken)

	ctx := context.Background()
	rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID:         "rec-stale",
		RouteID:    "route-1",
		EnvelopeID: "env-stale",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-stale"}),
		Status:     persistence.OutboxPending,
	})
	_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 0 {
		t.Fatal("should not send when fencing token is stale")
	}
}

// TestOutboxDrainer_NoLease verifies draining does not send when no lease token is available.
func TestOutboxDrainer_NoLease(t *testing.T) {
	outbox := NewFakeOutboxStore()
	sender := NewFakeSender()
	dlqStore := NewFakeDLQStore()

	cfg := outboxpkg.Config{
		OutboxStore:  outbox,
		Sender:       sender,
		DLQ:          dlq.New(dlqStore),
		RouteID:      "route-1",
		PartitionKey: persistence.OutboxPartitionKey("sess-1", ""),
		Policy:       routing.RoutePolicy{}.WithDefaults(),
		Strategy:     persistence.NewFixedPoll(50 * time.Millisecond),
		TokenFn: func() (persistence.LeaseToken, bool) {
			return persistence.LeaseToken{}, false
		},
	}
	drainer := outboxpkg.New(cfg)

	ctx := context.Background()
	rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID: "rec-nolease", RouteID: "route-1", EnvelopeID: "env-nolease",
		BindingID: "bind-1", SessionID: "sess-1",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-nolease"}), Status: persistence.OutboxPending,
	})
	_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 0 {
		t.Fatal("should not send when lease is not held")
	}
}

// TestOutboxDrainer_AppliesAddress verifies the record address travels via
// OutboundMessage.Address while the envelope's logical Subject is preserved.
func TestOutboxDrainer_AppliesAddress(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, _, drainer := makeDrainer(t, token)

	ctx := context.Background()
	rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID:         "rec-addr",
		RouteID:    "route-1",
		EnvelopeID: "env-addr",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Address:    "factory/a/orders/42",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-addr", Subject: "original-subject", Payload: []byte("data")}),
		Status:     persistence.OutboxPending,
	})
	_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent, got %d", sender.SentCount())
	}
	if sender.Sent[0].Subject() != "original-subject" {
		t.Fatalf("logical subject must be preserved on outbound envelope, got %q", sender.Sent[0].Subject())
	}
	outbound := sender.GetOutbound()
	if len(outbound) != 1 || outbound[0].Address != "factory/a/orders/42" {
		t.Fatalf("expected OutboundMessage.Address factory/a/orders/42, got %+v", outbound)
	}
	// And the persisted record must not have its logical Subject mutated.
	recs := outbox.Records()
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	if recs[0].Snapshot().Subject() != "original-subject" {
		t.Fatalf("persisted record envelope subject mutated: got %q", recs[0].Snapshot().Subject())
	}
	if recs[0].Address() != "factory/a/orders/42" {
		t.Fatalf("persisted record address = %q, want factory/a/orders/42", recs[0].Address())
	}
}

// TestOutboxDrainer_EmptyAddressPreservesSubject verifies an empty record address keeps the original subject.
func TestOutboxDrainer_EmptyAddressPreservesSubject(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, _, drainer := makeDrainer(t, token)

	ctx := context.Background()
	rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID:         "rec-noaddr",
		RouteID:    "route-1",
		EnvelopeID: "env-noaddr",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Address:    "",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-noaddr", Subject: "original", Payload: []byte("data")}),
		Status:     persistence.OutboxPending,
	})
	_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent, got %d", sender.SentCount())
	}
	if sender.Sent[0].Subject() != "original" {
		t.Fatalf("empty address should preserve original subject, got %q", sender.Sent[0].Subject())
	}
}

// TestOutboxDrainer_PermanentSendError verifies permanent send failure produces a DLQ entry.
func TestOutboxDrainer_PermanentSendError(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, dlqStore, drainer := makeDrainer(t, token)
	sender.SendErr = shared.ErrNotAuthorized

	ctx := context.Background()
	rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID: "rec-perm", RouteID: "route-1", EnvelopeID: "env-perm",
		BindingID: "bind-1", SessionID: "sess-1",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-perm"}), Status: persistence.OutboxPending,
	})
	_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if dlqStore.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry for permanent error, got %d", dlqStore.Count())
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// T1 Regression: break-in-select deadlock fix (labeled break loop)
//
// These tests verify that drainBatch exits promptly when the context is
// cancelled, without hanging on wg.Wait() due to semaphore slots that
// were never acquired.
//
// ┌──────────────────────────────────────────────────────────────────────┐
// │  Before fix:                                                       │
// │    select { case <-ctx.Done(): break }                             │
// │    ↓ falls through to wg.Add(1) + goroutine with <-sem release    │
// │    → hangs on wg.Wait() forever (deadlock)                        │
// │                                                                    │
// │  After fix:                                                        │
// │    select { case <-ctx.Done(): break loop }                        │
// │    ↓ exits for loop, proceeds directly to wg.Wait()               │
// │    → returns promptly                                              │
// └──────────────────────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestOutboxDrainer_CancelDuringBatch_ReturnsPromptly validates that
// drainBatch exits within a bounded time when the context is cancelled
// mid-batch, proving the labeled break prevents the deadlock.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────
//
//	50 records queued, maxConcurrency=1 (serial sem)
//	Sender cancels the run context after the second send
//	Run must return within the timeout guard (10s) or the test fails
//
// ───────────────────────────────────────────────────────────────────────
//
// Assertions:
//   - Run returns before the timeout guard (no deadlock/semaphore leak)
//   - At least one record is sent
//
// Note (findings 9 & 10): on cancellation the main-loop batch releases its
// unsent, claimed records back to pending instead of stranding them, and
// Run then performs a bounded finalDrain that flushes those released records
// during graceful shutdown. Consequently the total send count may reach the
// full batch — that is the correct no-loss behavior, not a bug. The subject
// of this test is prompt, deadlock-free return on cancellation, so we no
// longer assert an upper bound on the send count.
func TestOutboxDrainer_CancelDuringBatch_ReturnsPromptly(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}

	var sendCount int32
	cancelOnce := sync.Once{}
	var cancelFn context.CancelFunc

	_, _, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.DrainMaxConcurrency = 1
		cfg.DrainBatchSize = 50
		cfg.DrainTimeout = 500 * time.Millisecond

		outbox := NewFakeOutboxStore()
		ctx := context.Background()
		for i := 0; i < 50; i++ {
			rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
				ID:         fmt.Sprintf("rec-%d", i),
				RouteID:    "route-1",
				EnvelopeID: fmt.Sprintf("env-%d", i),
				BindingID:  "bind-1",
				SessionID:  "sess-1",
				Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: fmt.Sprintf("env-%d", i), Payload: []byte("data")}),
				Status:     persistence.OutboxPending,
			})
			_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})
		}
		cfg.OutboxStore = outbox

		sender := NewFakeSender()
		sender.SendFn = func(_ *messaging.Envelope) error {
			n := atomic.AddInt32(&sendCount, 1)
			if n >= 2 {
				cancelOnce.Do(func() {
					if cancelFn != nil {
						cancelFn()
					}
				})
			}
			return nil
		}
		cfg.Sender = sender
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancelFn = cancel
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = drainer.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return promptly after context cancellation — deadlock suspected")
	}

	sent := int(atomic.LoadInt32(&sendCount))
	if sent == 0 {
		t.Fatal("expected at least one send before cancellation")
	}
}

// TestOutboxDrainer_CancelBeforeBatch_ExitsPromptly validates that when the
// context is already cancelled before Run enters the main loop, Run exits
// promptly without hanging. Note: Run performs a finalDrain with a separate
// context whose timeout is configured via DrainTimeout. The key
// assertion is that Run returns in bounded time (no deadlock).
//
// Assertions:
//   - Run returns before the timeout guard (no deadlock)
func TestOutboxDrainer_CancelBeforeBatch_ExitsPromptly(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, _, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.Strategy = persistence.NewFixedPoll(10 * time.Millisecond)
		cfg.DrainTimeout = 500 * time.Millisecond
	})

	ctx := context.Background()
	for i := 0; i < 10; i++ {
		rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
			ID:         fmt.Sprintf("rec-pre-%d", i),
			RouteID:    "route-1",
			EnvelopeID: fmt.Sprintf("env-pre-%d", i),
			BindingID:  "bind-1",
			SessionID:  "sess-1",
			Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: fmt.Sprintf("env-pre-%d", i), Payload: []byte("x")}),
			Status:     persistence.OutboxPending,
		})
		_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})
	}

	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()

	done := make(chan struct{})
	go func() {
		_ = drainer.Run(cancelledCtx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after pre-cancelled context — deadlock suspected")
	}
}

// TestOutboxDrainer_ConcurrentBatch_SemaphoreConsistency validates that
// with maxConcurrency > 1, cancellation during a batch does not leak
// goroutines or cause semaphore imbalance (which would manifest as a hang).
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────
//
//	20 records, maxConcurrency=3
//	Context is cancelled after a few sends
//
// ───────────────────────────────────────────────────────────────────────
//
// Assertions:
//   - Run returns before the timeout guard (no semaphore imbalance hang)
//   - At least one record is sent
//
// Note (findings 9 & 10): as with CancelDuringBatch, released records are
// flushed by the bounded finalDrain on shutdown, so the total send count may
// reach the full batch. This test's subject is semaphore consistency and
// prompt, deadlock-free return — not a partial-send count.
func TestOutboxDrainer_ConcurrentBatch_SemaphoreConsistency(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}

	var sendCount int32
	cancelOnce := sync.Once{}
	var cancelFn context.CancelFunc

	_, _, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.DrainMaxConcurrency = 3
		cfg.DrainBatchSize = 20
		cfg.DrainTimeout = 500 * time.Millisecond

		outbox := NewFakeOutboxStore()
		ctx := context.Background()
		for i := 0; i < 20; i++ {
			rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
				ID:         fmt.Sprintf("rec-c-%d", i),
				RouteID:    "route-1",
				EnvelopeID: fmt.Sprintf("env-c-%d", i),
				BindingID:  "bind-1",
				SessionID:  "sess-1",
				Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: fmt.Sprintf("env-c-%d", i), Payload: []byte("data")}),
				Status:     persistence.OutboxPending,
			})
			_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})
		}
		cfg.OutboxStore = outbox

		sender := NewFakeSender()
		sender.SendFn = func(_ *messaging.Envelope) error {
			n := atomic.AddInt32(&sendCount, 1)
			if n >= 5 {
				cancelOnce.Do(func() {
					if cancelFn != nil {
						cancelFn()
					}
				})
			}
			return nil
		}
		cfg.Sender = sender
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancelFn = cancel
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = drainer.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return promptly — semaphore imbalance or deadlock suspected")
	}

	sent := int(atomic.LoadInt32(&sendCount))
	if sent == 0 {
		t.Fatal("expected at least some sends before cancellation")
	}
}
