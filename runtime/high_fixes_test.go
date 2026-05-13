package runtime_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	outboxpkg "github.com/mariotoffia/gobridge/runtime/outbox"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// ═══════════════════════════════════════════════════════════════════════
// T5: Send Timeout Tests
//
// Validates that RoutePolicy.SendTimeout is applied to sender.Send
// calls in both the outbox drainer and route runner, preventing
// indefinite blocking when a sender hangs.
//
// ┌──────────┐    SendTimeout    ┌──────────┐
// │  Drainer │───────────────────│  Sender  │
// │  /Runner │  ctx.WithTimeout  │  (hangs) │
// └──────────┘                   └──────────┘
// ═══════════════════════════════════════════════════════════════════════

// TestRoutePolicy_WithDefaults_SetsSendTimeout validates WithDefaults
// applies DefaultSendTimeout when SendTimeout is zero.
func TestRoutePolicy_WithDefaults_SetsSendTimeout(t *testing.T) {
	p := routing.RoutePolicy{}.WithDefaults()
	if p.SendTimeout != routing.DefaultSendTimeout {
		t.Fatalf("expected SendTimeout=%v, got %v", routing.DefaultSendTimeout, p.SendTimeout)
	}
}

// TestRoutePolicy_WithDefaults_PreservesExplicitSendTimeout validates
// an explicit SendTimeout is not overwritten by WithDefaults.
func TestRoutePolicy_WithDefaults_PreservesExplicitSendTimeout(t *testing.T) {
	p := routing.RoutePolicy{SendTimeout: 5 * time.Second}.WithDefaults()
	if p.SendTimeout != 5*time.Second {
		t.Fatalf("expected SendTimeout=5s, got %v", p.SendTimeout)
	}
}

// TestOutboxDrainer_SendTimeout validates that a hanging sender is
// cancelled by the per-operation send timeout.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	Sender blocks until ctx done → SendTimeout fires → record not completed
//
// ───────────────────────────────────────────────
//
// Test Parameters:
//   - SendTimeout: 100ms
//   - Sender: blocks until context cancelled
//
// Assertions:
//   - Record is NOT completed (send timed out)
//   - Sender sees context cancellation
func TestOutboxDrainer_SendTimeout(t *testing.T) {
	blockingSender := &BlockingSender{}

	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox := NewFakeOutboxStore()
	dlqStore := NewFakeDLQStore()
	leaseStore := NewFakeLeaseStore()

	pk := persistence.OutboxPartitionKey("sess-1", "")
	_, _ = leaseStore.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	cfg := outboxpkg.Config{
		OutboxStore:  outbox,
		LeaseStore:   leaseStore,
		Sender:       blockingSender,
		DLQ:          dlq.New(dlqStore),
		RouteID:      "route-1",
		PartitionKey: pk,
		LeaseID:      "sess-1",
		OwnerID:      token.Owner,
		Policy: routing.RoutePolicy{
			SendTimeout: 100 * time.Millisecond,
		}.WithDefaults(),
		Strategy: persistence.NewFixedPoll(50 * time.Millisecond),
		TokenFn: func() (persistence.LeaseToken, bool) {
			return token, true
		},
	}
	drainer := outboxpkg.New(cfg)

	ctx := context.Background()
	rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID:         "rec-timeout",
		RouteID:    "route-1",
		EnvelopeID: "env-timeout",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope:   messaging.Envelope{ID: "env-timeout", Payload: []byte("data")},
		Status:     persistence.OutboxPending,
	})
	_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if outbox.CompletedCount() != 0 {
		t.Fatal("record should NOT be completed when sender times out")
	}
	if blockingSender.ContextErrors() == 0 {
		t.Fatal("sender should have received at least one context cancellation")
	}
}

// TestRouteRunner_SendTimeout validates that a hanging sender in
// directHold delivery is cancelled by the per-operation send timeout.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	Sender blocks until ctx done → SendTimeout fires → delivery retried
//
// ───────────────────────────────────────────────
func TestRouteRunner_SendTimeout(t *testing.T) {
	blockingSender := &BlockingSender{}

	receiver := NewFakeReceiver()
	dlqStore := NewFakeDLQStore()
	outbox := NewFakeOutboxStore()

	cfg := route.RouteRunnerConfig{
		RouteID: "test-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			SendTimeout:  100 * time.Millisecond,
		}.WithDefaults(),
		Receiver:    receiver,
		Sender:      blockingSender,
		OutboxStore: outbox,
		DLQ:         dlq.New(dlqStore),
		InstanceID:  "bridge-1",
	}
	runner := route.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(&messaging.Envelope{ID: "msg-send-timeout"})
	_ = receiver.Emit(ctx, del)
	waitFor(t, 2*time.Second, "delivery retried after send timeout", func() bool {
		return del.IsRetried()
	})

	if del.IsAcked() {
		t.Fatal("delivery should not be acked when sender times out")
	}
}

// ═══════════════════════════════════════════════════════════════════════
// T7: Stale Fencing Token Batch Cancellation Tests
//
// Validates that when processRecord returns ErrStaleFencingToken,
// sibling goroutines are cancelled via batchCtx to prevent duplicate
// deliveries from the old owner.
//
//   ┌─ goroutine 1 ──▶ ErrStaleFencingToken ──▶ batchCancel()
//   │
//   ├─ goroutine 2 ──▶ batchCtx.Done() ──▶ aborted
//   │
//   └─ goroutine 3 ──▶ batchCtx.Done() ──▶ aborted
// ═══════════════════════════════════════════════════════════════════════

// TestOutboxDrainer_StaleFencingToken_CancelsSiblings validates that a
// stale fencing token from one record cancels the batch context, preventing
// sibling goroutines from completing their sends.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	rec-first: Complete returns ErrStaleFencingToken → batchCancel()
//	rec-slow:  sender blocks on ctx → cancelled by batchCancel()
//
// ───────────────────────────────────────────────
//
// Test Parameters:
//   - 2 outbox records
//   - rec-first triggers ErrStaleFencingToken on Complete
//   - rec-slow uses a context-aware sender that blocks
//   - DrainMaxConcurrency: 2 (both goroutines run in parallel)
//
// Assertions:
//   - rec-slow sender sees context cancellation
//   - rec-slow is NOT completed
func TestOutboxDrainer_StaleFencingToken_CancelsSiblings(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	var slowSendCancelled atomic.Int32

	ctxSender := &ContextAwareSender{
		sendFn: func(ctx context.Context, env *messaging.Envelope) error {
			if env.ID == "env-slow" {
				<-ctx.Done()
				slowSendCancelled.Add(1)
				return ctx.Err()
			}
			return nil
		},
	}

	outbox := NewFakeOutboxStore()
	dlqStore := NewFakeDLQStore()
	leaseStore := NewFakeLeaseStore()

	pk := persistence.OutboxPartitionKey("sess-1", "")
	_, _ = leaseStore.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	outbox.CompleteFn = func(ids []string, _ persistence.LeaseToken) error {
		for _, id := range ids {
			if id == "rec-first" {
				return shared.ErrStaleFencingToken
			}
		}
		return nil
	}

	// Only allow one drain batch; after the first call + per-record
	// pre-send checks, revoke the token so the drainer does not
	// re-process records on subsequent poll cycles.
	// Threshold: 1 (Run loop) + 2 (pre-send checks for 2 records) = 3.
	var tokenCalls atomic.Int32
	cfg := outboxpkg.Config{
		OutboxStore:         outbox,
		LeaseStore:          leaseStore,
		Sender:              ctxSender,
		DLQ:                 dlq.New(dlqStore),
		RouteID:             "route-1",
		PartitionKey:        pk,
		LeaseID:             "sess-1",
		OwnerID:             token.Owner,
		Policy:              routing.RoutePolicy{}.WithDefaults(),
		Strategy:            persistence.NewFixedPoll(50 * time.Millisecond),
		DrainMaxConcurrency: 2,
		TokenFn: func() (persistence.LeaseToken, bool) {
			if tokenCalls.Add(1) <= 3 {
				return token, true
			}
			return persistence.LeaseToken{}, false
		},
	}
	drainer := outboxpkg.New(cfg)

	ctx := context.Background()
	records := []*persistence.OutboxRecord{
		persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
			ID: "rec-first", RouteID: "route-1", EnvelopeID: "env-first",
			BindingID: "bind-1", SessionID: "sess-1",
			Envelope: messaging.Envelope{ID: "env-first", Payload: []byte("data")},
			Status:   persistence.OutboxPending,
		}),
		persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
			ID: "rec-slow", RouteID: "route-1", EnvelopeID: "env-slow",
			BindingID: "bind-1", SessionID: "sess-1",
			Envelope: messaging.Envelope{ID: "env-slow", Payload: []byte("data")},
			Status:   persistence.OutboxPending,
		})}
	for _, r := range records {
		_ = outbox.Persist(ctx, []*persistence.OutboxRecord{r})
	}

	drainCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if slowSendCancelled.Load() == 0 {
		t.Fatal("slow sender should have been cancelled by batchCancel after ErrStaleFencingToken")
	}
	if outbox.CompletedCount() != 0 {
		t.Fatalf("no records should be completed, got %d", outbox.CompletedCount())
	}
}

// TestOutboxDrainer_StaleFencingToken_PropagatedToRunLoop validates that
// when drainBatch detects ErrStaleFencingToken, it propagates the error
// back to the Run loop rather than swallowing it silently.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	Complete always returns ErrStaleFencingToken →
//	drainBatch returns ErrStaleFencingToken →
//	Run logs "stale fencing token" and continues polling
//
// ───────────────────────────────────────────────
//
// Assertions:
//   - Record is not completed
//   - Drainer does not crash
//   - Sender is invoked (send succeeds, Complete fails)
func TestOutboxDrainer_StaleFencingToken_PropagatedToRunLoop(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	sender := NewFakeSender()
	outbox := NewFakeOutboxStore()
	dlqStore := NewFakeDLQStore()
	leaseStore := NewFakeLeaseStore()

	pk := persistence.OutboxPartitionKey("sess-1", "")
	_, _ = leaseStore.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	outbox.CompleteFn = func(_ []string, _ persistence.LeaseToken) error {
		return shared.ErrStaleFencingToken
	}

	// Threshold: 1 (Run loop) + 1 (pre-send check for 1 record) = 2.
	var tokenCalls atomic.Int32
	cfg := outboxpkg.Config{
		OutboxStore:  outbox,
		LeaseStore:   leaseStore,
		Sender:       sender,
		DLQ:          dlq.New(dlqStore),
		RouteID:      "route-1",
		PartitionKey: pk,
		LeaseID:      "sess-1",
		OwnerID:      token.Owner,
		Policy:       routing.RoutePolicy{}.WithDefaults(),
		Strategy:     persistence.NewFixedPoll(50 * time.Millisecond),
		TokenFn: func() (persistence.LeaseToken, bool) {
			if tokenCalls.Add(1) <= 2 {
				return token, true
			}
			return persistence.LeaseToken{}, false
		},
	}
	drainer := outboxpkg.New(cfg)

	ctx := context.Background()
	rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID: "rec-stale", RouteID: "route-1", EnvelopeID: "env-stale",
		BindingID: "bind-1", SessionID: "sess-1",
		Envelope: messaging.Envelope{ID: "env-stale", Payload: []byte("data")},
		Status:   persistence.OutboxPending,
	})
	_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() == 0 {
		t.Fatal("sender should have been called (send succeeds, Complete fails)")
	}
	if outbox.CompletedCount() != 0 {
		t.Fatal("no records should be completed when Complete returns stale token error")
	}
}

// ---------------------------------------------------------------------------
// ContextAwareSender
// ---------------------------------------------------------------------------

// ContextAwareSender implements ports.Sender with a function that receives
// the context, enabling tests to verify context cancellation propagation.
type ContextAwareSender struct {
	sendFn func(context.Context, *messaging.Envelope) error
}

func (s *ContextAwareSender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	env := msg.Envelope
	return s.sendFn(ctx, env)
}

// ---------------------------------------------------------------------------
// BlockingSender
// ---------------------------------------------------------------------------

// BlockingSender implements ports.Sender and blocks until the context is
// cancelled, allowing tests to verify per-operation send timeouts.
type BlockingSender struct {
	mu        sync.Mutex
	ctxErrors int
}

func (s *BlockingSender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	<-ctx.Done()
	s.mu.Lock()
	s.ctxErrors++
	s.mu.Unlock()
	return ctx.Err()
}

func (s *BlockingSender) ContextErrors() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctxErrors
}
