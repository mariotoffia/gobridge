package bridge

import (
	"context"
	"log/slog"
	"sync"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

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
	push   ports.PushCredentialStore
	logger *slog.Logger

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

// NewCredentialRefresher creates a refresher bound to the given push
// store. push may be nil, in which case Watch is a no-op — callers
// should always construct one and delegate the nil-check here so the
// rest of the code path stays uniform.
func NewCredentialRefresher(push ports.PushCredentialStore, logger *slog.Logger, opts ...RefresherOption) *CredentialRefresher {
	ctx, cancel := context.WithCancel(context.Background())
	r := &CredentialRefresher{
		push:     push,
		logger:   logger,
		ctx:      ctx,
		cancel:   cancel,
		watchers: make(map[string][]CredentialAware),
	}
	for _, o := range opts {
		o(r)
	}
	return r
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
				"uri", uri, "kind", kind)
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
			r.logger.Warn("credential refresh: Watch failed", "uri", uri, "error", err)
		}
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

			for _, aware := range targets {
				if err := aware.ApplyCredentials(parent, creds); err != nil {
					if r.logger != nil {
						r.logger.Warn("credential refresh: ApplyCredentials failed",
							"uri", uri, "error", err)
					}
				} else if logging.DebugEnabled(r.logger) {
					r.logger.Log(parent, logging.LevelDebug,
						"credential refresh: applied rotated credentials",
						"uri", uri)
				}
			}

			// Invalidate the pull cache so subsequent synchronous resolves see
			// the rotated material (contract C4). Done once per rotation, after
			// the push targets are updated.
			if r.onRotation != nil {
				r.onRotation(uri)
			}
		}
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
