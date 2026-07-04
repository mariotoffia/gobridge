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

		// Inject an already-SETTLED failed flight (err set, done closed)
		// and keep it in the map so Resolve's join is deterministic: a
		// goroutine that raced delete(flight) against Resolve's lookup let
		// Resolve miss the flight entirely and re-lead a fresh fetch
		// against an error-free repo (observed under -count=300 -cpu=1).
		// A joiner that found the flight just before the leader's delete
		// observes exactly this state: done closed, err populated.
		fl := &credFlight{done: make(chan struct{}), err: fetchErr}
		close(fl.done)
		r.flightMu.Lock()
		r.flight[uri] = fl
		r.flightMu.Unlock()

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

// ctxBlockingRepo blocks every Get on release-or-ctx and signals entry, so a
// test can hold a fetch in flight, cancel the leader's ctx (making Get return
// ctx.Err()), and then release a subsequent re-leading fetch. entered is
// buffered so overlapping Gets never block on the signal.
type ctxBlockingRepo struct {
	scheme  string
	creds   *connectivity.CredentialSet
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (b *ctxBlockingRepo) Scheme() string    { return b.scheme }
func (b *ctxBlockingRepo) Namespace() string { return "" }
func (b *ctxBlockingRepo) Get(ctx context.Context, _ string) (*connectivity.CredentialSet, error) {
	b.calls.Add(1)
	b.entered <- struct{}{}
	select {
	case <-b.release:
		return b.creds, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func waitEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("repository Get was never reached")
	}
}

// TestCredentialResolver_SingleflightFollowerSurvivesLeaderCtxCancel is the
// regression for the singleflight leader-ctx propagation defect: a follower
// with a HEALTHY context must not inherit the LEADER's cancellation. When the
// leader's fetch fails with context.Canceled purely because the leader's own
// ctx died, the healthy follower re-leads a fresh fetch instead of returning the
// leader's spurious error.
func TestCredentialResolver_SingleflightFollowerSurvivesLeaderCtxCancel(t *testing.T) {
	repo := &ctxBlockingRepo{
		scheme:  "file",
		creds:   connectivity.NewCredentialSet(pwCred("u", "p"), nil),
		entered: make(chan struct{}, 4),
		release: make(chan struct{}),
	}
	r := NewCredentialResolver()
	r.Register(repo)
	const uri = "file://broker/main"

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	var leaderErr error
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, leaderErr = r.Resolve(leaderCtx, uri)
	}()

	// The leader is provably inside Get, so its flight is registered.
	waitEntered(t, repo.entered)

	// A follower with a healthy (never-cancelled) ctx joins the leader's flight.
	type res struct {
		creds *connectivity.CredentialSet
		err   error
	}
	followerDone := make(chan res, 1)
	go func() {
		creds, err := r.Resolve(context.Background(), uri)
		followerDone <- res{creds: creds, err: err}
	}()

	// Cancel the leader: its blocked Get returns context.Canceled and the flight
	// fails with a context error that belongs to the LEADER, not the follower.
	cancelLeader()
	<-leaderDone
	require.ErrorIs(t, leaderErr, context.Canceled, "leader must observe its own cancellation")

	// The healthy follower re-leads a fresh fetch; wait until it reaches the
	// repo, then release so its own Get completes with valid credentials.
	waitEntered(t, repo.entered)
	close(repo.release)

	select {
	case got := <-followerDone:
		require.NoError(t, got.err, "healthy follower must NOT inherit the leader's cancellation")
		require.NotNil(t, got.creds)
		assert.Equal(t, "u", got.creds.Password().Username())
	case <-time.After(2 * time.Second):
		t.Fatal("follower never completed")
	}

	assert.GreaterOrEqual(t, repo.calls.Load(), int32(2),
		"follower must have re-fetched as the new leader after the leader's ctx cancel")
}
