package dlq_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// retryCountingDLQStore fails the first failN writes and succeeds afterwards.
// Each Write call sends the 1-based attempt number on onWrite for test
// synchronisation.
type retryCountingDLQStore struct {
	mu      sync.Mutex
	count   int
	failN   int
	entries []routing.DLQEntry
	onWrite chan int // sends attempt number after each Write
}

func (s *retryCountingDLQStore) Write(_ context.Context, entry routing.DLQEntry) error {
	s.mu.Lock()
	s.count++
	n := s.count
	shouldFail := n <= s.failN
	if !shouldFail {
		s.entries = append(s.entries, entry)
	}
	s.mu.Unlock()

	if s.onWrite != nil {
		s.onWrite <- n
	}

	if shouldFail {
		return fmt.Errorf("fake write error (attempt %d)", n)
	}
	return nil
}

func (s *retryCountingDLQStore) storedEntries() []routing.DLQEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]routing.DLQEntry, len(s.entries))
	copy(out, s.entries)
	return out
}

func (s *retryCountingDLQStore) Get(_ context.Context, _ string) (routing.DLQEntry, error) {
	return routing.DLQEntry{}, shared.ErrNotFound
}
func (s *retryCountingDLQStore) List(_ context.Context, _ routing.DLQFilter) ([]routing.DLQEntry, error) {
	return nil, nil
}
func (s *retryCountingDLQStore) Delete(_ context.Context, _ []string) (int, error) { return 0, nil }
func (s *retryCountingDLQStore) DeleteByFilter(_ context.Context, _ routing.DLQFilter) (int, error) {
	return 0, nil
}
func (s *retryCountingDLQStore) Purge(_ context.Context, _ time.Time) (int, error) { return 0, nil }

// TestRouter_RetryBackoff_FakeClock proves that the DLQ router's
// exponential backoff between synchronous write retries is driven entirely
// by the injected clock, not wall time.
//
// Backoff policy: Initial=100ms, Multiplier=2.0, Max=1s, Attempts=3.
// The store fails writes 1 and 2, succeeding on write 3.
//
// Timeline (fake clock):
//
//	T+0ms   — attempt 1: immediate, fails
//	T+100ms — attempt 2: after 100ms backoff, fails
//	T+300ms — attempt 3: after 200ms backoff (100ms×2), succeeds
//
// Route runs in a goroutine because it now blocks until the write is
// confirmed; the test drives the fake clock between attempts and collects
// the final result from errCh.
func TestRouter_RetryBackoff_FakeClock(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	store := &retryCountingDLQStore{
		failN:   2,
		onWrite: make(chan int), // unbuffered — Route blocks until the test reads
	}

	router := dlq.NewFromConfig(dlq.Config{
		Store: store,
		Clock: fake,
		WriteRetryBackoff: routing.BackoffPolicy{
			InitialInterval: 100 * time.Millisecond,
			MaxInterval:     1 * time.Second,
			Multiplier:      2.0,
		},
		WriteMaxAttempts: 3,
		WriteTimeout:     5 * time.Second,
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "backoff-test-1",
		Subject: "test/backoff",
		Payload: []byte("payload"),
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- router.Route(
			context.Background(), env,
			"route-1", "bind-1", "", "sess-1", "src-1",
			shared.ErrUnavailable, 1,
		)
	}()

	// ── Attempt 1 (immediate, no timer wait) ─────────────────────────────
	n := <-store.onWrite
	assert.Equal(t, 1, n, "first write attempt")

	// The goroutine returns from Write, loops, and calls clk.After(100ms) to
	// register the backoff timer. Wait deterministically for that timer
	// instead of sleeping, then prove it does not fire without an advance.
	require.Eventually(t, func() bool { return fake.TimerCount() == 1 },
		time.Second, time.Millisecond, "backoff timer for attempt 2 not registered")
	select {
	case <-store.onWrite:
		t.Fatal("second attempt fired without clock advance")
	case <-time.After(50 * time.Millisecond):
	}

	// ── Attempt 2 (after 100ms backoff) ──────────────────────────────────
	fake.Advance(100 * time.Millisecond)

	n = <-store.onWrite
	assert.Equal(t, 2, n, "second write attempt after 100ms advance")

	require.Eventually(t, func() bool { return fake.TimerCount() == 1 },
		time.Second, time.Millisecond, "backoff timer for attempt 3 not registered")
	select {
	case <-store.onWrite:
		t.Fatal("third attempt fired without clock advance")
	case <-time.After(50 * time.Millisecond):
	}

	// ── Attempt 3 (after 200ms backoff = 100ms × 2.0) ───────────────────
	fake.Advance(200 * time.Millisecond)

	n = <-store.onWrite
	assert.Equal(t, 3, n, "third write attempt after 200ms advance")

	// The third attempt succeeds (failN == 2). Route returns nil and the
	// entry is durably recorded.
	require.NoError(t, <-errCh, "Route should confirm after the third attempt")
	entries := store.storedEntries()
	require.Len(t, entries, 1, "entry should be persisted after third attempt")
	assert.Equal(t, "backoff-test-1", entries[0].Snapshot().ID())
}
