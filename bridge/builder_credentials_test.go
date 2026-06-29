package bridge

import (
	"context"
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
// option both sets the pull store AND wraps it in a push store.
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
	require.NotNil(t, b.pushCredStore, "push store wrapper must be registered")

	// Sanity: the wrapper must be usable.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := b.pushCredStore.Watch(ctx, "file://x")
	require.NoError(t, err)
	require.NotNil(t, ch)
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
