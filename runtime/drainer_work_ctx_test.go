package runtime_test

import (
	"context"
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
)

// ═══════════════════════════════════════════════════════════════════════
// workCtx timeout derived from SendTimeout
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
//
//	DrainTimeout:  500ms
//	SendTimeout:   2s
//	Sender blocks: 800ms  (> drainTimeout, < SendTimeout)
//	Expected:      send succeeds, record completed
//
// ───────────────────────────────────────────────
//
// Assertions:
//   - Record is completed (send finished within SendTimeout)
//   - Sender was invoked
func TestOutboxDrainer_WorkCtxNotCappedBySendTimeout(t *testing.T) {
	var sendInvoked atomic.Bool

	ctxSender := &ContextAwareSender{
		sendFn: func(ctx context.Context, _ *messaging.Envelope) error {
			sendInvoked.Store(true)
			select {
			case <-time.After(800 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}

	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox := NewFakeOutboxStore()
	dlqStore := NewFakeDLQStore()
	leaseStore := NewFakeLeaseStore()

	pk := persistence.OutboxPartitionKey("sess-1", "")
	_, _ = leaseStore.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	var tokenCalls atomic.Int32
	cfg := outboxpkg.Config{
		OutboxStore:  outbox,
		LeaseStore:   leaseStore,
		Sender:       ctxSender,
		DLQ:          dlq.New(dlqStore),
		RouteID:      "route-1",
		PartitionKey: pk,
		LeaseID:      "sess-1",
		Policy: routing.RoutePolicy{
			SendTimeout: 2 * time.Second,
		}.WithDefaults(),
		Strategy:     persistence.NewFixedPoll(50 * time.Millisecond),
		DrainTimeout: 5 * time.Second,
		// Threshold: 1 (Run loop) + 1 (pre-send check) + 1 (post-send fence) = 3. The token must stay live THROUGH the post-send fence for
		// the record to complete; this test exercises the DrainTimeout-vs-
		// SendTimeout window, not lease staleness, so the owner never loses the
		// lease during the successful send.
		TokenFn: func() (persistence.LeaseToken, bool) {
			if tokenCalls.Add(1) <= 3 {
				return token, true
			}
			return persistence.LeaseToken{}, false
		},
	}
	drainer := outboxpkg.New(cfg)

	ctx := context.Background()
	rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID:         "rec-m1",
		RouteID:    "route-1",
		EnvelopeID: "env-m1",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-m1", Payload: []byte("data")}),
		Status:     persistence.OutboxPending,
	})
	_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})

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
// MetricOutboxCompletions emitted only after Complete succeeds
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

	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox := NewFakeOutboxStore()
	dlqStore := NewFakeDLQStore()
	leaseStore := NewFakeLeaseStore()
	sender := NewFakeSender()

	pk := persistence.OutboxPartitionKey("sess-1", "")
	_, _ = leaseStore.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	var completeCalls atomic.Int32
	outbox.CompleteFn = func(_ []string, _ persistence.LeaseToken) error {
		completeCalls.Add(1)
		return shared.ErrStaleFencingToken
	}

	var tokenCalls atomic.Int32
	cfg := outboxpkg.Config{
		OutboxStore:  outbox,
		LeaseStore:   leaseStore,
		Sender:       sender,
		DLQ:          dlq.New(dlqStore),
		RouteID:      "route-1",
		PartitionKey: pk,
		LeaseID:      "sess-1",
		Policy:       routing.RoutePolicy{}.WithDefaults(),
		Strategy:     persistence.NewFixedPoll(50 * time.Millisecond),
		Metrics:      rec,
		// Threshold: 1 (Run loop) + 1 (pre-send check) + 1 (post-send fence) = 3.
		// The token must stay live THROUGH the post-send fence so the success path
		// actually reaches Complete; otherwise the fence would short-circuit and
		// this test would pass for the wrong reason (fence fired, not Complete
		// failed). tokenFn returns the SAME token (v1) each call, so the pre-send
		// and post-send version checks match — only the Complete-fails path is
		// under test here.
		TokenFn: func() (persistence.LeaseToken, bool) {
			if tokenCalls.Add(1) <= 3 {
				return token, true
			}
			return persistence.LeaseToken{}, false
		},
	}
	drainer := outboxpkg.New(cfg)

	ctx := context.Background()
	outboxRec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID:         "rec-m3",
		RouteID:    "route-1",
		EnvelopeID: "env-m3",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-m3", Payload: []byte("data")}),
		Status:     persistence.OutboxPending,
	})
	_ = outbox.Persist(ctx, []*persistence.OutboxRecord{outboxRec})

	drainCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() == 0 {
		t.Fatal("sender should have been invoked")
	}

	// The test must GENUINELY exercise the Complete-fails path: with the post-send
	// fence in place, the live token has to survive the fence so Complete is
	// actually attempted (and returns stale). If the fence had short-circuited,
	// CompleteFn would never run and this test's "no completions" assertion would
	// pass vacuously.
	if got := completeCalls.Load(); got != 1 {
		t.Fatalf("CompleteFn invoked %d times, want exactly 1 (test must reach Complete, not be short-circuited by the post-send fence)", got)
	}

	completions := rec.FindEntries(shared.MetricOutboxCompletions)
	if len(completions) != 0 {
		t.Fatalf("MetricOutboxCompletions should NOT be emitted when Complete fails, got %d", len(completions))
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Negative SendTimeout guard
//
//   SendTimeout = -1s → WithDefaults() → DefaultSendTimeout (30s)
// ═══════════════════════════════════════════════════════════════════════

// TestRoutePolicy_WithDefaults_GuardsNegativeSendTimeout validates that
// a negative SendTimeout is replaced by the default.
func TestRoutePolicy_WithDefaults_GuardsNegativeSendTimeout(t *testing.T) {
	p := routing.RoutePolicy{SendTimeout: -1 * time.Second}.WithDefaults()
	if p.SendTimeout != routing.DefaultSendTimeout {
		t.Fatalf("expected SendTimeout=%v for negative input, got %v", routing.DefaultSendTimeout, p.SendTimeout)
	}
}
