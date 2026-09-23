package bridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// perURIPushStore gives every watched URI its own rotation channel and keeps
// the context each Watch was handed, so a test can rotate one URI and see
// whether that URI's poller, and only that one, was stopped. A rotation reaches
// the latest Watch of its URI only: a stopped poller still selecting on a
// shared channel could otherwise take it and drop it.
type perURIPushStore struct {
	mu   sync.Mutex
	out  map[string]chan *connectivity.CredentialSet
	ctxs map[string]context.Context
}

func newPerURIPushStore() *perURIPushStore {
	return &perURIPushStore{
		out:  make(map[string]chan *connectivity.CredentialSet),
		ctxs: make(map[string]context.Context),
	}
}

func (p *perURIPushStore) Watch(ctx context.Context, uri string) (<-chan *connectivity.CredentialSet, error) {
	src := make(chan *connectivity.CredentialSet, 1)
	p.mu.Lock()
	p.ctxs[uri] = ctx
	p.out[uri] = src
	p.mu.Unlock()
	ch := make(chan *connectivity.CredentialSet)
	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case creds := <-src:
				select {
				case ch <- creds:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, nil
}

// rotations is the channel a test pushes uri's rotated credentials into.
func (p *perURIPushStore) rotations(uri string) chan *connectivity.CredentialSet {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch, ok := p.out[uri]
	if !ok {
		ch = make(chan *connectivity.CredentialSet, 1)
		p.out[uri] = ch
	}
	return ch
}

// watchCtx is the context the latest Watch of uri was handed.
func (p *perURIPushStore) watchCtx(t *testing.T, uri string) context.Context {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx, ok := p.ctxs[uri]
	require.True(t, ok, "%s was never watched", uri)
	return ctx
}

var _ ports.PushCredentialStore = (*perURIPushStore)(nil)

func newCredTarget() *credAwareFakeSession {
	return &credAwareFakeSession{fakeSession: &fakeSession{}, applied: make(chan *connectivity.CredentialSet, 4)}
}

// valueCredReceiver is a CredentialAware receiver held by value whose type Go
// cannot compare, so matching it by identity must not panic.
type valueCredReceiver struct {
	tags []string
}

func (valueCredReceiver) Run(ctx context.Context, _ func(context.Context, ports.Delivery) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func (valueCredReceiver) ApplyCredentials(context.Context, *connectivity.CredentialSet) error {
	return nil
}

// TestForget_RotationSkipsForgottenTarget pins that a forgotten target stops
// receiving rotations while the target it shared the URI with keeps them.
func TestForget_RotationSkipsForgottenTarget(t *testing.T) {
	t.Parallel()

	push := newPerURIPushStore()
	r := NewCredentialRefresher(push, nil)
	a, b := newCredTarget(), newCredTarget()
	const uri = "file://shared"
	r.Watch(t.Context(), uri, a)
	r.Watch(t.Context(), uri, b)

	require.False(t, r.Forget([]any{a}), "a refresher still watching b is not idle")

	push.rotations(uri) <- connectivity.NewCredentialSet(pwCred("rotated", "p"), nil)
	got := wait.RequireReceive(t, b.applied, 2*time.Second)
	require.Equal(t, "rotated", got.Password().Username())
	require.NoError(t, push.watchCtx(t, uri).Err(), "the poller of a URI still in use keeps running")

	// Close joins the poller, so the rotation's fan-out has finished.
	r.Close()
	require.Empty(t, a.applied, "a forgotten target must not receive a rotation")
}

// TestForget_LastTargetStopsPoller pins that forgetting a URI's last target
// stops that URI's poller alone, and that a refresher left watching nothing
// reports itself idle.
func TestForget_LastTargetStopsPoller(t *testing.T) {
	t.Parallel()

	push := newPerURIPushStore()
	r := NewCredentialRefresher(push, nil)
	defer r.Close()
	a, b := newCredTarget(), newCredTarget()
	r.Watch(t.Context(), "file://a", a)
	r.Watch(t.Context(), "file://b", b)

	require.False(t, r.Forget([]any{a}), "a refresher still watching file://b is not idle")
	wait.RequireClosed(t, push.watchCtx(t, "file://a").Done(), 2*time.Second)
	require.NoError(t, push.watchCtx(t, "file://b").Err(), "another URI's poller keeps running")

	require.True(t, r.Forget([]any{b}), "a refresher left watching nothing is idle")
	wait.RequireClosed(t, push.watchCtx(t, "file://b").Done(), 2*time.Second)

	// The URI was dropped, so watching it again starts a fresh poller.
	c := newCredTarget()
	r.Watch(t.Context(), "file://a", c)
	require.NoError(t, push.watchCtx(t, "file://a").Err())
	push.rotations("file://a") <- connectivity.NewCredentialSet(pwCred("again", "p"), nil)
	require.Equal(t, "again", wait.RequireReceive(t, c.applied, 2*time.Second).Password().Username())
}

// TestForget_UnknownTargetKeepsWatching pins that forgetting targets the
// refresher never watched changes nothing: it is not idle and rotations still
// reach the watched target.
func TestForget_UnknownTargetKeepsWatching(t *testing.T) {
	t.Parallel()

	push := newPerURIPushStore()
	r := NewCredentialRefresher(push, nil)
	defer r.Close()
	a := newCredTarget()
	const uri = "file://creds"
	r.Watch(t.Context(), uri, a)

	require.False(t, r.Forget([]any{newCredTarget(), &fakeReceiver{}, nil}))

	push.rotations(uri) <- connectivity.NewCredentialSet(pwCred("rotated", "p"), nil)
	require.Equal(t, "rotated", wait.RequireReceive(t, a.applied, 2*time.Second).Password().Username())
	require.NoError(t, push.watchCtx(t, uri).Err())
}

// TestForget_NonComparableTargetNeverMatches pins that a target Go cannot
// compare is never matched, rather than panicking the retiring runtime.
func TestForget_NonComparableTargetNeverMatches(t *testing.T) {
	t.Parallel()

	r := NewCredentialRefresher(newPerURIPushStore(), nil)
	defer r.Close()
	r.WatchReceiver(t.Context(), "file://creds", valueCredReceiver{tags: []string{"a"}})

	require.NotPanics(t, func() {
		require.False(t, r.Forget([]any{valueCredReceiver{tags: []string{"a"}}}))
	})
}

// credSessionFactory hands out a prepared credential-aware session per session
// id, so a test can see which sessions a rotation reaches.
type credSessionFactory struct {
	fakeTransportFactory
	sessions map[string]*credAwareFakeSession
}

func (f *credSessionFactory) NewSession(_ context.Context, spec ports.SessionSpec) (ports.Session, error) {
	return f.sessions[spec.ID], nil
}

// TestComplete_AttachesCredentialForget pins that a built runtime lets its
// credential refresher forget the transports Retire takes out: a rotation of a
// credential two sessions share reaches the surviving session and no longer
// the retired one.
func TestComplete_AttachesCredentialForget(t *testing.T) {
	t.Parallel()

	const uri = "cred://shared"
	pull := &fakeCredentialStore{creds: map[string]*connectivity.CredentialSet{
		uri: connectivity.NewCredentialSet(pwCred("u", "p"), nil),
	}}
	push := newPerURIPushStore()
	factory := &credSessionFactory{sessions: map[string]*credAwareFakeSession{
		"s1": newCredTarget(),
		"s2": newCredTarget(),
	}}
	drop := ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"}
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		Sessions: []ports.SessionDef{
			{ID: "s1", Transport: "credsess", Config: &testCredConfig{URI: uri}},
			{ID: "s2", Transport: "credsess", Config: &testCredConfig{URI: uri}},
		},
		Receivers: []ports.ReceiverDef{{ID: "rx1", Transport: "credsess"}, {ID: "rx2", Transport: "credsess"}},
		Senders: []ports.SenderDef{
			{ID: "tx1", Transport: "credsess", SessionID: "s1"},
			{ID: "tx2", Transport: "credsess", SessionID: "s2"},
		},
		Bindings: []ports.BindingDef{
			{ID: "bd1", SenderID: "tx1", SessionID: "s1", Address: "queue://one"},
			{ID: "bd2", SenderID: "tx2", SessionID: "s2", Address: "queue://two"},
		},
		Routes: []ports.RouteDef{
			{ID: "r1", ReceiverID: "rx1", DeliveryMode: "direct_hold", Bindings: []string{"bd1"}, Policy: drop},
			{ID: "r2", ReceiverID: "rx2", DeliveryMode: "direct_hold", Bindings: []string{"bd2"}, Policy: drop},
		},
	}

	rt, err := NewBuilder(cfg, WithCredentialStore(pull), WithPushCredentialStore(push)).
		RegisterTransportFactory("credsess", factory).
		Build(t.Context())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, rt.Start(ctx))

	require.NoError(t, rt.Retire(ctx, runtime.Unit{Routes: []string{"r1"}, Sessions: []string{"s1"}}))

	push.rotations(uri) <- connectivity.NewCredentialSet(pwCred("rotated", "p"), nil)
	got := wait.RequireReceive(t, factory.sessions["s2"].applied, 2*time.Second)
	require.Equal(t, "rotated", got.Password().Username())

	// Stop closes the refresher and joins its poller, so the fan-out has finished.
	require.NoError(t, rt.Stop(ctx))
	require.Empty(t, factory.sessions["s1"].applied, "a retired session must not receive a rotation")
}
