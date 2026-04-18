package runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// fixedNoJitterStrategy returns a constant interval without the ±25%
// jitter that FixedPoll applies. Using a jitter-free strategy makes the
// fake-clock advance amounts exact, so the test can assert single-batch
// precision.
type fixedNoJitterStrategy struct {
	d time.Duration
}

func (f fixedNoJitterStrategy) NextInterval(_ int) time.Duration { return f.d }

// TestOutboxDrainer_PollInterval_FakeClock verifies that the drain loop
// is driven entirely by the injected Clock: no batches are processed
// until the fake clock advances by the configured strategy interval, and
// a subsequent batch fires only after the next advance.
//
// Timeline (fake clock):
//
//	T+0s   — drainer Run() starts, registers timer for 1s
//	T+1s   — first advance fires timer, batch #1 is drained
//	T+2s   — second advance fires timer, batch #2 is drained
func TestOutboxDrainer_PollInterval_FakeClock(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	const interval = 1 * time.Second
	const ownerID = "bridge-1"
	const sessionID = "sess-clock-poll"
	token := domain.LeaseToken{Version: 1, Owner: ownerID}

	outbox := NewFakeOutboxStore()
	sender := NewFakeSender()
	leaseStore := NewFakeLeaseStore()
	dlqStore := NewFakeDLQStore()

	pk := domain.OutboxPartitionKey(sessionID, "")
	if _, err := leaseStore.Acquire(context.Background(), sessionID, ownerID, 30*time.Second, nil); err != nil {
		t.Fatalf("acquire lease: %v", err)
	}

	persistRecord := func(id, envID string) {
		rec := domain.OutboxRecord{
			ID:         id,
			RouteID:    "route-1",
			EnvelopeID: envID,
			BindingID:  "bind-1",
			SessionID:  sessionID,
			Envelope:   domain.Envelope{ID: envID, Payload: []byte("payload")},
			Status:     domain.OutboxPending,
		}
		if err := outbox.Persist(context.Background(), []domain.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist %s: %v", id, err)
		}
	}

	persistRecord("rec-1", "env-1")

	batchCh := make(chan int, 8)
	cfg := goruntime.OutboxDrainerConfig{
		OutboxStore:         outbox,
		LeaseStore:          leaseStore,
		Sender:              sender,
		DLQ:                 goruntime.NewDLQRouter(dlqStore),
		RouteID:             "route-1",
		PartitionKey:        pk,
		LeaseID:             sessionID,
		OwnerID:             ownerID,
		Policy:              domain.RoutePolicy{}.WithDefaults(),
		Strategy:            fixedNoJitterStrategy{d: interval},
		DrainBatchSize:      100,
		DrainMaxBatchSize:   100,
		DrainMaxConcurrency: 4,
		DrainTimeout:        5 * time.Second,
		Clock:               fake,
		TokenFn:             func() (domain.LeaseToken, bool) { return token, true },
		OnBatchComplete:     func(n int) { batchCh <- n },
	}
	drainer := goruntime.NewOutboxDrainerFromConfig(cfg)

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

	// OTHER: real-time sync for clocktest.Fake
	// Give the drain loop time to reach its first <-timer.C() select.
	time.Sleep(20 * time.Millisecond)

	// Without advancing the fake clock, no batch must be drained.
	select {
	case n := <-batchCh:
		t.Fatalf("batch fired without clock advance (n=%d)", n)
	case <-time.After(30 * time.Millisecond):
	}

	// ── T+1s — first drain ────────────────────────────────────────────
	fake.Advance(interval)

	select {
	case n := <-batchCh:
		if n != 1 {
			t.Fatalf("first batch: expected 1 successful send, got %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first batch did not fire after 1s advance")
	}

	if got := sender.SentCount(); got != 1 {
		t.Fatalf("after first batch: expected 1 sent, got %d", got)
	}

	// OTHER: real-time sync for clocktest.Fake
	// Give the drain loop time to call timer.Reset(interval) for the
	// next cycle before we advance again.
	time.Sleep(20 * time.Millisecond)

	persistRecord("rec-2", "env-2")

	// Still no batch without another advance.
	select {
	case n := <-batchCh:
		t.Fatalf("second batch fired without clock advance (n=%d)", n)
	case <-time.After(30 * time.Millisecond):
	}

	// ── T+2s — second drain ───────────────────────────────────────────
	fake.Advance(interval)

	select {
	case n := <-batchCh:
		if n != 1 {
			t.Fatalf("second batch: expected 1 successful send, got %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second batch did not fire after 1s advance")
	}

	if got := sender.SentCount(); got != 2 {
		t.Fatalf("after second batch: expected 2 sent, got %d", got)
	}
}
