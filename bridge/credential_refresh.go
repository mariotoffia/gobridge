package bridge

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// DefaultCredentialApplyTimeout bounds a single ApplyCredentials call during a
// rotation fan-out. A transport whose ApplyCredentials honours ctx but is slow
// (or wedged) is cancelled after this so it cannot stall the rotation for other
// targets sharing the URI. Values <= 0 (via WithApplyTimeout) disable the
// per-apply deadline and use the refresher's parent context directly.
const DefaultCredentialApplyTimeout = 15 * time.Second

// reactiveCredentialStore is an optional capability of a
// ports.PushCredentialStore: forcing an immediate, out-of-band re-resolve of a
// URI (bypassing the poll timer). runtime/credentials.PollBasedWrapper
// implements it. The refresher detects it structurally to avoid importing the
// runtime package, mirroring the InvalidateCache capability detection in the
// builder.
type reactiveCredentialStore interface {
	Refresh(uri string)
}

// CredentialAware is implemented by transport sessions that can rotate
// their authentication material at runtime. When a PushCredentialStore
// emits a new *connectivity.CredentialSet on a URI bound to such a session,
// the CredentialRefresher calls ApplyCredentials on it.
//
// Why a capability interface instead of a method on ports.Session: not
// every transport supports hot credential rotation (e.g. HTTP servers
// have no concept of session auth). Keeping this as a capability lets
// transports opt in without enlarging the core Session port.
type CredentialAware interface {
	ApplyCredentials(ctx context.Context, creds *connectivity.CredentialSet) error
}

// CredentialRefresher owns the watcher goroutines that translate push
// rotations into ApplyCredentials calls on transport sessions. One
// Refresher is created per Build() when a PushCredentialStore is
// registered; it is torn down by Close.
type CredentialRefresher struct {
	push    ports.PushCredentialStore
	logger  *slog.Logger
	metrics ports.MetricsExporter

	// applyTimeout bounds each ApplyCredentials call in a rotation fan-out.
	applyTimeout time.Duration

	// onRotation, when set, is invoked with the URI after each rotation is
	// applied. The builder wires it to the CredentialResolver's InvalidateCache
	// so the pull-side cache drops the stale entry and the next synchronous
	// resolve fetches the rotated material (contract C4).
	onRotation func(uri string)

	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// watchers maps a URI to the set of CredentialAware targets that share it.
	// Exactly ONE poller goroutine is started per URI (on first registration)
	// and it fans the rotation out to every target; additional Watch calls for
	// the same URI append to the slice instead of spawning a duplicate poller
	// (Finding 14). A nil slice value means "poller starting/failed"; presence
	// of the key means a poller has been (or is being) established.
	watchers map[string][]CredentialAware
}

// RefresherOption configures a CredentialRefresher.
type RefresherOption func(*CredentialRefresher)

// WithRotationCallback sets a callback invoked with the URI after each applied
// rotation. The builder passes CredentialResolver.InvalidateCache so the pull
// cache is dropped in lock-step with a push rotation (contract C4). nil is
// ignored.
func WithRotationCallback(fn func(uri string)) RefresherOption {
	return func(r *CredentialRefresher) {
		if fn != nil {
			r.onRotation = fn
		}
	}
}

// WithRefresherMetrics sets the metrics exporter used to emit
// MetricCredentialRotationApplied (one per successful target ApplyCredentials).
// Defaults to a no-op exporter. nil is ignored.
func WithRefresherMetrics(m ports.MetricsExporter) RefresherOption {
	return func(r *CredentialRefresher) {
		if m != nil {
			r.metrics = m
		}
	}
}

// WithApplyTimeout overrides DefaultCredentialApplyTimeout, the per-target
// ApplyCredentials deadline used during a rotation fan-out. A value <= 0
// disables the deadline (the parent context is used directly).
func WithApplyTimeout(d time.Duration) RefresherOption {
	return func(r *CredentialRefresher) {
		r.applyTimeout = d
	}
}

// NewCredentialRefresher creates a refresher bound to the given push
// store. push may be nil, in which case Watch is a no-op — callers
// should always construct one and delegate the nil-check here so the
// rest of the code path stays uniform.
func NewCredentialRefresher(push ports.PushCredentialStore, logger *slog.Logger, opts ...RefresherOption) *CredentialRefresher {
	ctx, cancel := context.WithCancel(context.Background())
	r := &CredentialRefresher{
		push:         push,
		logger:       logger,
		metrics:      &ports.NoopExporter{},
		applyTimeout: DefaultCredentialApplyTimeout,
		ctx:          ctx,
		cancel:       cancel,
		watchers:     make(map[string][]CredentialAware),
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// NotifyAuthFailure is the reactive-recovery hook (F2). A live transport that
// observes a broker authorization failure (shared.ErrNotAuthorized) on a
// watched URI calls this to force an IMMEDIATE credential re-resolve instead of
// leaving the session stuck on the revoked credentials for up to a full poll
// interval (default 5m) after a hard rotation.
//
// It is a no-op when err is not an authorization failure, when uri is empty, or
// when the push store does not support reactive refresh. Idempotency and
// per-URI rate limiting live in the push store's Refresh, so a reconnect storm
// (every session on the URI reporting NOT_AUTHORIZED at once) collapses into at
// most one out-of-band backend fetch.
func (r *CredentialRefresher) NotifyAuthFailure(uri string, err error) {
	if r == nil || uri == "" || err == nil {
		return
	}
	if !errors.Is(err, shared.ErrNotAuthorized) {
		return
	}
	if rc, ok := r.push.(reactiveCredentialStore); ok {
		if logging.DebugEnabled(r.logger) {
			r.logger.Log(context.Background(), logging.LevelDebug,
				"credential refresh: auth failure -> forcing reactive re-resolve", "uri", shared.RedactURI(uri))
		}
		rc.Refresh(uri)
	}
}

// Watch registers a (uri, session) pair for refresh. If the same URI
// is registered by multiple targets (e.g. two sessions share
// credentials), a SINGLE poller is spawned for the URI and each
// registered target receives the rotation — duplicate Watch calls no
// longer spawn duplicate pollers (Finding 14).
//
// Sessions that do not implement CredentialAware are silently skipped
// with a debug log — this is intentional so mixed transports (MQTT
// with rotation + HTTP without) can coexist in one bridge without
// forcing HTTP to implement a no-op ApplyCredentials.
func (r *CredentialRefresher) Watch(_ context.Context, uri string, sess ports.Session) {
	r.watchTarget(uri, sess, "session")
}

// WatchReceiver binds a receiver-level credentials_uri to its Receiver.
// The Receiver must implement CredentialAware to receive rotations;
// non-aware receivers are silently skipped (debug log), matching
// session-level behavior. This lets per-endpoint transports (e.g. HTTP
// with per-route auth) observe rotation without forcing session-scoped
// credentials.
func (r *CredentialRefresher) WatchReceiver(_ context.Context, uri string, recv ports.Receiver) {
	r.watchTarget(uri, recv, "receiver")
}

// WatchSender binds a sender-level credentials_uri to its Sender. Same
// contract as WatchReceiver.
func (r *CredentialRefresher) WatchSender(_ context.Context, uri string, snd ports.Sender) {
	r.watchTarget(uri, snd, "sender")
}

func (r *CredentialRefresher) watchTarget(uri string, target any, kind string) {
	if r == nil || r.push == nil || target == nil || uri == "" {
		return
	}
	aware, ok := target.(CredentialAware)
	if !ok {
		if logging.DebugEnabled(r.logger) {
			r.logger.Log(r.ctx, logging.LevelDebug,
				"credential refresh: target does not implement CredentialAware; skipping",
				"uri", shared.RedactURI(uri), "kind", kind)
		}
		return
	}

	r.mu.Lock()
	if r.ctx == nil {
		// Closed: nothing to do.
		r.mu.Unlock()
		return
	}
	parent := r.ctx
	_, alreadyWatching := r.watchers[uri]
	r.watchers[uri] = append(r.watchers[uri], aware)
	r.mu.Unlock()

	if alreadyWatching {
		// A poller already exists for this URI; the target we just appended
		// will be served by it. Do not spawn a duplicate poller (Finding 14).
		return
	}

	ch, err := r.push.Watch(parent, uri)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("credential refresh: Watch failed", "uri", shared.RedactURI(uri), "error", shared.RedactURIError(err))
		}
		// Surface the failed watcher as a metric, not just a log: a failed
		// Watch means rotation is silently disabled for this URI for the whole
		// process lifetime, so it must be observable/alertable rather than
		// permanently silent (credential_refresh.go:221, Chunk 3).
		r.metrics.Counter(shared.MetricCredentialRefreshFailures, 1)
		// Drop the key so a later Watch for the same URI can retry
		// establishing a poller rather than being suppressed as a duplicate.
		r.mu.Lock()
		delete(r.watchers, uri)
		r.mu.Unlock()
		return
	}

	r.wg.Add(1)
	go r.run(parent, uri, ch)
}

func (r *CredentialRefresher) run(
	parent context.Context,
	uri string,
	ch <-chan *connectivity.CredentialSet,
) {
	defer r.wg.Done()
	for {
		select {
		case <-parent.Done():
			return
		case creds, ok := <-ch:
			if !ok {
				// The watcher channel closed on its own while the refresher is
				// still live (parent not cancelled). This is a DEAD watcher:
				// credentials for this URI will never refresh again until the
				// process restarts. Previously this exited silently — surface it
				// with a WARN + a failure metric so a dead credential watcher is
				// observable/alertable instead of a silent auth time-bomb on the
				// next rotation (credential_refresh.go:221, Chunk 3).
				// ponytail: we surface (log + metric) rather than auto-restart
				// with backoff — the push store owns reconnection, and an
				// unconditional re-Watch loop here could hot-spin against a store
				// that closes immediately. Restart is the store's job; the
				// bridge's job is to make the death visible.
				if parent.Err() == nil {
					r.metrics.Counter(shared.MetricCredentialRefreshFailures, 1)
					if r.logger != nil {
						r.logger.Warn("credential refresh: watcher channel closed unexpectedly; "+
							"credential rotation for this URI is now disabled until restart",
							"uri", shared.RedactURI(uri))
					}
				}
				return
			}
			if creds == nil {
				continue
			}
			// Snapshot the fan-out targets under lock; the slice may grow while
			// the poller runs (targets registering during build).
			r.mu.Lock()
			targets := make([]CredentialAware, len(r.watchers[uri]))
			copy(targets, r.watchers[uri])
			r.mu.Unlock()

			// Fan out CONCURRENTLY with a per-target deadline so one slow or
			// wedged ApplyCredentials cannot delay the rotation for the other
			// targets sharing this URI (previously serial). Each apply gets its
			// own bounded context derived from the parent, so Close (which
			// cancels the parent) still stops every apply promptly.
			var applyWG sync.WaitGroup
			for _, aware := range targets {
				applyWG.Add(1)
				go func(aware CredentialAware) {
					defer applyWG.Done()
					r.applyOne(parent, uri, aware, creds)
				}(aware)
			}
			applyWG.Wait()

			// Invalidate the pull cache so subsequent synchronous resolves see
			// the rotated material (contract C4). Done once per rotation, after
			// the push targets are updated.
			if r.onRotation != nil {
				r.onRotation(uri)
			}
		}
	}
}

// applyOne applies creds to a single target under a bounded context and records
// the outcome: MetricCredentialRotationApplied on success, a WARN log plus a
// reactive re-resolve trigger (F2) when the transport rejects the credentials
// with an authorization failure.
func (r *CredentialRefresher) applyOne(
	parent context.Context,
	uri string,
	aware CredentialAware,
	creds *connectivity.CredentialSet,
) {
	ctx := parent
	if r.applyTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parent, r.applyTimeout)
		defer cancel()
	}

	if err := aware.ApplyCredentials(ctx, creds); err != nil {
		if r.logger != nil {
			r.logger.Warn("credential refresh: ApplyCredentials failed",
				"uri", shared.RedactURI(uri), "error", shared.RedactURIError(err))
		}
		// The transport rejected the credentials as unauthorized: force an
		// out-of-band re-resolve so the next attempt uses freshly fetched
		// material rather than waiting for the poll timer (F2).
		r.NotifyAuthFailure(uri, err)
		return
	}

	r.metrics.Counter(shared.MetricCredentialRotationApplied, 1)
	if logging.DebugEnabled(r.logger) {
		r.logger.Log(parent, logging.LevelDebug,
			"credential refresh: applied rotated credentials",
			"uri", shared.RedactURI(uri))
	}
}

// Close cancels all watcher goroutines and waits for them to exit.
// After Close, further Watch calls are no-ops.
func (r *CredentialRefresher) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.ctx = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	r.wg.Wait()
}
