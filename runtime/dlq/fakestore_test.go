package dlq_test

import (
	"context"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// FakeStore is a thread-safe in-memory [routing.DLQEntry] sink used by
// the dlq package tests. It mirrors the FakeDLQStore helper kept by the
// runtime package's own internal test fixtures, but is duplicated here
// so the dlq subpackage can be tested without reaching into its parent
// (which would invert the inward-only dependency direction the package
// split is designed to enforce).
type FakeStore struct {
	mu       sync.Mutex
	Entries  []routing.DLQEntry
	WriteErr error
}

func NewFakeStore() *FakeStore { return &FakeStore{} }

func (s *FakeStore) Write(_ context.Context, entry routing.DLQEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.WriteErr != nil {
		return s.WriteErr
	}
	s.Entries = append(s.Entries, entry)
	return nil
}

func (s *FakeStore) List(_ context.Context, filter routing.DLQFilter) ([]routing.DLQEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []routing.DLQEntry
	for _, e := range s.Entries {
		if filter.RouteID != "" && e.RouteID() != filter.RouteID {
			continue
		}
		if filter.Category != "" && e.Category() != filter.Category {
			continue
		}
		out = append(out, e)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}

func (s *FakeStore) Get(_ context.Context, id string) (routing.DLQEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.Entries {
		if e.ID() == id {
			return e, nil
		}
	}
	return routing.DLQEntry{}, shared.ErrNotFound
}

func (s *FakeStore) Delete(_ context.Context, ids []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		idSet[id] = struct{}{}
	}
	var remaining []routing.DLQEntry
	var count int
	for _, e := range s.Entries {
		if _, ok := idSet[e.ID()]; ok {
			count++
		} else {
			remaining = append(remaining, e)
		}
	}
	s.Entries = remaining
	return count, nil
}

func (s *FakeStore) DeleteByFilter(_ context.Context, _ routing.DLQFilter) (int, error) {
	return 0, nil
}

func (s *FakeStore) Purge(_ context.Context, _ time.Time) (int, error) { return 0, nil }

func (s *FakeStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Entries)
}
