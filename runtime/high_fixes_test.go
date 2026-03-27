package runtime_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
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
	p := domain.RoutePolicy{}.WithDefaults()
	if p.SendTimeout != domain.DefaultSendTimeout {
		t.Fatalf("expected SendTimeout=%v, got %v", domain.DefaultSendTimeout, p.SendTimeout)
	}
}

// TestRoutePolicy_WithDefaults_PreservesExplicitSendTimeout validates
// an explicit SendTimeout is not overwritten by WithDefaults.
func TestRoutePolicy_WithDefaults_PreservesExplicitSendTimeout(t *testing.T) {
	p := domain.RoutePolicy{SendTimeout: 5 * time.Second}.WithDefaults()
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

	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox := NewFakeOutboxStore()
	dlqStore := NewFakeDLQStore()
	leaseStore := NewFakeLeaseStore()

	pk := domain.OutboxPartitionKey("sess-1", "")
	_, _ = leaseStore.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	cfg := goruntime.OutboxDrainerConfig{
		OutboxStore:  outbox,
		LeaseStore:   leaseStore,
		Sender:       blockingSender,
		DLQ:          goruntime.NewDLQRouter(dlqStore),
		RouteID:      "route-1",
		PartitionKey: pk,
		LeaseID:      "sess-1",
		OwnerID:      token.Owner,
		Policy: domain.RoutePolicy{
			SendTimeout: 100 * time.Millisecond,
		}.WithDefaults(),
		Strategy: domain.NewFixedPoll(50 * time.Millisecond),
		TokenFn: func() (domain.LeaseToken, bool) {
			return token, true
		},
	}
	drainer := goruntime.NewOutboxDrainerFromConfig(cfg)

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID:         "rec-timeout",
		RouteID:    "route-1",
		EnvelopeID: "env-timeout",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope:   domain.Envelope{ID: "env-timeout", Payload: []byte("data")},
		Status:     domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

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

	cfg := goruntime.RouteRunnerConfig{
		RouteID: "test-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
			SendTimeout:  100 * time.Millisecond,
		}.WithDefaults(),
		Receiver:    receiver,
		Sender:      blockingSender,
		OutboxStore: outbox,
		DLQ:         goruntime.NewDLQRouter(dlqStore),
		InstanceID:  "bridge-1",
	}
	runner := goruntime.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(&domain.Envelope{ID: "msg-send-timeout"})
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
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	var slowSendCancelled atomic.Int32

	ctxSender := &ContextAwareSender{
		sendFn: func(ctx context.Context, env *domain.Envelope) error {
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

	pk := domain.OutboxPartitionKey("sess-1", "")
	_, _ = leaseStore.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	outbox.CompleteFn = func(ids []string, _ domain.LeaseToken) error {
		for _, id := range ids {
			if id == "rec-first" {
				return domain.ErrStaleFencingToken
			}
		}
		return nil
	}

	// Only allow one drain batch; after the first call, revoke the token
	// so the drainer does not re-process records on subsequent poll cycles.
	var tokenCalls atomic.Int32
	cfg := goruntime.OutboxDrainerConfig{
		OutboxStore:         outbox,
		LeaseStore:          leaseStore,
		Sender:              ctxSender,
		DLQ:                 goruntime.NewDLQRouter(dlqStore),
		RouteID:             "route-1",
		PartitionKey:        pk,
		LeaseID:             "sess-1",
		OwnerID:             token.Owner,
		Policy:              domain.RoutePolicy{}.WithDefaults(),
		Strategy:            domain.NewFixedPoll(50 * time.Millisecond),
		DrainMaxConcurrency: 2,
		TokenFn: func() (domain.LeaseToken, bool) {
			if tokenCalls.Add(1) <= 1 {
				return token, true
			}
			return domain.LeaseToken{}, false
		},
	}
	drainer := goruntime.NewOutboxDrainerFromConfig(cfg)

	ctx := context.Background()
	records := []domain.OutboxRecord{
		{
			ID: "rec-first", RouteID: "route-1", EnvelopeID: "env-first",
			BindingID: "bind-1", SessionID: "sess-1",
			Envelope: domain.Envelope{ID: "env-first", Payload: []byte("data")},
			Status:   domain.OutboxPending,
		},
		{
			ID: "rec-slow", RouteID: "route-1", EnvelopeID: "env-slow",
			BindingID: "bind-1", SessionID: "sess-1",
			Envelope: domain.Envelope{ID: "env-slow", Payload: []byte("data")},
			Status:   domain.OutboxPending,
		},
	}
	for _, r := range records {
		_ = outbox.Persist(ctx, []domain.OutboxRecord{r})
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
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	sender := NewFakeSender()
	outbox := NewFakeOutboxStore()
	dlqStore := NewFakeDLQStore()
	leaseStore := NewFakeLeaseStore()

	pk := domain.OutboxPartitionKey("sess-1", "")
	_, _ = leaseStore.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	outbox.CompleteFn = func(_ []string, _ domain.LeaseToken) error {
		return domain.ErrStaleFencingToken
	}

	var tokenCalls atomic.Int32
	cfg := goruntime.OutboxDrainerConfig{
		OutboxStore:  outbox,
		LeaseStore:   leaseStore,
		Sender:       sender,
		DLQ:          goruntime.NewDLQRouter(dlqStore),
		RouteID:      "route-1",
		PartitionKey: pk,
		LeaseID:      "sess-1",
		OwnerID:      token.Owner,
		Policy:       domain.RoutePolicy{}.WithDefaults(),
		Strategy:     domain.NewFixedPoll(50 * time.Millisecond),
		TokenFn: func() (domain.LeaseToken, bool) {
			if tokenCalls.Add(1) <= 1 {
				return token, true
			}
			return domain.LeaseToken{}, false
		},
	}
	drainer := goruntime.NewOutboxDrainerFromConfig(cfg)

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID: "rec-stale", RouteID: "route-1", EnvelopeID: "env-stale",
		BindingID: "bind-1", SessionID: "sess-1",
		Envelope: domain.Envelope{ID: "env-stale", Payload: []byte("data")},
		Status:   domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

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
	sendFn func(context.Context, *domain.Envelope) error
}

func (s *ContextAwareSender) Send(ctx context.Context, env *domain.Envelope) error {
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

func (s *BlockingSender) Send(ctx context.Context, _ *domain.Envelope) error {
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
