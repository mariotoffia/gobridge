package bridge

import (
	"context"
	"log/slog"
	"sync"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// CredentialAware is implemented by transport sessions that can rotate
// their authentication material at runtime. When a PushCredentialStore
// emits a new *domain.CredentialSet on a URI bound to such a session,
// the CredentialRefresher calls ApplyCredentials on it.
//
// Why a capability interface instead of a method on ports.Session: not
// every transport supports hot credential rotation (e.g. HTTP servers
// have no concept of session auth). Keeping this as a capability lets
// transports opt in without enlarging the core Session port.
type CredentialAware interface {
	ApplyCredentials(ctx context.Context, creds *domain.CredentialSet) error
}

// CredentialRefresher owns the watcher goroutines that translate push
// rotations into ApplyCredentials calls on transport sessions. One
// Refresher is created per Build() when a PushCredentialStore is
// registered; it is torn down by Close.
type CredentialRefresher struct {
	push   ports.PushCredentialStore
	logger *slog.Logger

	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	watchers map[string]bool // uri → already watching
}

// NewCredentialRefresher creates a refresher bound to the given push
// store. push may be nil, in which case Watch is a no-op — callers
// should always construct one and delegate the nil-check here so the
// rest of the code path stays uniform.
func NewCredentialRefresher(push ports.PushCredentialStore, logger *slog.Logger) *CredentialRefresher {
	ctx, cancel := context.WithCancel(context.Background())
	return &CredentialRefresher{
		push:     push,
		logger:   logger,
		ctx:      ctx,
		cancel:   cancel,
		watchers: make(map[string]bool),
	}
}

// Watch registers a (uri, session) pair for refresh. If the same URI
// is registered multiple times (e.g. two sessions share credentials),
// a new goroutine is spawned per session so each gets its own apply
// callback — the underlying PushCredentialStore is expected to handle
// multiple concurrent Watch calls for the same URI efficiently (the
// PollBasedWrapper does, by giving each call its own ticker).
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
	r.watchers[uri] = true
	r.mu.Unlock()

	ch, err := r.push.Watch(parent, uri)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("credential refresh: Watch failed", "uri", uri, "error", err)
		}
		return
	}

	r.wg.Add(1)
	go r.run(parent, uri, ch, aware)
}

func (r *CredentialRefresher) run(
	parent context.Context,
	uri string,
	ch <-chan *domain.CredentialSet,
	aware CredentialAware,
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
