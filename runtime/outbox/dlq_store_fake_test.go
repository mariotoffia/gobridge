package outbox

import (
	"context"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
)

// fakeDLQStore is a minimal ports.DLQStore for drainer tests that need a router
// whose HasStore() reports true so the DLQ path (not the H3 drop path) runs. It
// records Write calls so a test can assert a durable DLQ entry was written, and
// can be primed to fail Write to exercise the fail-closed (no-Complete) branch.
// All reader/admin methods return zero values — the drainer never reads.
type fakeDLQStore struct {
	mu       sync.Mutex
	written  []routing.DLQEntry
	writeErr error
}

var _ ports.DLQStore = (*fakeDLQStore)(nil)

func (s *fakeDLQStore) Write(_ context.Context, entry routing.DLQEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	s.written = append(s.written, entry)
	return nil
}

// writes reports how many DLQ entries were durably written.
func (s *fakeDLQStore) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.written)
}

func (s *fakeDLQStore) Get(context.Context, string) (routing.DLQEntry, error) {
	return routing.DLQEntry{}, nil
}

func (s *fakeDLQStore) List(context.Context, routing.DLQFilter) ([]routing.DLQEntry, error) {
	return nil, nil
}

func (s *fakeDLQStore) Delete(context.Context, []string) (int, error) { return 0, nil }

func (s *fakeDLQStore) DeleteByFilter(context.Context, routing.DLQFilter) (int, error) {
	return 0, nil
}

func (s *fakeDLQStore) Purge(context.Context, time.Time) (int, error) { return 0, nil }
