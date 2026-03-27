package runtime_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

func makeDrainer(t *testing.T, token domain.LeaseToken, opts ...func(*goruntime.OutboxDrainerConfig)) (*FakeOutboxStore, *FakeSender, *FakeDLQStore, *goruntime.OutboxDrainer) {
	t.Helper()
	outbox := NewFakeOutboxStore()
	sender := NewFakeSender()
	dlqStore := NewFakeDLQStore()
	leaseStore := NewFakeLeaseStore()

	pk := domain.OutboxPartitionKey("sess-1", "")
	_, _ = leaseStore.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second)

	cfg := goruntime.OutboxDrainerConfig{
		OutboxStore:    outbox,
		LeaseStore:     leaseStore,
		Sender:         sender,
		DLQ:            goruntime.NewDLQRouter(dlqStore),
		RouteID:        "route-1",
		PartitionKey:   pk,
		LeaseID:        "sess-1",
		OwnerID:        token.Owner,
		Policy:         domain.RoutePolicy{}.WithDefaults(),
		Strategy:       domain.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize: 100,
		TokenFn: func() (domain.LeaseToken, bool) {
			return token, true
		},
	}
	for _, o := range opts {
		o(&cfg)
	}
	drainer := goruntime.NewOutboxDrainerFromConfig(cfg)
	return outbox, sender, dlqStore, drainer
}

// TestOutboxDrainer_HappyPath verifies a pending outbox record is sent and marked completed.
func TestOutboxDrainer_HappyPath(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, _, drainer := makeDrainer(t, token)

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID:         "rec-1",
		RouteID:    "route-1",
		EnvelopeID: "env-1",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope:   domain.Envelope{ID: "env-1", Payload: []byte("data")},
		Status:     domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

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
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, dlqStore, drainer := makeDrainer(t, token)

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID:         "rec-exp",
		RouteID:    "route-1",
		EnvelopeID: "env-exp",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope:   domain.Envelope{ID: "env-exp", ExpiresAt: time.Now().Add(-time.Second)},
		Status:     domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

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
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, dlqStore, drainer := makeDrainer(t, token, func(cfg *goruntime.OutboxDrainerConfig) {
		cfg.Policy.MaxReplayAttempts = 2
	})

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID:          "rec-poison",
		RouteID:     "route-1",
		EnvelopeID:  "env-poison",
		BindingID:   "bind-1",
		SessionID:   "sess-1",
		Envelope:    domain.Envelope{ID: "env-poison", Payload: []byte("bad")},
		Status:      domain.OutboxPending,
		ReplayCount: 3,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

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
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, _, drainer := makeDrainer(t, token)
	outbox.SetClaimErr(domain.ErrStaleFencingToken)

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID:         "rec-stale",
		RouteID:    "route-1",
		EnvelopeID: "env-stale",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope:   domain.Envelope{ID: "env-stale"},
		Status:     domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

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

	cfg := goruntime.OutboxDrainerConfig{
		OutboxStore:  outbox,
		Sender:       sender,
		DLQ:          goruntime.NewDLQRouter(dlqStore),
		RouteID:      "route-1",
		PartitionKey: domain.OutboxPartitionKey("sess-1", ""),
		OwnerID:      "bridge-1",
		Policy:       domain.RoutePolicy{}.WithDefaults(),
		Strategy:     domain.NewFixedPoll(50 * time.Millisecond),
		TokenFn: func() (domain.LeaseToken, bool) {
			return domain.LeaseToken{}, false
		},
	}
	drainer := goruntime.NewOutboxDrainerFromConfig(cfg)

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID: "rec-nolease", RouteID: "route-1", EnvelopeID: "env-nolease",
		BindingID: "bind-1", SessionID: "sess-1",
		Envelope: domain.Envelope{ID: "env-nolease"}, Status: domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 0 {
		t.Fatal("should not send when lease is not held")
	}
}

// TestOutboxDrainer_AppliesAddress verifies the record address overrides the envelope subject on send.
func TestOutboxDrainer_AppliesAddress(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, _, drainer := makeDrainer(t, token)

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID:         "rec-addr",
		RouteID:    "route-1",
		EnvelopeID: "env-addr",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Address:    "factory/a/orders/42",
		Envelope:   domain.Envelope{ID: "env-addr", Subject: "original-subject", Payload: []byte("data")},
		Status:     domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent, got %d", sender.SentCount())
	}
	if sender.Sent[0].Subject != "factory/a/orders/42" {
		t.Fatalf("expected subject from outbox record address, got %q", sender.Sent[0].Subject)
	}
}

// TestOutboxDrainer_EmptyAddressPreservesSubject verifies an empty record address keeps the original subject.
func TestOutboxDrainer_EmptyAddressPreservesSubject(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, _, drainer := makeDrainer(t, token)

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID:         "rec-noaddr",
		RouteID:    "route-1",
		EnvelopeID: "env-noaddr",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Address:    "",
		Envelope:   domain.Envelope{ID: "env-noaddr", Subject: "original", Payload: []byte("data")},
		Status:     domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent, got %d", sender.SentCount())
	}
	if sender.Sent[0].Subject != "original" {
		t.Fatalf("empty address should preserve original subject, got %q", sender.Sent[0].Subject)
	}
}

// TestOutboxDrainer_PermanentSendError verifies permanent send failure produces a DLQ entry.
func TestOutboxDrainer_PermanentSendError(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, dlqStore, drainer := makeDrainer(t, token)
	sender.SendErr = domain.ErrNotAuthorized

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID: "rec-perm", RouteID: "route-1", EnvelopeID: "env-perm",
		BindingID: "bind-1", SessionID: "sess-1",
		Envelope: domain.Envelope{ID: "env-perm"}, Status: domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

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
//	Sender blocks until context is cancelled after first send
//	Run must return within the timeout guard (2s) or the test fails
//
// ───────────────────────────────────────────────────────────────────────
//
// Assertions:
//   - Run returns before the timeout guard
//   - Fewer than 50 records are sent (batch was interrupted)
func TestOutboxDrainer_CancelDuringBatch_ReturnsPromptly(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}

	var sendCount int32
	cancelOnce := sync.Once{}
	var cancelFn context.CancelFunc

	_, _, _, drainer := makeDrainer(t, token, func(cfg *goruntime.OutboxDrainerConfig) {
		cfg.DrainMaxConcurrency = 1
		cfg.DrainBatchSize = 50
		cfg.DrainTimeout = 500 * time.Millisecond

		outbox := NewFakeOutboxStore()
		ctx := context.Background()
		for i := 0; i < 50; i++ {
			rec := domain.OutboxRecord{
				ID:         fmt.Sprintf("rec-%d", i),
				RouteID:    "route-1",
				EnvelopeID: fmt.Sprintf("env-%d", i),
				BindingID:  "bind-1",
				SessionID:  "sess-1",
				Envelope:   domain.Envelope{ID: fmt.Sprintf("env-%d", i), Payload: []byte("data")},
				Status:     domain.OutboxPending,
			}
			_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})
		}
		cfg.OutboxStore = outbox

		sender := NewFakeSender()
		sender.SendFn = func(_ *domain.Envelope) error {
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
	if sent >= 50 {
		t.Fatalf("expected fewer than 50 sends (batch interrupted), got %d", sent)
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
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, _, _, drainer := makeDrainer(t, token, func(cfg *goruntime.OutboxDrainerConfig) {
		cfg.Strategy = domain.NewFixedPoll(10 * time.Millisecond)
		cfg.DrainTimeout = 500 * time.Millisecond
	})

	ctx := context.Background()
	for i := 0; i < 10; i++ {
		rec := domain.OutboxRecord{
			ID:         fmt.Sprintf("rec-pre-%d", i),
			RouteID:    "route-1",
			EnvelopeID: fmt.Sprintf("env-pre-%d", i),
			BindingID:  "bind-1",
			SessionID:  "sess-1",
			Envelope:   domain.Envelope{ID: fmt.Sprintf("env-pre-%d", i), Payload: []byte("x")},
			Status:     domain.OutboxPending,
		}
		_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})
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
//	Sender introduces a small delay to ensure concurrent processing
//	Context is cancelled after a few sends
//
// ───────────────────────────────────────────────────────────────────────
//
// Assertions:
//   - Run returns before the timeout guard (no semaphore imbalance hang)
//   - Some but not all records are sent
func TestOutboxDrainer_ConcurrentBatch_SemaphoreConsistency(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}

	var sendCount int32
	cancelOnce := sync.Once{}
	var cancelFn context.CancelFunc

	_, _, _, drainer := makeDrainer(t, token, func(cfg *goruntime.OutboxDrainerConfig) {
		cfg.DrainMaxConcurrency = 3
		cfg.DrainBatchSize = 20
		cfg.DrainTimeout = 500 * time.Millisecond

		outbox := NewFakeOutboxStore()
		ctx := context.Background()
		for i := 0; i < 20; i++ {
			rec := domain.OutboxRecord{
				ID:         fmt.Sprintf("rec-c-%d", i),
				RouteID:    "route-1",
				EnvelopeID: fmt.Sprintf("env-c-%d", i),
				BindingID:  "bind-1",
				SessionID:  "sess-1",
				Envelope:   domain.Envelope{ID: fmt.Sprintf("env-c-%d", i), Payload: []byte("data")},
				Status:     domain.OutboxPending,
			}
			_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})
		}
		cfg.OutboxStore = outbox

		sender := NewFakeSender()
		sender.SendFn = func(_ *domain.Envelope) error {
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
	if sent >= 20 {
		t.Fatalf("expected fewer than 20 sends (batch interrupted), got %d", sent)
	}
	if sent == 0 {
		t.Fatal("expected at least some sends before cancellation")
	}
}
