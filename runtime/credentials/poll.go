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

// DefaultReactiveReResolveInterval is the minimum wall-clock spacing between
// two reactive (auth-failure-triggered) re-resolves for the same URI. It rate
// limits PollBasedWrapper.Refresh so a broker reconnect storm — every session
// bound to a URI reporting NOT_AUTHORIZED at once — collapses into at most one
// out-of-band backend fetch per interval instead of hammering the secrets
// backend.
const DefaultReactiveReResolveInterval = 5 * time.Second

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
	metrics    ports.MetricsExporter
	onRotation func(uri string)

	// rngMu guards rng: Watch spawns one goroutine per watched URI and
	// all of them draw jitter from the same source (math/rand.Rand is
	// not safe for concurrent use).
	rngMu sync.Mutex
	rng   *rand.Rand

	// reactiveMinInterval bounds how often Refresh may force an out-of-band
	// re-resolve for one URI (see DefaultReactiveReResolveInterval).
	reactiveMinInterval time.Duration
	// reactiveMu guards nudges and lastReactive.
	reactiveMu sync.Mutex
	// nudges maps a watched URI to the per-watch reactive-refresh signal
	// channels. Refresh sends a non-blocking nudge to each so the watch
	// goroutine resolves immediately instead of waiting for its timer.
	nudges map[string][]chan struct{}
	// lastReactive records the clock time of the last honoured Refresh per URI
	// so a reconnect storm is rate limited to one backend fetch per interval.
	lastReactive map[string]time.Time
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

// WithPollMetrics attaches a metrics exporter so the wrapper can emit
// shared.MetricCredentialRefreshFailures whenever a poll resolve fails.
// A nil exporter is ignored; the wrapper defaults to a no-op exporter.
func WithPollMetrics(m ports.MetricsExporter) PollBasedWrapperOption {
	return func(w *PollBasedWrapper) {
		if m != nil {
			w.metrics = m
		}
	}
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

// WithReactiveReResolveInterval overrides the minimum spacing between reactive
// (auth-failure-triggered) re-resolves for one URI. Values <= 0 are ignored;
// the default is DefaultReactiveReResolveInterval. Tests pass a small value
// with a fake clock to exercise the rate limiter deterministically.
func WithReactiveReResolveInterval(d time.Duration) PollBasedWrapperOption {
	return func(w *PollBasedWrapper) {
		if d > 0 {
			w.reactiveMinInterval = d
		}
	}
}

// NewPollBasedWrapper wraps pull as a PushCredentialStore. pull must be
// non-nil; Watch returns an error otherwise. The wrapper itself holds
// no global state — all polling state lives inside the per-watch
// goroutine spawned by Watch.
func NewPollBasedWrapper(pull ports.PullCredentialStore, cfg ports.PollBasedWrapperConfig, opts ...PollBasedWrapperOption) *PollBasedWrapper {
	w := &PollBasedWrapper{
		pull:                pull,
		cfg:                 cfg,
		clk:                 clock.System,
		metrics:             &ports.NoopExporter{},
		reactiveMinInterval: DefaultReactiveReResolveInterval,
		nudges:              make(map[string][]chan struct{}),
		lastReactive:        make(map[string]time.Time),
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

// Refresh forces an immediate, out-of-band re-resolve of uri (bypassing the
// poll timer) on every watch goroutine currently watching it. It is the
// reactive-recovery hook for a hard credential rotation: when a live transport
// observes a broker authorization failure (shared.ErrNotAuthorized), calling
// Refresh re-resolves the secret NOW instead of leaving the session stuck on
// revoked credentials for up to a full PollInterval (default 5m).
//
// Refresh is safe for concurrent use, idempotent (a nudge already pending is
// coalesced), and rate limited per URI to at most one honoured call per
// reactiveMinInterval so a reconnect storm cannot hammer the secrets backend.
// It is a no-op for an unknown or unwatched URI.
func (w *PollBasedWrapper) Refresh(uri string) {
	if w == nil || uri == "" {
		return
	}
	w.reactiveMu.Lock()
	now := w.clk.Now()
	if last, ok := w.lastReactive[uri]; ok && now.Sub(last) < w.reactiveMinInterval {
		w.reactiveMu.Unlock()
		return
	}
	chans := w.nudges[uri]
	if len(chans) == 0 {
		// No watcher to nudge; do NOT record the timestamp so the first real
		// watcher's Refresh is honoured immediately.
		w.reactiveMu.Unlock()
		return
	}
	w.lastReactive[uri] = now
	snapshot := make([]chan struct{}, len(chans))
	copy(snapshot, chans)
	w.reactiveMu.Unlock()

	for _, ch := range snapshot {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (w *PollBasedWrapper) registerNudge(uri string, ch chan struct{}) {
	w.reactiveMu.Lock()
	w.nudges[uri] = append(w.nudges[uri], ch)
	w.reactiveMu.Unlock()
}

func (w *PollBasedWrapper) unregisterNudge(uri string, ch chan struct{}) {
	w.reactiveMu.Lock()
	defer w.reactiveMu.Unlock()
	lst := w.nudges[uri]
	for i, c := range lst {
		if c == ch {
			w.nudges[uri] = append(lst[:i:i], lst[i+1:]...)
			break
		}
	}
	if len(w.nudges[uri]) == 0 {
		delete(w.nudges, uri)
		delete(w.lastReactive, uri)
	}
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

// runWatch is the per-watch loop. It is structured so the seed resolve happens
// immediately (so EmitOnStart and the build-window rotation check work even
// with a large PollInterval), after which polls happen on the cadence or
// whenever Refresh nudges it. When the wrapped store exposes ResolveUncached,
// every poll bypasses the store's cache — see UncachedPullCredentialStore.
func (w *PollBasedWrapper) runWatch(ctx context.Context, uri string, out chan *connectivity.CredentialSet) {
	defer close(out)

	nudge := make(chan struct{}, 1)
	w.registerNudge(uri, nudge)
	defer w.unregisterNudge(uri, nudge)

	// cachedResolve reads through any read-side cache the store keeps (a cache
	// hit at seed time, since Build resolved the same URI moments earlier).
	// resolve is the poll read: uncached when the store supports it so a
	// rotation is detected within one interval regardless of cache TTL.
	cachedResolve := w.pull.Resolve
	resolve := w.pull.Resolve
	uncachedCapable := false
	if uncached, ok := w.pull.(UncachedPullCredentialStore); ok {
		resolve = uncached.ResolveUncached
		uncachedCapable = true
	}

	last := w.seed(ctx, uri, out, cachedResolve, resolve, uncachedCapable)

	timer := w.clk.NewTimer(w.nextDelay())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
		case <-nudge:
			// Reactive re-resolve requested (e.g. a broker NOT_AUTHORIZED).
			// Fall through to an immediate uncached resolve; the timer is reset
			// below so the periodic cadence resumes from now.
		}

		creds, err := resolve(ctx, uri)
		if err != nil {
			w.metrics.Counter(shared.MetricCredentialRefreshFailures, 1)
			if w.logger != nil {
				w.logger.Warn("credential poll: resolve failed", "uri", shared.RedactURI(uri), "error", shared.RedactURIError(err))
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

// seed establishes the dedup baseline and performs the initial emission. It
// returns the baseline CredentialSet (may be nil on a seed failure).
//
// For a caching pull store (UncachedPullCredentialStore) it closes
// rebuild blind window: the baseline is read through the CACHE (what the
// freshly built session actually holds) and compared against a FRESH uncached
// read. If they differ a rotation landed in the build→watch window, so it is
// surfaced immediately as a rotation — even when EmitOnStart is false —
// instead of being silently baselined away. The cached read is a cache hit,
// not an extra backend round-trip, so this adds no fetch beyond the uncached
// seed the loop already needed.
func (w *PollBasedWrapper) seed(
	ctx context.Context,
	uri string,
	out chan *connectivity.CredentialSet,
	cachedResolve, resolve func(context.Context, string) (*connectivity.CredentialSet, error),
	uncachedCapable bool,
) (last *connectivity.CredentialSet) {
	var built *connectivity.CredentialSet
	if uncachedCapable {
		// Best-effort: the build-time cached value the session was constructed
		// with. Errors are non-fatal — a missing baseline just falls back to
		// EmitOnStart semantics below.
		built, _ = cachedResolve(ctx, uri)
	}

	fresh, err := resolve(ctx, uri)
	if err != nil {
		w.metrics.Counter(shared.MetricCredentialRefreshFailures, 1)
		if w.logger != nil {
			w.logger.Warn("credential poll: initial resolve failed", "uri", shared.RedactURI(uri), "error", shared.RedactURIError(err))
		}
		return nil
	}
	if fresh == nil {
		return nil
	}

	switch {
	case built != nil && !fresh.Equal(built):
		// a rotation landed between the cache-backed build resolve and this
		// watch seed. Surface it now so the new session is corrected off the
		// rotated-out secret, regardless of EmitOnStart. Invalidate caches
		// before publishing (mirrors the tick path).
		if w.onRotation != nil {
			w.onRotation(uri)
		}
		if !send(ctx, out, fresh) {
			return fresh
		}
	case w.cfg.EmitOnStart:
		// Initial snapshot (not a rotation): the OnRotation callback is
		// deliberately not invoked here.
		if !send(ctx, out, fresh) {
			return fresh
		}
	}
	return fresh
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
