package runtime_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// ═══════════════════════════════════════════════════════════════════════
// M1: workCtx timeout derived from SendTimeout
//
// Validates that drainBatch computes the workCtx deadline from
// SendTimeout (plus buffer) rather than from drainTimeout, so the
// configured SendTimeout is not silently capped.
//
//   SendTimeout = 2s → workCtx ≥ 2s  (not capped to drainTimeout=500ms)
// ═══════════════════════════════════════════════════════════════════════

// TestOutboxDrainer_WorkCtxNotCappedBySendTimeout validates that a sender
// whose response time exceeds drainTimeout but is within SendTimeout
// still succeeds.
//
// Scenario:
// ───────────────────────────────────────────────
//   DrainTimeout:  500ms
//   SendTimeout:   2s
//   Sender blocks: 800ms  (> drainTimeout, < SendTimeout)
//   Expected:      send succeeds, record completed
// ───────────────────────────────────────────────
//
// Assertions:
//   - Record is completed (send finished within SendTimeout)
//   - Sender was invoked
func TestOutboxDrainer_WorkCtxNotCappedBySendTimeout(t *testing.T) {
	var sendInvoked atomic.Bool

	ctxSender := &ContextAwareSender{
		sendFn: func(ctx context.Context, _ *domain.Envelope) error {
			sendInvoked.Store(true)
			select {
			case <-time.After(800 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}

	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox := NewFakeOutboxStore()
	dlqStore := NewFakeDLQStore()
	leaseStore := NewFakeLeaseStore()

	pk := domain.OutboxPartitionKey("sess-1", "")
	_, _ = leaseStore.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second)

	var tokenCalls atomic.Int32
	cfg := goruntime.OutboxDrainerConfig{
		OutboxStore:  outbox,
		LeaseStore:   leaseStore,
		Sender:       ctxSender,
		DLQ:          goruntime.NewDLQRouter(dlqStore),
		RouteID:      "route-1",
		PartitionKey: pk,
		LeaseID:      "sess-1",
		OwnerID:      token.Owner,
		Policy: domain.RoutePolicy{
			SendTimeout: 2 * time.Second,
		}.WithDefaults(),
		Strategy:     domain.NewFixedPoll(50 * time.Millisecond),
		DrainTimeout: 500 * time.Millisecond,
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
		ID:         "rec-m1",
		RouteID:    "route-1",
		EnvelopeID: "env-m1",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope:   domain.Envelope{ID: "env-m1", Payload: []byte("data")},
		Status:     domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if !sendInvoked.Load() {
		t.Fatal("sender should have been invoked")
	}
	if outbox.CompletedCount() != 1 {
		t.Fatalf("expected 1 completed record (send within SendTimeout), got %d", outbox.CompletedCount())
	}
}

// ═══════════════════════════════════════════════════════════════════════
// M3: MetricOutboxCompletions emitted only after Complete succeeds
//
//   Send ──▶ ✓ ──▶ Complete ──▶ ✗ (ErrStaleFencingToken)
//                                → MetricOutboxCompletions = 0
// ═══════════════════════════════════════════════════════════════════════

// TestOutboxDrainer_MetricNotEmittedOnCompleteFail validates that
// MetricOutboxCompletions is NOT emitted when Send succeeds but
// Complete fails (e.g., due to a stale fencing token).
//
// Assertions:
//   - Sender is invoked (send succeeds)
//   - MetricOutboxCompletions count is 0
//   - Record is not completed
func TestOutboxDrainer_MetricNotEmittedOnCompleteFail(t *testing.T) {
	rec := &ports.RecordingExporter{}

	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox := NewFakeOutboxStore()
	dlqStore := NewFakeDLQStore()
	leaseStore := NewFakeLeaseStore()
	sender := NewFakeSender()

	pk := domain.OutboxPartitionKey("sess-1", "")
	_, _ = leaseStore.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second)

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
		Metrics:      rec,
		TokenFn: func() (domain.LeaseToken, bool) {
			if tokenCalls.Add(1) <= 1 {
				return token, true
			}
			return domain.LeaseToken{}, false
		},
	}
	drainer := goruntime.NewOutboxDrainerFromConfig(cfg)

	ctx := context.Background()
	outboxRec := domain.OutboxRecord{
		ID:         "rec-m3",
		RouteID:    "route-1",
		EnvelopeID: "env-m3",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope:   domain.Envelope{ID: "env-m3", Payload: []byte("data")},
		Status:     domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{outboxRec})

	drainCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() == 0 {
		t.Fatal("sender should have been invoked")
	}

	completions := rec.FindEntries(domain.MetricOutboxCompletions)
	if len(completions) != 0 {
		t.Fatalf("MetricOutboxCompletions should NOT be emitted when Complete fails, got %d", len(completions))
	}
}

// ═══════════════════════════════════════════════════════════════════════
// L4: Negative SendTimeout guard
//
//   SendTimeout = -1s → WithDefaults() → DefaultSendTimeout (30s)
// ═══════════════════════════════════════════════════════════════════════

// TestRoutePolicy_WithDefaults_GuardsNegativeSendTimeout validates that
// a negative SendTimeout is replaced by the default.
func TestRoutePolicy_WithDefaults_GuardsNegativeSendTimeout(t *testing.T) {
	p := domain.RoutePolicy{SendTimeout: -1 * time.Second}.WithDefaults()
	if p.SendTimeout != domain.DefaultSendTimeout {
		t.Fatalf("expected SendTimeout=%v for negative input, got %v", domain.DefaultSendTimeout, p.SendTimeout)
	}
}
