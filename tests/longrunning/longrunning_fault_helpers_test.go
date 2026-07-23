//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
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

func (s *failFirstNSender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	env := msg.Envelope
	s.mu.Lock()
	s.attempts[env.ID()]++
	n := s.attempts[env.ID()]
	s.mu.Unlock()
	if n <= s.maxFails {
		return shared.ErrUnavailable.WithMessage(
			fmt.Sprintf("failFirstN: attempt %d/%d for %s", n, s.maxFails, env.ID()))
	}
	return s.inner.Send(ctx, msg)
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

func (s *countingSender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	err := s.inner.Send(ctx, msg)
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
	rngMu       sync.Mutex
	rng         *rand.Rand
}

// newDegradedSender seeds the PRNG with a fixed constant (not the global
// unseeded source), removing run-to-run RANDOMNESS as a variable. It does not
// make outcomes fully reproducible under concurrency: both which message gets
// which draw AND how many draws are consumed (redelivery counts) depend on
// scheduling. That is fine here — the RES001 assertions are order-independent
// safety/liveness properties (recoverable failures are never DLQ'd; the pipeline
// keeps making progress), so they hold regardless of draw assignment. The fixed
// seed just makes a given environment consistently pass or fail rather than
// flaking intermittently on RNG luck.
func newDegradedSender(inner ports.Sender, failPct int, latency time.Duration) *degradedSender {
	return &degradedSender{
		inner: inner, failPercent: failPct, latency: latency,
		rng: rand.New(rand.NewPCG(0x9e3779b97f4a7c15, 0x243f6a8885a308d3)),
	}
}

func (s *degradedSender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	if s.latency > 0 {
		select {
		case <-time.After(s.latency):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.calls.Add(1)
	s.rngMu.Lock()
	fail := s.rng.IntN(100) < s.failPercent
	s.rngMu.Unlock()
	if fail {
		return shared.ErrUnavailable.WithMessage("degraded sender: injected failure")
	}
	return s.inner.Send(ctx, msg)
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

func (s *slowDLQStore) Write(ctx context.Context, entry routing.DLQEntry) error {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.inner.Write(ctx, entry)
}

func (s *slowDLQStore) List(ctx context.Context, f routing.DLQFilter) ([]routing.DLQEntry, error) {
	return s.inner.List(ctx, f)
}

func (s *slowDLQStore) Get(ctx context.Context, id string) (routing.DLQEntry, error) {
	return s.inner.Get(ctx, id)
}

func (s *slowDLQStore) Delete(ctx context.Context, ids []string) (int, error) {
	return s.inner.Delete(ctx, ids)
}

func (s *slowDLQStore) DeleteByFilter(ctx context.Context, filter routing.DLQFilter) (int, error) {
	return s.inner.DeleteByFilter(ctx, filter)
}

func (s *slowDLQStore) Purge(ctx context.Context, before time.Time) (int, error) {
	return s.inner.Purge(ctx, before)
}

// ---------------------------------------------------------------------------
// replayableDLQStore — extends lrDLQStore with working List and Delete
// ---------------------------------------------------------------------------

type replayableDLQStore struct {
	lrDLQStore
}

func (s *replayableDLQStore) List(_ context.Context, filter routing.DLQFilter) ([]routing.DLQEntry, error) {
	entries := s.getEntries()
	var result []routing.DLQEntry
	for _, e := range entries {
		if filter.RouteID != "" && e.RouteID() != filter.RouteID {
			continue
		}
		if filter.Category != "" && e.Category() != filter.Category {
			continue
		}
		result = append(result, e)
		if filter.Limit > 0 && len(result) >= filter.Limit {
			break
		}
	}
	return result, nil
}

func (s *replayableDLQStore) Delete(_ context.Context, ids []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		idSet[id] = struct{}{}
	}
	var remaining []routing.DLQEntry
	var count int
	for _, e := range s.entries {
		if _, ok := idSet[e.ID()]; ok {
			count++
		} else {
			remaining = append(remaining, e)
		}
	}
	s.entries = remaining
	return count, nil
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
	ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc,
) error {
	c := p.count.Add(1)
	if c%int64(p.n) == 0 {
		return shared.ErrInvalidPayload.WithMessage(
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
	ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc,
) error {
	n := p.count.Add(1)
	if n%int64(p.panicEvery) == 0 {
		panic(fmt.Sprintf("panicProcessor: deliberate panic on message %d", n))
	}
	return next(ctx, env)
}
