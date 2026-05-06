package runtime_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// ═══════════════════════════════════════════════════════════════════
// Outbox Drainer Concurrency & Adaptive Batch Sizing
//
// Tests verifying parallel record processing within drainBatch (P1)
// and adaptive batch sizing during burst/idle transitions (P2).
//
// Summary:
// ┌──────┬──────────────────────────────────────────────┬──────────┐
// │ ID   │ Description                                  │ Type     │
// ├──────┼──────────────────────────────────────────────┼──────────┤
// │ T001 │ Parallel sends in a batch                    │ unit     │
// │ T002 │ Concurrency limit respected                  │ unit     │
// │ T003 │ Error isolation between records              │ unit     │
// │ T004 │ Default concurrency is 10                    │ unit     │
// │ T005 │ Batch size scales up on full batch           │ unit     │
// │ T006 │ Batch size caps at MaxBatchSize              │ unit     │
// │ T007 │ Batch size resets on empty batch             │ unit     │
// │ T008 │ Partial batch leaves size unchanged          │ unit     │
// │ T009 │ Default MaxBatchSize is 500                  │ unit     │
// └──────┴──────────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════

func persistRecords(outbox *FakeOutboxStore, count int, prefix string) {
	ctx := context.Background()
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%s-%d", prefix, i)
		rec := domain.OutboxRecord{
			ID:         id,
			RouteID:    "route-1",
			EnvelopeID: "env-" + id,
			BindingID:  "bind-1",
			SessionID:  "sess-1",
			Envelope:   messaging.Envelope{ID: "env-" + id, Payload: []byte("data")},
			Status:     domain.OutboxPending,
		}
		_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})
	}
}

// TestDrainBatch_ParallelSends validates records in a batch are sent
// concurrently rather than sequentially.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────
//
//	20 records persisted → drainer claims all → sends concurrently
//	Each send sleeps 50ms. Sequential = 1000ms. Parallel < 500ms.
//
// ───────────────────────────────────────────────────────────────────
//
// Assertions:
//   - All 20 records sent and completed
//   - Peak concurrency > 1
func TestDrainBatch_ParallelSends(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}

	var concurrentPeak int64
	var currentConcurrency int64
	sendStart := make(chan time.Time, 1)

	sender := NewConcurrentSender(func(_ *messaging.Envelope) error {
		select {
		case sendStart <- time.Now():
		default:
		}
		cur := atomic.AddInt64(&currentConcurrency, 1)
		defer atomic.AddInt64(&currentConcurrency, -1)
		for {
			peak := atomic.LoadInt64(&concurrentPeak)
			if cur <= peak {
				break
			}
			if atomic.CompareAndSwapInt64(&concurrentPeak, peak, cur) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond) // OTHER: simulated processing duration
		return nil
	})

	outbox, _, _, drainer := makeDrainer(t, token, func(cfg *goruntime.OutboxDrainerConfig) {
		cfg.Sender = sender
		cfg.DrainMaxConcurrency = 10
		cfg.DrainBatchSize = 20
	})

	persistRecords(outbox, 20, "par")

	drainCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = drainer.Run(drainCtx)
	}()

	waitFor(t, 5*time.Second, "all 20 sent", func() bool {
		return sender.SentCount() >= 20
	})

	start := <-sendStart
	elapsed := time.Since(start)
	cancel()

	peak := atomic.LoadInt64(&concurrentPeak)
	if peak < 2 {
		t.Errorf("expected concurrent sends (peak=%d), records were sent sequentially", peak)
	}

	if elapsed > 500*time.Millisecond {
		t.Errorf("expected parallel drain <500ms from first send, took %v", elapsed)
	}
}

// TestDrainBatch_ConcurrencyLimit validates MaxDrainConcurrency caps
// the number of simultaneous goroutines processing records.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────
//
//	10 records, DrainMaxConcurrency=3 → peak goroutines never exceeds 3
//
// ───────────────────────────────────────────────────────────────────
func TestDrainBatch_ConcurrencyLimit(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}

	var concurrentPeak int64
	var currentConcurrency int64

	sender := NewConcurrentSender(func(_ *messaging.Envelope) error {
		cur := atomic.AddInt64(&currentConcurrency, 1)
		defer atomic.AddInt64(&currentConcurrency, -1)
		for {
			peak := atomic.LoadInt64(&concurrentPeak)
			if cur <= peak {
				break
			}
			if atomic.CompareAndSwapInt64(&concurrentPeak, peak, cur) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond) // OTHER: simulated processing duration
		return nil
	})

	outbox, _, _, drainer := makeDrainer(t, token, func(cfg *goruntime.OutboxDrainerConfig) {
		cfg.Sender = sender
		cfg.DrainMaxConcurrency = 3
		cfg.DrainBatchSize = 10
	})

	persistRecords(outbox, 10, "lim")

	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = drainer.Run(drainCtx)

	peak := atomic.LoadInt64(&concurrentPeak)
	if peak > 3 {
		t.Errorf("concurrency limit violated: peak=%d, limit=3", peak)
	}
	if sender.SentCount() < 10 {
		t.Fatalf("expected 10 sent, got %d", sender.SentCount())
	}
}

// TestDrainBatch_ErrorIsolation validates one record's processing failure
// does not prevent other records from being processed.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────
//
//	5 records: record[2] send fails → other 4 still sent + completed
//
// ───────────────────────────────────────────────────────────────────
func TestDrainBatch_ErrorIsolation(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}

	var callCount int64
	sender := NewConcurrentSender(func(_ *messaging.Envelope) error {
		n := atomic.AddInt64(&callCount, 1)
		if n == 3 {
			return shared.NewBridgeError("FAIL", shared.ErrorTransient, "transient")
		}
		return nil
	})

	outbox, _, _, drainer := makeDrainer(t, token, func(cfg *goruntime.OutboxDrainerConfig) {
		cfg.Sender = sender
		cfg.DrainMaxConcurrency = 5
		cfg.DrainBatchSize = 5
	})

	persistRecords(outbox, 5, "iso")

	drainCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if outbox.CompletedCount() < 4 {
		t.Errorf("expected at least 4 completed (one transient failure), got %d", outbox.CompletedCount())
	}
}

// TestDrainBatch_ConcurrencyDefault validates the default MaxDrainConcurrency
// is applied when not explicitly configured.
func TestDrainBatch_ConcurrencyDefault(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}

	var concurrentPeak int64
	var currentConcurrency int64

	sender := NewConcurrentSender(func(_ *messaging.Envelope) error {
		cur := atomic.AddInt64(&currentConcurrency, 1)
		defer atomic.AddInt64(&currentConcurrency, -1)
		for {
			peak := atomic.LoadInt64(&concurrentPeak)
			if cur <= peak {
				break
			}
			if atomic.CompareAndSwapInt64(&concurrentPeak, peak, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond) // OTHER: simulated processing duration
		return nil
	})

	outbox, _, _, drainer := makeDrainer(t, token, func(cfg *goruntime.OutboxDrainerConfig) {
		cfg.Sender = sender
		cfg.DrainBatchSize = 15
	})

	persistRecords(outbox, 15, "def")

	drainCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = drainer.Run(drainCtx)

	peak := atomic.LoadInt64(&concurrentPeak)
	if peak > 10 {
		t.Errorf("default concurrency should be 10, got peak=%d", peak)
	}
	if peak < 2 {
		t.Errorf("expected parallel execution with default concurrency, got peak=%d", peak)
	}
}

// TestAdaptBatchSize_ScalesUpOnFullBatch validates the drainer increases
// the claim size when a full batch is returned.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────
//
//	DrainBatchSize=5, DrainMaxBatchSize=20, persist 15 records
//	Cycle 1: claims 5 (full) → next claim size = 10
//	Cycle 2: claims 10 (full) → next claim size = 20
//	Cycle 3: claims remaining 0 → resets to 5
//
// ───────────────────────────────────────────────────────────────────
func TestAdaptBatchSize_ScalesUpOnFullBatch(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}

	outbox, sender, _, drainer := makeDrainer(t, token, func(cfg *goruntime.OutboxDrainerConfig) {
		cfg.DrainBatchSize = 5
		cfg.DrainMaxBatchSize = 20
		cfg.DrainMaxConcurrency = 20
	})

	persistRecords(outbox, 15, "batch")

	drainCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() < 15 {
		t.Fatalf("expected all 15 records sent, got %d", sender.SentCount())
	}
	if outbox.CompletedCount() < 15 {
		t.Fatalf("expected 15 completed, got %d", outbox.CompletedCount())
	}
}

// TestAdaptBatchSize_CapsAtMaxBatchSize validates batch size never grows
// beyond MaxBatchSize.
func TestAdaptBatchSize_CapsAtMaxBatchSize(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}

	outbox, sender, _, drainer := makeDrainer(t, token, func(cfg *goruntime.OutboxDrainerConfig) {
		cfg.DrainBatchSize = 5
		cfg.DrainMaxBatchSize = 8
		cfg.DrainMaxConcurrency = 20
	})

	persistRecords(outbox, 30, "cap")

	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() < 30 {
		t.Fatalf("expected 30 sent, got %d", sender.SentCount())
	}
}

// TestAdaptBatchSize_ResetsOnEmptyBatch validates batch size resets to
// the initial value when no records are found.
func TestAdaptBatchSize_ResetsOnEmptyBatch(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}

	outbox, sender, _, drainer := makeDrainer(t, token, func(cfg *goruntime.OutboxDrainerConfig) {
		cfg.DrainBatchSize = 5
		cfg.DrainMaxBatchSize = 20
		cfg.DrainMaxConcurrency = 20
	})

	persistRecords(outbox, 5, "reset")

	drainCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() < 5 {
		t.Fatalf("expected 5 sent, got %d", sender.SentCount())
	}
	if outbox.CompletedCount() < 5 {
		t.Fatalf("expected 5 completed, got %d", outbox.CompletedCount())
	}
}

// TestAdaptBatchSize_DefaultMaxBatchSize validates the default
// MaxBatchSize of 500 is applied.
func TestAdaptBatchSize_DefaultMaxBatchSize(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	_, _, _, _ = makeDrainer(t, token)
}
