package credentials

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// DefaultCredentialPollInterval is used by PollBasedWrapper when the
// configured interval is zero or negative.
const DefaultCredentialPollInterval = 5 * time.Minute

// UncachedPullCredentialStore is an optional capability of a
// ports.PullCredentialStore: resolving credentials while bypassing any
// read-side cache the store maintains (the store is expected to
// refresh its cache with the fresh value as a side effect).
//
// PollBasedWrapper type-asserts for this capability and prefers it for
// its poll reads. Rationale: when the wrapped store is a caching
// resolver (e.g. the runtime CredentialResolver with its TTL cache),
// polling through the cache can never observe a rotation before the
// TTL expires — a 30s rotation poll against a 5m cache silently
// degrades to a 5m rotation poll. Consumer-side interface per repo
// convention; the runtime CredentialResolver implements it.
type UncachedPullCredentialStore interface {
	// ResolveUncached fetches credentials for uri directly from the
	// backing repository, bypassing any cached value, and refreshes
	// the cache with the result.
	ResolveUncached(ctx context.Context, uri string) (*connectivity.CredentialSet, error)
}

// PollBasedWrapper adapts a ports.PullCredentialStore into a
// ports.PushCredentialStore by invoking Resolve on a timer and emitting
// only when the returned CredentialSet differs from the last observed
// value. Each Watch call spawns its own goroutine and polling loop;
// watchers are independent and do not share cached state.
//
// If the wrapped store implements UncachedPullCredentialStore, the
// poll reads bypass the store's cache so rotations are detected within
// one PollInterval regardless of any cache TTL.
//
// The wrapper takes a clock.Clock so tests can drive time deterministically
// via clocktest.Fake. In production, pass clock.System (the default when
// nil is supplied).
type PollBasedWrapper struct {
	pull       ports.PullCredentialStore
	cfg        ports.PollBasedWrapperConfig
	clk        clock.Clock
	logger     *slog.Logger
	onRotation func(uri string)

	// rngMu guards rng: Watch spawns one goroutine per watched URI and
	// all of them draw jitter from the same source (math/rand.Rand is
	// not safe for concurrent use).
	rngMu sync.Mutex
	rng   *rand.Rand
}

var _ ports.PushCredentialStore = (*PollBasedWrapper)(nil)

// PollBasedWrapperOption configures a PollBasedWrapper.
type PollBasedWrapperOption func(*PollBasedWrapper)

// WithPollClock overrides the clock used for polling. Tests pass a
// clocktest.Fake to advance time synchronously.
func WithPollClock(c clock.Clock) PollBasedWrapperOption {
	return func(w *PollBasedWrapper) {
		if c != nil {
			w.clk = c
		}
	}
}

// WithPollLogger attaches a logger to the wrapper. When nil, the
// wrapper is silent.
func WithPollLogger(l *slog.Logger) PollBasedWrapperOption {
	return func(w *PollBasedWrapper) { w.logger = l }
}

// WithOnRotation registers a callback invoked when a rotation is
// detected for a watched URI — that is, whenever the freshly resolved
// CredentialSet differs from the previously observed one. The callback
// runs synchronously in the watch goroutine BEFORE the rotated
// credentials are published on the Watch channel, so any cache
// invalidation it performs is complete by the time a consumer reacts
// to the emission (e.g. a connection rebuild will re-resolve and see
// the rotated secret, never the stale cache entry).
//
// The callback is NOT invoked for the initial EmitOnStart snapshot —
// that emission is a seed, not a rotation.
//
// Contract for the composition root (bridge/credential_refresh.go):
// wire this to CredentialResolver.InvalidateCache, e.g.
//
//	credentials.NewPollBasedWrapper(resolver, cfg,
//	    credentials.WithOnRotation(resolver.InvalidateCache),
//	)
//
// The callback must be safe for concurrent use: one watch goroutine
// exists per watched URI. A nil fn is ignored.
func WithOnRotation(fn func(uri string)) PollBasedWrapperOption {
	return func(w *PollBasedWrapper) {
		if fn != nil {
			w.onRotation = fn
		}
	}
}

// NewPollBasedWrapper wraps pull as a PushCredentialStore. pull must be
// non-nil; Watch returns an error otherwise. The wrapper itself holds
// no global state — all polling state lives inside the per-watch
// goroutine spawned by Watch.
func NewPollBasedWrapper(pull ports.PullCredentialStore, cfg ports.PollBasedWrapperConfig, opts ...PollBasedWrapperOption) *PollBasedWrapper {
	w := &PollBasedWrapper{
		pull: pull,
		cfg:  cfg,
		clk:  clock.System,
	}
	for _, o := range opts {
		o(w)
	}
	w.rng = rand.New(rand.NewSource(w.clk.Now().UnixNano()))
	if w.cfg.PollInterval <= 0 {
		w.cfg.PollInterval = DefaultCredentialPollInterval
	}
	if w.cfg.Jitter < 0 {
		w.cfg.Jitter = 0
	}
	return w
}

// Watch implements ports.PushCredentialStore. It spawns a single
// goroutine that periodically calls Resolve and publishes on the
// returned channel whenever the new CredentialSet differs from the
// last one. The channel is closed when ctx is cancelled or a resolve
// error occurs that the caller is expected to observe via logs.
//
// If pull is nil, Watch returns context.Canceled-style error so the
// caller can fail fast.
func (w *PollBasedWrapper) Watch(ctx context.Context, uri string) (<-chan *connectivity.CredentialSet, error) {
	if w.pull == nil {
		return nil, shared.ErrUnavailable.WithMessage("PollBasedWrapper: nil pull store")
	}
	if uri == "" {
		return nil, shared.ErrInvalidPayload.WithMessage("PollBasedWrapper: empty URI")
	}

	// Buffered: the receiver may be slow applying credentials. One slot
	// is enough — a second rotation during an in-flight apply replaces
	// the first (coalescing) before the receiver wakes up.
	out := make(chan *connectivity.CredentialSet, 1)

	go w.runWatch(ctx, uri, out)
	// Cast away the directionality: the goroutine needs bidirectional
	// access for coalescing. Callers see read-only per the interface.
	return out, nil
}

// runWatch is the per-watch loop. It is structured so the very first
// iteration fires immediately (so EmitOnStart works even with a tiny
// PollInterval), after which polls happen on the cadence. When the
// wrapped store exposes ResolveUncached, every poll bypasses the
// store's cache — see UncachedPullCredentialStore.
func (w *PollBasedWrapper) runWatch(ctx context.Context, uri string, out chan *connectivity.CredentialSet) {
	defer close(out)

	resolve := w.pull.Resolve
	if uncached, ok := w.pull.(UncachedPullCredentialStore); ok {
		resolve = uncached.ResolveUncached
	}

	var last *connectivity.CredentialSet

	// Seed: resolve once so dedup has a baseline AND so EmitOnStart can
	// publish the initial value. A resolve failure is logged but non-fatal;
	// the loop will retry on the next tick. The seed is not a rotation,
	// so the OnRotation callback is deliberately not invoked here.
	if creds, err := resolve(ctx, uri); err == nil && creds != nil {
		last = creds
		if w.cfg.EmitOnStart {
			if !send(ctx, out, creds) {
				return
			}
		}
	} else if err != nil && w.logger != nil {
		w.logger.Warn("credential poll: initial resolve failed", "uri", uri, "error", err)
	}

	timer := w.clk.NewTimer(w.nextDelay())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
		}

		creds, err := resolve(ctx, uri)
		if err != nil {
			if w.logger != nil {
				w.logger.Warn("credential poll: resolve failed", "uri", uri, "error", err)
			}
			timer.Reset(w.nextDelay())
			continue
		}
		if creds == nil {
			timer.Reset(w.nextDelay())
			continue
		}

		if !creds.Equal(last) {
			last = creds
			// Invalidate caches BEFORE publishing so a consumer that
			// rebuilds on this emission re-resolves the fresh secret.
			if w.onRotation != nil {
				w.onRotation(uri)
			}
			if !send(ctx, out, creds) {
				return
			}
		}
		timer.Reset(w.nextDelay())
	}
}

// nextDelay returns the next poll delay applying ±Jitter if configured.
// Safe for concurrent use by multiple watch goroutines.
func (w *PollBasedWrapper) nextDelay() time.Duration {
	base := w.cfg.PollInterval
	if w.cfg.Jitter <= 0 {
		return base
	}
	// Draw in [-Jitter, +Jitter]. Guard against negative overall delay.
	w.rngMu.Lock()
	delta := time.Duration(w.rng.Int63n(int64(2*w.cfg.Jitter))) - w.cfg.Jitter
	w.rngMu.Unlock()
	d := base + delta
	if d <= 0 {
		d = time.Millisecond
	}
	return d
}

// send publishes creds with coalescing semantics: if the previous value
// has not been read yet, it is discarded in favour of the newer one.
// Returns false when ctx is cancelled while attempting to send.
func send(ctx context.Context, out chan *connectivity.CredentialSet, creds *connectivity.CredentialSet) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case out <- creds:
			return true
		default:
			// Drain a stale pending value so we can replace it. If the
			// consumer raced us and drained it first, the next iteration
			// takes the `out <- creds` branch.
			select {
			case <-out:
			default:
			}
		}
	}
}
