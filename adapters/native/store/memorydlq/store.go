package memorydlq

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/bridge/logging"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.DLQStore = (*Store)(nil)

type dlqEntry struct {
	entry      domain.DLQEntry
	replayedAt time.Time
}

// Store implements ports.DLQStore in memory for tests and
// single-process mode. It is not safe for clustered production deployments.
type Store struct {
	mu      sync.Mutex
	entries map[string]*dlqEntry
	now     func() time.Time
	logger  *slog.Logger
}

// Option configures a Store.
type Option func(*Store)

// WithLogger sets the logger for trace-level diagnostics.
func WithLogger(l *slog.Logger) Option {
	return func(s *Store) { s.logger = l }
}

// WithClock overrides the time source (defaults to time.Now).
func WithClock(fn func() time.Time) Option {
	return func(s *Store) { s.now = fn }
}

// NewStore creates a new in-memory DLQStore.
func NewStore(opts ...Option) *Store {
	s := &Store{
		entries: make(map[string]*dlqEntry),
		now:     time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *Store) Write(ctx context.Context, entry domain.DLQEntry) error {
	logging.TraceContext(s.logger, ctx, "memorydlq: store", "entryID", entry.ID)

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.entries[entry.ID]; ok {
		return domain.ErrDuplicateRecord.
			WithMessage("dlq entry already exists").
			With("entryID", entry.ID)
	}

	s.entries[entry.ID] = &dlqEntry{entry: entry}
	return nil
}

func (s *Store) List(ctx context.Context, filter domain.DLQFilter) ([]domain.DLQEntry, error) {
	logging.TraceContext(s.logger, ctx, "memorydlq: query",
		"routeID", filter.RouteID, "category", filter.Category, "limit", filter.Limit)

	s.mu.Lock()
	defer s.mu.Unlock()

	var result []domain.DLQEntry
	for _, e := range s.entries {
		if filter.RouteID != "" && e.entry.RouteID != filter.RouteID {
			continue
		}
		if filter.Category != "" && e.entry.Category != filter.Category {
			continue
		}
		if !filter.Since.IsZero() && e.entry.FailedAt.Before(filter.Since) {
			continue
		}
		if !filter.Before.IsZero() && !e.entry.FailedAt.Before(filter.Before) {
			continue
		}
		result = append(result, e.entry)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].FailedAt.After(result[j].FailedAt)
	})

	if filter.Limit > 0 && len(result) > filter.Limit {
		result = result[:filter.Limit]
	}

	return result, nil
}

func (s *Store) Replay(_ context.Context, entryIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, id := range entryIDs {
		if _, ok := s.entries[id]; !ok {
			return domain.ErrNotFound.
				WithMessage("dlq entry not found").
				With("entryID", id)
		}
	}

	now := s.now()
	for _, id := range entryIDs {
		s.entries[id].replayedAt = now
	}

	return nil
}

func (s *Store) Purge(ctx context.Context, before time.Time) (int, error) {
	logging.TraceContext(s.logger, ctx, "memorydlq: purge")

	s.mu.Lock()
	defer s.mu.Unlock()

	var count int
	for id, e := range s.entries {
		if e.entry.FailedAt.Before(before) {
			delete(s.entries, id)
			count++
		}
	}

	return count, nil
}
