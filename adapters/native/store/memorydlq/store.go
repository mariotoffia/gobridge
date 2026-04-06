package memorydlq

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.DLQStore = (*Store)(nil)

// Store implements ports.DLQStore in memory for tests and
// single-process mode. It is not safe for clustered production deployments.
type Store struct {
	mu      sync.Mutex
	entries map[string]domain.DLQEntry
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
		entries: make(map[string]domain.DLQEntry),
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

	s.entries[entry.ID] = entry
	return nil
}

func (s *Store) Get(ctx context.Context, id string) (domain.DLQEntry, error) {
	logging.TraceContext(s.logger, ctx, "memorydlq: get", "entryID", id)

	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[id]
	if !ok {
		return domain.DLQEntry{}, domain.ErrNotFound.
			WithMessage("dlq entry not found").
			With("entryID", id)
	}

	return e, nil
}

func (s *Store) List(ctx context.Context, filter domain.DLQFilter) ([]domain.DLQEntry, error) {
	logging.TraceContext(s.logger, ctx, "memorydlq: query",
		"routeID", filter.RouteID, "category", filter.Category, "limit", filter.Limit)

	s.mu.Lock()
	defer s.mu.Unlock()

	var result []domain.DLQEntry
	for _, e := range s.entries {
		if matchesFilter(e, filter) {
			result = append(result, e)
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].FailedAt.After(result[j].FailedAt)
	})

	if filter.Limit > 0 && len(result) > filter.Limit {
		result = result[:filter.Limit]
	}

	return result, nil
}

func (s *Store) Delete(ctx context.Context, ids []string) (int, error) {
	logging.TraceContext(s.logger, ctx, "memorydlq: delete", "count", len(ids))

	s.mu.Lock()
	defer s.mu.Unlock()

	var count int
	for _, id := range ids {
		if _, ok := s.entries[id]; ok {
			delete(s.entries, id)
			count++
		}
	}

	return count, nil
}

func (s *Store) DeleteByFilter(ctx context.Context, filter domain.DLQFilter) (int, error) {
	logging.TraceContext(s.logger, ctx, "memorydlq: delete_by_filter",
		"routeID", filter.RouteID, "category", filter.Category)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Collect matching IDs, sorted by FailedAt descending (newest first)
	// to match SQLite/DynamoDB ordering when Limit is applied.
	var matched []domain.DLQEntry
	for _, e := range s.entries {
		if matchesFilter(e, filter) {
			matched = append(matched, e)
		}
	}

	if filter.Limit > 0 && len(matched) > filter.Limit {
		sort.Slice(matched, func(i, j int) bool {
			return matched[i].FailedAt.After(matched[j].FailedAt)
		})
		matched = matched[:filter.Limit]
	}

	for _, e := range matched {
		delete(s.entries, e.ID)
	}

	return len(matched), nil
}

func (s *Store) Purge(ctx context.Context, before time.Time) (int, error) {
	logging.TraceContext(s.logger, ctx, "memorydlq: purge")

	s.mu.Lock()
	defer s.mu.Unlock()

	var count int
	for id, e := range s.entries {
		if e.FailedAt.Before(before) {
			delete(s.entries, id)
			count++
		}
	}

	return count, nil
}

func matchesFilter(e domain.DLQEntry, filter domain.DLQFilter) bool {
	if filter.RouteID != "" && e.RouteID != filter.RouteID {
		return false
	}
	if filter.Category != "" && e.Category != filter.Category {
		return false
	}
	if !filter.Since.IsZero() && e.FailedAt.Before(filter.Since) {
		return false
	}
	if !filter.Before.IsZero() && !e.FailedAt.Before(filter.Before) {
		return false
	}
	return true
}
