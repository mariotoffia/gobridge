package runtime_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/runtime"
)

// retryCountingDLQStore fails the first failN writes and succeeds afterwards.
// Each Write call sends the 1-based attempt number on onWrite for test
// synchronisation.
type retryCountingDLQStore struct {
	mu      sync.Mutex
	count   int
	failN   int
	entries []domain.DLQEntry
	onWrite chan int // sends attempt number after each Write
}

func (s *retryCountingDLQStore) Write(_ context.Context, entry domain.DLQEntry) error {
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

func (s *retryCountingDLQStore) storedEntries() []domain.DLQEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.DLQEntry, len(s.entries))
	copy(out, s.entries)
	return out
}

func (s *retryCountingDLQStore) Get(_ context.Context, _ string) (domain.DLQEntry, error) {
	return domain.DLQEntry{}, domain.ErrNotFound
}
func (s *retryCountingDLQStore) List(_ context.Context, _ domain.DLQFilter) ([]domain.DLQEntry, error) {
	return nil, nil
}
func (s *retryCountingDLQStore) Delete(_ context.Context, _ []string) (int, error) { return 0, nil }
func (s *retryCountingDLQStore) DeleteByFilter(_ context.Context, _ domain.DLQFilter) (int, error) {
	return 0, nil
}
func (s *retryCountingDLQStore) Purge(_ context.Context, _ time.Time) (int, error) { return 0, nil }

// TestDLQRouter_RetryBackoff_FakeClock proves that the DLQ router's
// exponential backoff between write retries is driven entirely by the
// injected clock, not wall time.
//
// Backoff policy: Initial=100ms, Multiplier=2.0, Max=1s, Attempts=3.
// The store fails writes 1 and 2, succeeding on write 3.
//
// Timeline (fake clock):
//
//	T+0ms   — attempt 1: immediate, fails
//	T+100ms — attempt 2: after 100ms backoff, fails
//	T+300ms — attempt 3: after 200ms backoff (100ms×2), succeeds
func TestDLQRouter_RetryBackoff_FakeClock(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	store := &retryCountingDLQStore{
		failN:   2,
		onWrite: make(chan int), // unbuffered — worker blocks until test reads
	}

	router := runtime.NewDLQRouterFromConfig(runtime.DLQRouterConfig{
		Store:      store,
		BufferSize: 1,
		Workers:    1,
		Clock:      fake,
		WriteRetryBackoff: domain.BackoffPolicy{
			InitialInterval: 100 * time.Millisecond,
			MaxInterval:     1 * time.Second,
			Multiplier:      2.0,
		},
		WriteMaxAttempts: 3,
		WriteTimeout:     5 * time.Second,
		EnqTimeout:       5 * time.Second,
	})

	ctx := context.Background()
	router.Start(ctx)
	defer router.Close()

	env := &domain.Envelope{
		ID:      "backoff-test-1",
		Subject: "test/backoff",
		Payload: []byte("payload"),
	}

	err := router.Route(ctx, env, "route-1", "bind-1", "sess-1", "src-1", domain.ErrUnavailable, 1)
	require.NoError(t, err)

	// ── Attempt 1 (immediate, no timer wait) ─────────────────────────────
	n := <-store.onWrite
	assert.Equal(t, 1, n, "first write attempt")

	// OTHER: real-time sync for clocktest.Fake
	// The worker returns from Write, loops, and calls clk.After(100ms) to
	// register the backoff timer. A brief wall-clock sleep lets the
	// goroutine reach that point before we touch the fake clock.
	time.Sleep(10 * time.Millisecond)

	// Without advancing the fake clock, the retry must NOT fire.
	select {
	case <-store.onWrite:
		t.Fatal("second attempt fired without clock advance")
	case <-time.After(50 * time.Millisecond):
	}

	// ── Attempt 2 (after 100ms backoff) ──────────────────────────────────
	fake.Advance(100 * time.Millisecond)

	n = <-store.onWrite
	assert.Equal(t, 2, n, "second write attempt after 100ms advance")

	time.Sleep(10 * time.Millisecond) // OTHER: real-time sync for clocktest.Fake

	select {
	case <-store.onWrite:
		t.Fatal("third attempt fired without clock advance")
	case <-time.After(50 * time.Millisecond):
	}

	// ── Attempt 3 (after 200ms backoff = 100ms × 2.0) ───────────────────
	fake.Advance(200 * time.Millisecond)

	n = <-store.onWrite
	assert.Equal(t, 3, n, "third write attempt after 200ms advance")

	// The third attempt succeeds (failN == 2). Verify the entry landed.
	entries := store.storedEntries()
	require.Len(t, entries, 1, "entry should be persisted after third attempt")
	assert.Equal(t, "backoff-test-1", entries[0].Envelope.ID)
}
