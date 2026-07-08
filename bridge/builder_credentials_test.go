package bridge

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// fakePushStore is a minimal PushCredentialStore used to prove that
// Builder.Build wires the push store through to a per-URI watcher.
type fakePushStore struct {
	watched int32
}

func (f *fakePushStore) Watch(ctx context.Context, _ string) (<-chan *connectivity.CredentialSet, error) {
	atomic.AddInt32(&f.watched, 1)
	ch := make(chan *connectivity.CredentialSet)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// TestWithCredentialStore_BackwardCompat verifies that the legacy
// WithCredentialStore option continues to accept pull-only stores
// after PullCredentialStore was introduced.
func TestWithCredentialStore_BackwardCompat(t *testing.T) {
	t.Parallel()

	cs := &fakeCredentialStore{creds: map[string]*connectivity.CredentialSet{
		"file://creds/a": connectivity.NewCredentialSet(pwCred("u", "p"), nil),
	}}

	cfg := &ports.BridgeConfig{}
	b := NewBuilder(cfg, WithCredentialStore(cs))

	require.Same(t, cs, b.credStore, "WithCredentialStore must populate b.credStore")
	require.Nil(t, b.pushCredStore, "WithCredentialStore must NOT touch push store")
}

// TestWithPullCredentialStore_Alias verifies WithPullCredentialStore is
// the same knob as WithCredentialStore.
func TestWithPullCredentialStore_Alias(t *testing.T) {
	t.Parallel()

	cs := &fakeCredentialStore{}

	cfg := &ports.BridgeConfig{}
	b := NewBuilder(cfg, WithPullCredentialStore(cs))

	require.Same(t, cs, b.credStore)
}

// TestWithPushCredentialStore verifies the push store is registered
// independently and does not displace the pull store.
func TestWithPushCredentialStore(t *testing.T) {
	t.Parallel()

	push := &fakePushStore{}
	pull := &fakeCredentialStore{}

	cfg := &ports.BridgeConfig{}
	b := NewBuilder(cfg,
		WithCredentialStore(pull),
		WithPushCredentialStore(push),
	)

	require.Same(t, pull, b.credStore)
	require.NotNil(t, b.pushCredStore)
}

// TestWithPolledCredentialStore_WrapsPullStore verifies that the convenience
// option registers the pull store AND that the push wrapper is produced at
// build time (Finding 13). The wrapper is NOT constructed eagerly at
// option-application time — doing so captured whatever logger was set so far,
// making the result depend on option ordering. The pull store and poll config
// are recorded and effectivePushStore builds the wrapper with the fully-resolved
// logger, so option order is irrelevant.
func TestWithPolledCredentialStore_WrapsPullStore(t *testing.T) {
	t.Parallel()

	pull := &fakeCredentialStore{
		creds: map[string]*connectivity.CredentialSet{
			"file://x": connectivity.NewCredentialSet(pwCred("u", ""), nil),
		},
	}

	cfg := &ports.BridgeConfig{}
	b := NewBuilder(cfg, WithPolledCredentialStore(pull, ports.PollBasedWrapperConfig{
		PollInterval: time.Second,
	}))

	require.Same(t, pull, b.credStore, "pull store must be registered")
	require.Nil(t, b.pushCredStore, "poll wrapper must NOT be built eagerly (Finding 13)")
	require.Same(t, pull, b.pollCredStore, "pull store must be recorded for lazy wrapping")

	// The wrapper is resolved at build time and must be usable.
	push := b.effectivePushStore()
	require.NotNil(t, push, "effectivePushStore must build the poll wrapper")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := push.Watch(ctx, "file://x")
	require.NoError(t, err)
	require.NotNil(t, ch)
}

// TestWithPolledCredentialStore_OrderIndependent verifies the poll wrapper picks
// up a logger set by a LATER option (Finding 13): before the fix, WithLogger
// applied after WithPolledCredentialStore was silently ignored because the
// wrapper had already captured a nil logger.
func TestWithPolledCredentialStore_OrderIndependent(t *testing.T) {
	t.Parallel()

	pull := &fakeCredentialStore{
		creds: map[string]*connectivity.CredentialSet{
			"file://x": connectivity.NewCredentialSet(pwCred("u", ""), nil),
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := &ports.BridgeConfig{}
	// WithLogger applied AFTER WithPolledCredentialStore — the wrapper must
	// still observe the logger because it is built lazily at build time.
	b := NewBuilder(cfg,
		WithPolledCredentialStore(pull, ports.PollBasedWrapperConfig{PollInterval: time.Second}),
		WithLogger(logger),
	)
	require.Same(t, logger, b.logger)
	require.NotNil(t, b.effectivePushStore(), "wrapper resolves regardless of option order")
}

// TestPullCacheInvalidation_OnlyForExplicitPushStore validates adversarial
// Finding 1: the post-rotation InvalidateCache (contract C4) must be wired ONLY
// for an explicitly-registered push store, which rotates out of band from the
// pull cache. The lazy poll wrapper (WithPolledCredentialStore) wraps the same
// resolver and refreshes its cache on the detecting poll, so invalidating there
// would delete a just-cached fresh entry and blind F5 stale-serve for a poll
// interval. The builder must therefore NOT invalidate on the polled path.
func TestPullCacheInvalidation_OnlyForExplicitPushStore(t *testing.T) {
	t.Parallel()

	cfg := &ports.BridgeConfig{}
	pull := &fakeCredentialStore{creds: map[string]*connectivity.CredentialSet{}}

	polled := NewBuilder(cfg, WithPolledCredentialStore(pull, ports.PollBasedWrapperConfig{PollInterval: time.Second}))
	require.False(t, polled.pullCacheNeedsRotationInvalidation(),
		"coherent lazy-wrapper path must NOT invalidate the pull cache on rotation (Finding 1)")

	decoupled := NewBuilder(cfg, WithPushCredentialStore(&fakePushStore{}))
	require.True(t, decoupled.pullCacheNeedsRotationInvalidation(),
		"an explicitly-registered push store must invalidate the pull cache on rotation (contract C4)")
}

// TestCredentialRefresher_NoopWithoutPush verifies the refresher is a
// safe no-op when constructed with a nil push store (makes composition
// root code uniform).
func TestCredentialRefresher_NoopWithoutPush(t *testing.T) {
	t.Parallel()

	r := NewCredentialRefresher(nil, nil)
	r.Watch(t.Context(), "file://x", nil) // must not panic
	r.Close()                             // must not panic
}

// credAwareFakeSession implements ports.Session + CredentialAware to
// prove the refresher routes rotation events to ApplyCredentials.
type credAwareFakeSession struct {
	*fakeSession
	applied chan *connectivity.CredentialSet
}

func (c *credAwareFakeSession) ApplyCredentials(_ context.Context, creds *connectivity.CredentialSet) error {
	c.applied <- creds
	return nil
}

// inlinePushStore is a hand-driven push store: callers push rotations
// via Emit, and every Watch call gets its own channel.
type inlinePushStore struct {
	out chan *connectivity.CredentialSet
}

func (p *inlinePushStore) Watch(ctx context.Context, _ string) (<-chan *connectivity.CredentialSet, error) {
	// Proxy ctx cancellation to close the channel per the contract.
	proxy := make(chan *connectivity.CredentialSet, 1)
	go func() {
		defer close(proxy)
		for {
			select {
			case <-ctx.Done():
				return
			case c, ok := <-p.out:
				if !ok {
					return
				}
				select {
				case proxy <- c:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return proxy, nil
}

// TestCredentialRefresher_RoutesRotationToSession verifies that when the
// push store emits a new credential, the refresher delivers it to the
// CredentialAware session via ApplyCredentials.
func TestCredentialRefresher_RoutesRotationToSession(t *testing.T) {
	t.Parallel()

	push := &inlinePushStore{out: make(chan *connectivity.CredentialSet, 1)}
	r := NewCredentialRefresher(push, nil)
	defer r.Close()
	sess := &credAwareFakeSession{
		fakeSession: &fakeSession{},
		applied:     make(chan *connectivity.CredentialSet, 2),
	}

	r.Watch(t.Context(), "file://creds", sess)

	want := connectivity.NewCredentialSet(pwCred("u2", "p2"), nil)
	push.out <- want

	select {
	case got := <-sess.applied:
		require.Equal(t, "u2", got.Password().Username())
	case <-time.After(2 * time.Second):
		t.Fatal("rotation not delivered to session")
	}
}

// pwCred builds a pointer to an immutable PasswordCredential for tests.
func pwCred(username, password string) *connectivity.PasswordCredential {
	c := connectivity.NewPasswordCredential(username, password)
	return &c
}

// countingPushStore counts Watch calls and shares a single emit channel across
// every Watch so a test can assert both how many pollers were spawned and that
// a single rotation fans out to all shared targets.
type countingPushStore struct {
	watchCalls atomic.Int32
	out        chan *connectivity.CredentialSet
}

func (p *countingPushStore) Watch(ctx context.Context, _ string) (<-chan *connectivity.CredentialSet, error) {
	p.watchCalls.Add(1)
	proxy := make(chan *connectivity.CredentialSet, 1)
	go func() {
		defer close(proxy)
		for {
			select {
			case <-ctx.Done():
				return
			case c, ok := <-p.out:
				if !ok {
					return
				}
				select {
				case proxy <- c:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return proxy, nil
}

// TestCredentialRefresher_DedupesWatchPerURI validates Finding 14: two targets
// that share the same credentials URI must spawn exactly ONE poller (not one
// per Watch call), and a single rotation must fan out to BOTH targets. Before
// the fix the watchers map was write-only, so every Watch spawned a duplicate
// poller and the intended dedup did not exist.
func TestCredentialRefresher_DedupesWatchPerURI(t *testing.T) {
	t.Parallel()

	push := &countingPushStore{out: make(chan *connectivity.CredentialSet, 1)}
	r := NewCredentialRefresher(push, nil)
	defer r.Close()

	s1 := &credAwareFakeSession{fakeSession: &fakeSession{}, applied: make(chan *connectivity.CredentialSet, 2)}
	s2 := &credAwareFakeSession{fakeSession: &fakeSession{}, applied: make(chan *connectivity.CredentialSet, 2)}

	const uri = "file://shared-creds"
	r.Watch(t.Context(), uri, s1)
	r.Watch(t.Context(), uri, s2)

	require.Equal(t, int32(1), push.watchCalls.Load(),
		"a second Watch for the same URI must not spawn a duplicate poller")

	want := connectivity.NewCredentialSet(pwCred("rotated", "secret"), nil)
	push.out <- want

	for i, s := range []*credAwareFakeSession{s1, s2} {
		select {
		case got := <-s.applied:
			require.Equal(t, "rotated", got.Password().Username())
		case <-time.After(2 * time.Second):
			t.Fatalf("rotation not fanned out to shared target %d", i+1)
		}
	}
}
