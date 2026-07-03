package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// Structural assertion that CredentialResolver satisfies the poll
// wrapper's UncachedPullCredentialStore capability
// (runtime/credentials). Declared structurally because the dependency
// direction forbids runtime from importing its leaf package: the leaf
// type-asserts against this exact method set at runtime.
var _ interface {
	ResolveUncached(ctx context.Context, uri string) (*connectivity.CredentialSet, error)
} = (*CredentialResolver)(nil)

// blockingRepo blocks every Get until release is closed, so tests can
// hold a fetch in flight while concurrent Resolve calls pile up.
type blockingRepo struct {
	scheme  string
	creds   *connectivity.CredentialSet
	err     error
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockingRepo) Scheme() string    { return b.scheme }
func (b *blockingRepo) Namespace() string { return "" }
func (b *blockingRepo) Get(_ context.Context, _ string) (*connectivity.CredentialSet, error) {
	b.calls.Add(1)
	b.entered <- struct{}{}
	<-b.release
	if b.err != nil {
		return nil, b.err
	}
	return b.creds, nil
}

// Verifies the singleflight guard: concurrent cache misses for the
// same URI issue exactly ONE repository Get (no thundering herd) and
// every caller receives the fetched credentials.
func TestCredentialResolver_SingleflightCoalescesConcurrentMisses(t *testing.T) {
	repo := &blockingRepo{
		scheme:  "file",
		creds:   connectivity.NewCredentialSet(pwCred("u", "p"), nil),
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}

	r := NewCredentialResolver()
	r.Register(repo)

	const uri = "file://broker/main"
	const followers = 9

	results := make(chan *connectivity.CredentialSet, followers+1)
	errs := make(chan error, followers+1)
	var wg sync.WaitGroup

	resolveOne := func() {
		defer wg.Done()
		got, err := r.Resolve(context.Background(), uri)
		if err != nil {
			errs <- err
			return
		}
		results <- got
	}

	// Leader: registers the flight, then blocks inside Get.
	wg.Add(1)
	go resolveOne()
	select {
	case <-repo.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("leader never reached the repository Get")
	}

	// Followers start while the leader is provably blocked in Get, so
	// the flight entry exists for the whole of their Resolve call —
	// each one MUST join it rather than fetch.
	for range followers {
		wg.Add(1)
		go resolveOne()
	}

	close(repo.release)
	wg.Wait()

	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("unexpected Resolve error: %v", err)
	}
	var n int
	for got := range results {
		n++
		require.NotNil(t, got.Password())
		assert.Equal(t, "u", got.Password().Username())
	}
	assert.Equal(t, followers+1, n)
	assert.Equal(t, int32(1), repo.calls.Load(), "thundering herd: every caller hit the repository")
}

// Verifies a caller that joins an in-flight fetch receives the
// leader's error (white-box: the flight entry is injected so the join
// is deterministic), and that errors are never cached — every
// subsequent Resolve retries the repository.
func TestCredentialResolver_SingleflightErrorPropagatesAndIsNotCached(t *testing.T) {
	fetchErr := errors.New("backend down")

	t.Run("follower receives the leader's error", func(t *testing.T) {
		repo := &mutableRepo{scheme: "file"}
		r := NewCredentialResolver()
		r.Register(repo)
		const uri = "file://broker/main"

		fl := &credFlight{done: make(chan struct{})}
		r.flightMu.Lock()
		r.flight[uri] = fl
		r.flightMu.Unlock()

		go func() {
			fl.err = fetchErr
			r.flightMu.Lock()
			delete(r.flight, uri)
			r.flightMu.Unlock()
			close(fl.done)
		}()

		_, err := r.Resolve(context.Background(), uri)
		require.ErrorIs(t, err, fetchErr)
		assert.Equal(t, int32(0), repo.calls.Load(), "follower must not fetch while a flight is in progress")
	})

	t.Run("errors are not cached", func(t *testing.T) {
		repo := &mutableRepo{scheme: "file", err: fetchErr}
		r := NewCredentialResolver()
		r.Register(repo)
		const uri = "file://broker/main"

		_, err := r.Resolve(context.Background(), uri)
		require.ErrorIs(t, err, fetchErr)
		_, err = r.Resolve(context.Background(), uri)
		require.ErrorIs(t, err, fetchErr)
		assert.Equal(t, int32(2), repo.calls.Load(), "failed fetch must not be cached")
	})
}

// Verifies a follower whose context is cancelled while waiting on an
// in-flight fetch returns promptly with the context error instead of
// blocking until the leader finishes.
func TestCredentialResolver_SingleflightFollowerHonoursContext(t *testing.T) {
	repo := &blockingRepo{
		scheme:  "file",
		creds:   connectivity.NewCredentialSet(pwCred("u", "p"), nil),
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}

	r := NewCredentialResolver()
	r.Register(repo)

	const uri = "file://broker/main"
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = r.Resolve(context.Background(), uri)
	}()
	select {
	case <-repo.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("leader never reached the repository Get")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Resolve(cancelled, uri)
	require.ErrorIs(t, err, context.Canceled)

	close(repo.release)
	wg.Wait()
}

// mutableRepo lets the test swap the served credentials or error
// between calls (sequentially — no concurrent access in these tests).
type mutableRepo struct {
	scheme string
	creds  *connectivity.CredentialSet
	err    error
	calls  atomic.Int32
}

func (m *mutableRepo) Scheme() string    { return m.scheme }
func (m *mutableRepo) Namespace() string { return "" }
func (m *mutableRepo) Get(_ context.Context, _ string) (*connectivity.CredentialSet, error) {
	m.calls.Add(1)
	if m.err != nil {
		return nil, m.err
	}
	return m.creds, nil
}

// Verifies ResolveUncached bypasses the cached value AND refreshes the
// cache: after a rotation, an uncached read observes the new secret
// immediately, and subsequent cached Resolve calls (e.g. a connection
// rebuild) see the rotated secret too — not the stale entry that would
// otherwise live until TTL expiry.
func TestCredentialResolver_ResolveUncachedBypassesAndRefreshesCache(t *testing.T) {
	repo := &mutableRepo{
		scheme: "file",
		creds:  connectivity.NewCredentialSet(pwCred("u", "old-secret"), nil),
	}

	r := NewCredentialResolver()
	r.Register(repo)
	ctx := context.Background()
	const uri = "file://broker/main"

	// Prime the cache with the pre-rotation secret.
	got, err := r.Resolve(ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, "old-secret", got.Password().Password().Reveal())
	assert.Equal(t, int32(1), repo.calls.Load())

	// Rotate the secret in the backend. A cached Resolve still serves
	// the stale value (this is the cache doing its job).
	repo.creds = connectivity.NewCredentialSet(pwCred("u", "new-secret"), nil)
	got, err = r.Resolve(ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, "old-secret", got.Password().Password().Reveal())
	assert.Equal(t, int32(1), repo.calls.Load(), "expected a cache hit")

	// The uncached read (the rotation poll's path) must see the
	// rotated secret immediately.
	got, err = r.ResolveUncached(ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, "new-secret", got.Password().Password().Reveal())
	assert.Equal(t, int32(2), repo.calls.Load())

	// ...and it must have refreshed the cache: a rebuild right after
	// rotation resolves the rotated secret without another fetch.
	got, err = r.Resolve(ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, "new-secret", got.Password().Password().Reveal())
	assert.Equal(t, int32(2), repo.calls.Load(), "cache should have been refreshed by ResolveUncached")
}
