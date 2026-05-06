package runtime_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// ctxAwareSender is a test sender whose Send respects context
// cancellation while simulating an egress latency. Unlike FakeSender /
// ConcurrentSender (which invoke a context-less SendFn), this sender
// aborts mid-latency when the caller's context is cancelled — required
// for tests that verify the drainer's batch-ctx deadline actually
// propagates.
type ctxAwareSender struct {
	latency time.Duration
	mu      sync.Mutex
	sent    int64
}

var _ ports.Sender = (*ctxAwareSender)(nil)

func (s *ctxAwareSender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.latency):
		s.mu.Lock()
		s.sent++
		s.mu.Unlock()
		return nil
	}
}

func (s *ctxAwareSender) sentCount() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}

// TestComputeBatchDeadline_BackwardCompat verifies that when neither of
// the new scaling fields is set, the helper returns the legacy
// DrainTimeout verbatim so existing callers keep the pre-scaling
// semantics (a single fixed batch deadline regardless of batch size).
func TestComputeBatchDeadline_BackwardCompat(t *testing.T) {
	cfg := goruntime.OutboxDrainerConfig{
		DrainTimeout: 7 * time.Second,
	}
	for _, batchCount := range []int{0, 1, 5, 100} {
		got := goruntime.ComputeBatchDeadlineForTest(batchCount, cfg)
		if got != 7*time.Second {
			t.Fatalf("batchCount=%d: expected 7s (legacy DrainTimeout), got %v",
				batchCount, got)
		}
	}
}

// TestComputeBatchDeadline_ScalesWithBatchSize verifies that when the
// scaled formula is active and the product stays under the cap, the
// result is batchCount * PerRecord.
func TestComputeBatchDeadline_ScalesWithBatchSize(t *testing.T) {
	cfg := goruntime.OutboxDrainerConfig{
		PerRecordDrainTimeout: 2 * time.Second,
		MaxDrainTimeout:       30 * time.Second,
	}
	got := goruntime.ComputeBatchDeadlineForTest(3, cfg)
	want := 6 * time.Second
	if got != want {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

// TestComputeBatchDeadline_CappedAtMax verifies that a large batch
// cannot produce a deadline longer than MaxDrainTimeout — this is the
// reason we chose min() over max() for the scaling formula.
func TestComputeBatchDeadline_CappedAtMax(t *testing.T) {
	cfg := goruntime.OutboxDrainerConfig{
		PerRecordDrainTimeout: 2 * time.Second,
		MaxDrainTimeout:       30 * time.Second,
	}
	got := goruntime.ComputeBatchDeadlineForTest(100, cfg)
	want := 30 * time.Second
	if got != want {
		t.Fatalf("expected cap %v, got %v", want, got)
	}
}

// TestComputeBatchDeadline_DefaultsApplyWhenOneFieldSet verifies that
// when the caller sets only PerRecordDrainTimeout, the default
// MaxDrainTimeout (10s) applies as the ceiling.
func TestComputeBatchDeadline_DefaultsApplyWhenOneFieldSet(t *testing.T) {
	cfg := goruntime.OutboxDrainerConfig{
		PerRecordDrainTimeout: 2 * time.Second,
	}
	// batchCount=100 * 2s = 200s, capped at default 10s.
	got := goruntime.ComputeBatchDeadlineForTest(100, cfg)
	if got != 10*time.Second {
		t.Fatalf("expected default MaxDrainTimeout 10s, got %v", got)
	}

	// And when only MaxDrainTimeout is set, default PerRecord (3s)
	// applies per record.
	cfg = goruntime.OutboxDrainerConfig{
		MaxDrainTimeout: 30 * time.Second,
	}
	got = goruntime.ComputeBatchDeadlineForTest(2, cfg)
	if got != 6*time.Second {
		t.Fatalf("expected 2 * default PerRecord (6s), got %v", got)
	}
}

// TestComputeBatchDeadline_ZeroBatchReturnsFloor is a guard for the
// edge case where batchCount=0 slips into the helper. It must not
// return 0 when the scaled formula is active — that would mean an
// already-expired context in the caller.
func TestComputeBatchDeadline_ZeroBatchReturnsFloor(t *testing.T) {
	cfg := goruntime.OutboxDrainerConfig{
		PerRecordDrainTimeout: 2 * time.Second,
		MaxDrainTimeout:       30 * time.Second,
	}
	got := goruntime.ComputeBatchDeadlineForTest(0, cfg)
	if got != 30*time.Second {
		t.Fatalf("expected MaxDrainTimeout when batch==0, got %v", got)
	}
}

// TestOutboxDrainer_ScaledTimeout_SlowSenderBatchCompletes verifies
// end-to-end that a batch of 5 records under a slow sender (each send
// takes ~250ms) completes successfully when the scaled formula is
// enabled, producing a deadline well above the legacy 3s fixed
// ceiling that would otherwise cancel mid-batch.
//
// Setup:
//   - 5 records, DrainMaxConcurrency=1 (serialize sends so total time
//     is ~5 * 250ms = 1.25s)
//   - SendTimeout set small so per-send budget is not the limit — the
//     test is about total batch ceiling
//   - PerRecordDrainTimeout=500ms, MaxDrainTimeout=5s → deadline for
//     a 5-record batch is 2.5s, plenty for serial sends
//   - ConcurrentSender used to sidestep the serialization mutex in
//     FakeSender so SendFn latency is observable without starving the
//     semaphore
//
// Assertions:
//   - All 5 records reach Completed status
//   - No duplicate completion (no stale-claim re-pickup)
func TestOutboxDrainer_ScaledTimeout_SlowSenderBatchCompletes(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}

	const recordCount = 5
	const sendLatency = 250 * time.Millisecond

	outbox := NewFakeOutboxStore()
	leaseStore := NewFakeLeaseStore()
	dlqStore := NewFakeDLQStore()

	pk := persistence.OutboxPartitionKey("sess-slow", "")
	if _, err := leaseStore.Acquire(context.Background(), "sess-slow",
		token.Owner, 30*time.Second, nil); err != nil {
		t.Fatalf("acquire lease: %v", err)
	}

	ctx := context.Background()
	for i := range recordCount {
		rec := persistence.OutboxRecord{
			ID:         fmt.Sprintf("rec-%d", i),
			RouteID:    "route-1",
			EnvelopeID: fmt.Sprintf("env-%d", i),
			BindingID:  "bind-1",
			SessionID:  "sess-slow",
			Envelope: messaging.Envelope{
				ID:      fmt.Sprintf("env-%d", i),
				Payload: []byte("payload"),
			},
			Status: persistence.OutboxPending,
		}
		if err := outbox.Persist(ctx, []persistence.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist: %v", err)
		}
	}

	slowSender := &ctxAwareSender{latency: sendLatency}

	batchCh := make(chan int, 4)
	cfg := goruntime.OutboxDrainerConfig{
		OutboxStore:         outbox,
		LeaseStore:          leaseStore,
		Sender:              slowSender,
		DLQ:                 goruntime.NewDLQRouter(dlqStore),
		RouteID:             "route-1",
		PartitionKey:        pk,
		LeaseID:             "sess-slow",
		OwnerID:             token.Owner,
		Policy:              routing.RoutePolicy{}.WithDefaults(),
		Strategy:            persistence.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize:      recordCount,
		DrainMaxBatchSize:   recordCount,
		DrainMaxConcurrency: 1,
		// Scaled timeout: 5 * 500ms = 2500ms, capped at 5s.
		// Legacy DrainTimeout of 500ms would cancel mid-batch.
		DrainTimeout:          500 * time.Millisecond,
		PerRecordDrainTimeout: 500 * time.Millisecond,
		MaxDrainTimeout:       5 * time.Second,
		BatchTimeoutFloor:     2 * time.Second,
		TokenFn: func() (persistence.LeaseToken, bool) {
			return token, true
		},
		OnBatchComplete: func(n int) { batchCh <- n },
	}
	drainer := goruntime.NewOutboxDrainerFromConfig(cfg)

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = drainer.Run(runCtx)
		close(done)
	}()

	// Wait for the batch callback with the expected success count.
	select {
	case n := <-batchCh:
		if n != recordCount {
			t.Fatalf("expected %d successful sends in one batch, got %d "+
				"(scaled timeout should have covered the full batch)",
				recordCount, n)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("drain batch did not complete: sends=%d, completed=%d",
			slowSender.sentCount(), outbox.CompletedCount())
	}

	cancel()
	<-done

	if got := slowSender.sentCount(); got != int64(recordCount) {
		t.Fatalf("expected %d sends, got %d", recordCount, got)
	}
	if got := outbox.CompletedCount(); got != recordCount {
		t.Fatalf("expected %d completions, got %d", recordCount, got)
	}
}

// TestOutboxDrainer_LegacyTimeout_SlowSenderBatchCancelled is the
// negative counterpart to the scaled-timeout test: when only the
// legacy DrainTimeout is set (new fields zero), a slow batch should
// hit the fixed deadline and fail to complete all records. This
// confirms that the backward-compat path still behaves like the old
// code.
//
// Assertions:
//   - Fewer than recordCount records complete within the fixed
//     DrainTimeout window (context cancels mid-batch).
func TestOutboxDrainer_LegacyTimeout_SlowSenderBatchCancelled(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}

	const recordCount = 5
	const sendLatency = 250 * time.Millisecond

	outbox := NewFakeOutboxStore()
	leaseStore := NewFakeLeaseStore()
	dlqStore := NewFakeDLQStore()

	pk := persistence.OutboxPartitionKey("sess-legacy", "")
	if _, err := leaseStore.Acquire(context.Background(), "sess-legacy",
		token.Owner, 30*time.Second, nil); err != nil {
		t.Fatalf("acquire lease: %v", err)
	}

	ctx := context.Background()
	for i := range recordCount {
		rec := persistence.OutboxRecord{
			ID:         fmt.Sprintf("rec-legacy-%d", i),
			RouteID:    "route-1",
			EnvelopeID: fmt.Sprintf("env-legacy-%d", i),
			BindingID:  "bind-1",
			SessionID:  "sess-legacy",
			Envelope: messaging.Envelope{
				ID:      fmt.Sprintf("env-legacy-%d", i),
				Payload: []byte("payload"),
			},
			Status: persistence.OutboxPending,
		}
		if err := outbox.Persist(ctx, []persistence.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist: %v", err)
		}
	}

	slowSender := &ctxAwareSender{latency: sendLatency}

	batchCh := make(chan int, 8)
	cfg := goruntime.OutboxDrainerConfig{
		OutboxStore:         outbox,
		LeaseStore:          leaseStore,
		Sender:              slowSender,
		DLQ:                 goruntime.NewDLQRouter(dlqStore),
		RouteID:             "route-1",
		PartitionKey:        pk,
		LeaseID:             "sess-legacy",
		OwnerID:             token.Owner,
		Policy:              routing.RoutePolicy{}.WithDefaults(),
		Strategy:            persistence.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize:      recordCount,
		DrainMaxBatchSize:   recordCount,
		DrainMaxConcurrency: 1,
		// Legacy fixed DrainTimeout well below serial-batch duration
		// (5 * 250ms = 1250ms > 500ms). Leaving the new fields zero
		// forces backward-compat path. Subsequent drain cycles may
		// re-pick the unfinished records, so we assert on the *first*
		// batch's success count rather than the cumulative total.
		DrainTimeout:      500 * time.Millisecond,
		BatchTimeoutFloor: 500 * time.Millisecond,
		TokenFn: func() (persistence.LeaseToken, bool) {
			return token, true
		},
		OnBatchComplete: func(n int) {
			select {
			case batchCh <- n:
			default:
			}
		},
	}
	drainer := goruntime.NewOutboxDrainerFromConfig(cfg)

	runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = drainer.Run(runCtx)
		close(done)
	}()

	// Capture the success count from the first drain batch — the one
	// whose deadline is the fixed legacy DrainTimeout.
	var firstBatch int
	select {
	case firstBatch = <-batchCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("no batch callback received; completed=%d",
			outbox.CompletedCount())
	}

	cancel()
	<-done

	if firstBatch >= recordCount {
		t.Fatalf("legacy fixed DrainTimeout should have cancelled the "+
			"first batch before all %d records completed; got %d in one batch",
			recordCount, firstBatch)
	}
	if firstBatch == 0 {
		t.Fatalf("expected at least one successful send before the " +
			"legacy deadline fired; got 0")
	}
}
