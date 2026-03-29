//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// ---------------------------------------------------------------------------
// failFirstNSender — fails first N attempts per message ID, then succeeds
// ---------------------------------------------------------------------------

type failFirstNSender struct {
	inner    ports.Sender
	maxFails int
	mu       sync.Mutex
	attempts map[string]int
}

func newFailFirstNSender(inner ports.Sender, maxFails int) *failFirstNSender {
	return &failFirstNSender{inner: inner, maxFails: maxFails, attempts: make(map[string]int)}
}

func (s *failFirstNSender) Send(ctx context.Context, env *domain.Envelope) error {
	s.mu.Lock()
	s.attempts[env.ID]++
	n := s.attempts[env.ID]
	s.mu.Unlock()
	if n <= s.maxFails {
		return domain.ErrUnavailable.WithMessage(
			fmt.Sprintf("failFirstN: attempt %d/%d for %s", n, s.maxFails, env.ID))
	}
	return s.inner.Send(ctx, env)
}

// ---------------------------------------------------------------------------
// countingSender — wraps a sender and counts successes/failures
// ---------------------------------------------------------------------------

type countingSender struct {
	inner    ports.Sender
	success  atomic.Int64
	failures atomic.Int64
}

func newCountingSender(inner ports.Sender) *countingSender {
	return &countingSender{inner: inner}
}

func (s *countingSender) Send(ctx context.Context, env *domain.Envelope) error {
	err := s.inner.Send(ctx, env)
	if err != nil {
		s.failures.Add(1)
	} else {
		s.success.Add(1)
	}
	return err
}

// ---------------------------------------------------------------------------
// degradedSender — configurable failure rate + latency injection
// ---------------------------------------------------------------------------

type degradedSender struct {
	inner       ports.Sender
	failPercent int
	latency     time.Duration
	calls       atomic.Int64
}

func newDegradedSender(inner ports.Sender, failPct int, latency time.Duration) *degradedSender {
	return &degradedSender{inner: inner, failPercent: failPct, latency: latency}
}

func (s *degradedSender) Send(ctx context.Context, env *domain.Envelope) error {
	if s.latency > 0 {
		select {
		case <-time.After(s.latency):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	n := s.calls.Add(1)
	if int(n%100) < s.failPercent {
		return domain.ErrUnavailable.WithMessage("degraded sender: injected failure")
	}
	return s.inner.Send(ctx, env)
}

// ---------------------------------------------------------------------------
// slowDLQStore — wraps a DLQ store with configurable write delay
// ---------------------------------------------------------------------------

type slowDLQStore struct {
	inner ports.DLQStore
	delay time.Duration
}

func newSlowDLQStore(inner ports.DLQStore, delay time.Duration) *slowDLQStore {
	return &slowDLQStore{inner: inner, delay: delay}
}

func (s *slowDLQStore) Write(ctx context.Context, entry domain.DLQEntry) error {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.inner.Write(ctx, entry)
}

func (s *slowDLQStore) List(ctx context.Context, f domain.DLQFilter) ([]domain.DLQEntry, error) {
	return s.inner.List(ctx, f)
}

func (s *slowDLQStore) Replay(ctx context.Context, ids []string) error {
	return s.inner.Replay(ctx, ids)
}

func (s *slowDLQStore) Purge(ctx context.Context, before time.Time) (int, error) {
	return s.inner.Purge(ctx, before)
}

// ---------------------------------------------------------------------------
// replayableDLQStore — extends lrDLQStore with working List and Replay
// ---------------------------------------------------------------------------

type replayableDLQStore struct {
	lrDLQStore
}

func (s *replayableDLQStore) List(_ context.Context, filter domain.DLQFilter) ([]domain.DLQEntry, error) {
	entries := s.getEntries()
	var result []domain.DLQEntry
	for _, e := range entries {
		if filter.RouteID != "" && e.RouteID != filter.RouteID {
			continue
		}
		if filter.Category != "" && e.Category != filter.Category {
			continue
		}
		result = append(result, e)
		if filter.Limit > 0 && len(result) >= filter.Limit {
			break
		}
	}
	return result, nil
}

func (s *replayableDLQStore) Replay(_ context.Context, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		idSet[id] = struct{}{}
	}
	var remaining []domain.DLQEntry
	for _, e := range s.entries {
		if _, ok := idSet[e.ID]; !ok {
			remaining = append(remaining, e)
		}
	}
	s.entries = remaining
	return nil
}

// ---------------------------------------------------------------------------
// rejectEveryNthProcessor — rejects every Nth message (permanent error)
// ---------------------------------------------------------------------------

type rejectEveryNthProcessor struct {
	n     int
	count atomic.Int64
}

func (p *rejectEveryNthProcessor) Name() string { return "reject-every-nth" }

func (p *rejectEveryNthProcessor) Process(
	ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc,
) error {
	c := p.count.Add(1)
	if c%int64(p.n) == 0 {
		return domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("reject-every-%d: msg %d rejected", p.n, c))
	}
	return next(ctx, env)
}

// ---------------------------------------------------------------------------
// panicProcessor — panics on every Nth message
// ---------------------------------------------------------------------------

type panicProcessor struct {
	panicEvery int
	count      atomic.Int64
}

func (p *panicProcessor) Name() string { return "panic-processor" }

func (p *panicProcessor) Process(
	ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc,
) error {
	n := p.count.Add(1)
	if n%int64(p.panicEvery) == 0 {
		panic(fmt.Sprintf("panicProcessor: deliberate panic on message %d", n))
	}
	return next(ctx, env)
}
