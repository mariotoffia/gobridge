package credentials

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// fakePullStore returns a new CredentialSet from a queue on each
// Resolve call. Once the queue drains it keeps returning the last
// value, mirroring a stable backend.
type fakePullStore struct {
	mu    sync.Mutex
	queue []*connectivity.CredentialSet
	calls int64
	err   error
}

func (f *fakePullStore) Resolve(_ context.Context, _ string) (*connectivity.CredentialSet, error) {
	atomic.AddInt64(&f.calls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if len(f.queue) == 0 {
		return nil, shared.ErrNotFound
	}
	head := f.queue[0]
	if len(f.queue) > 1 {
		f.queue = f.queue[1:]
	}
	return head, nil
}

func pwd(user, pass string) *connectivity.CredentialSet {
	return connectivity.NewCredentialSet(pwCred(user, pass), nil)
}

// TestPollBasedWrapper_EmitsOnChange verifies that the wrapper publishes
// a new credential set only when Resolve returns a value that differs
// from the previously observed one, and suppresses duplicates.
func TestPollBasedWrapper_EmitsOnChange(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	pull := &fakePullStore{queue: []*connectivity.CredentialSet{
		pwd("u1", "p1"), // seed
		pwd("u1", "p1"), // dup
		pwd("u2", "p2"), // change
		pwd("u2", "p2"), // dup
		pwd("u3", "p3"), // change
	}}

	w := NewPollBasedWrapper(pull, ports.PollBasedWrapperConfig{
		PollInterval: time.Second,
		EmitOnStart:  true,
	}, WithPollClock(fake))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ch, err := w.Watch(ctx, "file://creds/a")
	require.NoError(t, err)

	// Seed emission (EmitOnStart) — comes from the initial resolve, no
	// tick required.
	select {
	case got := <-ch:
		require.Equal(t, "u1", got.Password().Username())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial emission")
	}

	// Wait for the watcher to install its timer before advancing the
	// fake clock — otherwise Advance races the goroutine's first
	// NewTimer call and drops the tick.
	require.Eventually(t, func() bool { return fake.TimerCount() >= 1 }, time.Second, time.Millisecond)

	// Advance: pulls duplicate (u1,p1) — must NOT emit.
	fake.Advance(time.Second)
	select {
	case got := <-ch:
		t.Fatalf("unexpected emission on duplicate: %+v", got.Password())
	case <-time.After(100 * time.Millisecond):
	}

	// Second tick: change to (u2,p2) — must emit.
	require.Eventually(t, func() bool { return fake.TimerCount() >= 1 }, time.Second, time.Millisecond)
	fake.Advance(time.Second)
	select {
	case got := <-ch:
		require.Equal(t, "u2", got.Password().Username())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for changed emission")
	}

	// Third tick: duplicate — must NOT emit.
	require.Eventually(t, func() bool { return fake.TimerCount() >= 1 }, time.Second, time.Millisecond)
	fake.Advance(time.Second)
	select {
	case got := <-ch:
		t.Fatalf("unexpected emission: %+v", got.Password())
	case <-time.After(100 * time.Millisecond):
	}

	// Fourth tick: change to (u3,p3) — must emit.
	require.Eventually(t, func() bool { return fake.TimerCount() >= 1 }, time.Second, time.Millisecond)
	fake.Advance(time.Second)
	select {
	case got := <-ch:
		require.Equal(t, "u3", got.Password().Username())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for final emission")
	}
}

// TestPollBasedWrapper_ContextCancel verifies the channel is closed
// once the caller cancels the context.
func TestPollBasedWrapper_ContextCancel(t *testing.T) {
	t.Parallel()

	pull := &fakePullStore{queue: []*connectivity.CredentialSet{pwd("u", "p")}}
	fake := clocktest.New()
	w := NewPollBasedWrapper(pull, ports.PollBasedWrapperConfig{
		PollInterval: time.Second,
	}, WithPollClock(fake))

	ctx, cancel := context.WithCancel(t.Context())
	ch, err := w.Watch(ctx, "file://creds/x")
	require.NoError(t, err)

	cancel()

	select {
	case _, ok := <-ch:
		require.False(t, ok, "channel must be closed after context cancel")
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after ctx cancel")
	}
}

// TestPollBasedWrapper_ResolveErrorKeepsGoing verifies that transient
// errors from Resolve do not close the channel or abort the loop.
func TestPollBasedWrapper_ResolveErrorKeepsGoing(t *testing.T) {
	t.Parallel()

	pull := &fakePullStore{err: errors.New("boom")}
	fake := clocktest.New()
	w := NewPollBasedWrapper(pull, ports.PollBasedWrapperConfig{
		PollInterval: 500 * time.Millisecond,
	}, WithPollClock(fake))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := w.Watch(ctx, "file://creds/err")
	require.NoError(t, err)

	// Advance several periods; ensure no emission and channel still open.
	require.Eventually(t, func() bool { return fake.TimerCount() >= 1 }, time.Second, time.Millisecond)
	fake.Advance(2 * time.Second)

	select {
	case _, ok := <-ch:
		require.True(t, ok, "channel must remain open through transient errors")
		t.Fatal("unexpected emission on resolve error")
	case <-time.After(50 * time.Millisecond):
	}

	require.GreaterOrEqual(t, atomic.LoadInt64(&pull.calls), int64(2))
}

// TestPollBasedWrapper_NilGuards verifies Watch rejects nil/invalid
// inputs at the boundary.
func TestPollBasedWrapper_NilGuards(t *testing.T) {
	t.Parallel()

	w := NewPollBasedWrapper(nil, ports.PollBasedWrapperConfig{})
	_, err := w.Watch(t.Context(), "file://x")
	require.Error(t, err)

	w2 := NewPollBasedWrapper(&fakePullStore{}, ports.PollBasedWrapperConfig{})
	_, err = w2.Watch(t.Context(), "")
	require.Error(t, err)
}

// TestPollBasedWrapper_DefaultsApplied verifies zero-config produces
// sane behavior (default poll interval, nil jitter).
func TestPollBasedWrapper_DefaultsApplied(t *testing.T) {
	t.Parallel()

	w := NewPollBasedWrapper(&fakePullStore{}, ports.PollBasedWrapperConfig{})
	require.Equal(t, DefaultCredentialPollInterval, w.cfg.PollInterval)
	require.Equal(t, time.Duration(0), w.cfg.Jitter)
}

func TestPollBasedWrapper_JitterSeedUsesInjectedClock(t *testing.T) {
	t.Parallel()

	seedTime := time.Unix(123, 456)
	fake := clocktest.NewAt(seedTime)
	w := NewPollBasedWrapper(&fakePullStore{}, ports.PollBasedWrapperConfig{
		PollInterval: time.Second,
		Jitter:       100 * time.Millisecond,
	}, WithPollClock(fake))

	wantRNG := rand.New(rand.NewSource(seedTime.UnixNano()))
	want := time.Second + time.Duration(wantRNG.Int63n(int64(200*time.Millisecond))) - 100*time.Millisecond
	require.Equal(t, want, w.nextDelay())
}

// pwCred builds a pointer to an immutable PasswordCredential for tests.
func pwCred(username, password string) *connectivity.PasswordCredential {
	c := connectivity.NewPasswordCredential(username, password)
	return &c
}

// TestPollBasedWrapper_OnRotationInvokedBeforeEmission verifies the
// WithOnRotation callback contract: it fires synchronously BEFORE each
// rotation emission (so cache invalidation completes before consumers
// react) and does NOT fire for the initial EmitOnStart seed.
func TestPollBasedWrapper_OnRotationInvokedBeforeEmission(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	pull := &fakePullStore{queue: []*connectivity.CredentialSet{
		pwd("u1", "p1"), // seed
		pwd("u2", "p2"), // rotation 1
		pwd("u3", "p3"), // rotation 2
	}}

	var mu sync.Mutex
	var rotated []string
	w := NewPollBasedWrapper(pull, ports.PollBasedWrapperConfig{
		PollInterval: time.Second,
		EmitOnStart:  true,
	}, WithPollClock(fake), WithOnRotation(func(uri string) {
		mu.Lock()
		rotated = append(rotated, uri)
		mu.Unlock()
	}))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const uri = "file://creds/a"
	ch, err := w.Watch(ctx, uri)
	require.NoError(t, err)

	// Seed emission: NOT a rotation, callback must not fire.
	select {
	case got := <-ch:
		require.Equal(t, "u1", got.Password().Username())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for seed emission")
	}
	mu.Lock()
	require.Empty(t, rotated, "seed emission must not invoke OnRotation")
	mu.Unlock()

	// First rotation: the callback runs synchronously before the send,
	// so by the time the emission is received it must be recorded.
	require.Eventually(t, func() bool { return fake.TimerCount() >= 1 }, time.Second, time.Millisecond)
	fake.Advance(time.Second)
	select {
	case got := <-ch:
		require.Equal(t, "u2", got.Password().Username())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first rotation emission")
	}
	mu.Lock()
	require.Equal(t, []string{uri}, rotated, "OnRotation must have fired before the emission was delivered")
	mu.Unlock()

	// Second rotation.
	require.Eventually(t, func() bool { return fake.TimerCount() >= 1 }, time.Second, time.Millisecond)
	fake.Advance(time.Second)
	select {
	case got := <-ch:
		require.Equal(t, "u3", got.Password().Username())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for second rotation emission")
	}
	mu.Lock()
	require.Equal(t, []string{uri, uri}, rotated)
	mu.Unlock()
}

// fakeUncachedPull implements both Resolve and ResolveUncached with
// separate counters so tests can prove which path the wrapper uses.
type fakeUncachedPull struct {
	inner         fakePullStore
	cachedCalls   atomic.Int64
	uncachedCalls atomic.Int64
}

func (f *fakeUncachedPull) Resolve(ctx context.Context, uri string) (*connectivity.CredentialSet, error) {
	f.cachedCalls.Add(1)
	return f.inner.Resolve(ctx, uri)
}

func (f *fakeUncachedPull) ResolveUncached(ctx context.Context, uri string) (*connectivity.CredentialSet, error) {
	f.uncachedCalls.Add(1)
	return f.inner.Resolve(ctx, uri)
}

var _ UncachedPullCredentialStore = (*fakeUncachedPull)(nil)

// TestPollBasedWrapper_PrefersUncachedResolve verifies that when the
// wrapped store exposes ResolveUncached, EVERY poll read (seed and
// ticks) bypasses the cached Resolve path — a rotation poll that reads
// a TTL cache cannot detect rotations before the TTL expires.
func TestPollBasedWrapper_PrefersUncachedResolve(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	pull := &fakeUncachedPull{inner: fakePullStore{queue: []*connectivity.CredentialSet{
		pwd("u1", "p1"), // seed
		pwd("u2", "p2"), // rotation
	}}}

	w := NewPollBasedWrapper(pull, ports.PollBasedWrapperConfig{
		PollInterval: time.Second,
		EmitOnStart:  true,
	}, WithPollClock(fake))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ch, err := w.Watch(ctx, "file://creds/a")
	require.NoError(t, err)

	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for seed emission")
	}

	require.Eventually(t, func() bool { return fake.TimerCount() >= 1 }, time.Second, time.Millisecond)
	fake.Advance(time.Second)
	select {
	case got := <-ch:
		require.Equal(t, "u2", got.Password().Username())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for rotation emission")
	}

	require.Zero(t, pull.cachedCalls.Load(), "wrapper polled the cached Resolve path; rotations would hide behind the cache TTL")
	require.GreaterOrEqual(t, pull.uncachedCalls.Load(), int64(2))
}

// TestPollBasedWrapper_NextDelayConcurrentlySafe exercises the shared
// jitter RNG from multiple goroutines; the race detector fails this
// test if nextDelay touches the RNG without synchronisation (multiple
// Watch goroutines share one *rand.Rand).
func TestPollBasedWrapper_NextDelayConcurrentlySafe(t *testing.T) {
	t.Parallel()

	w := NewPollBasedWrapper(&fakePullStore{}, ports.PollBasedWrapperConfig{
		PollInterval: time.Second,
		Jitter:       100 * time.Millisecond,
	})

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				d := w.nextDelay()
				if d <= 0 {
					t.Errorf("nextDelay returned non-positive duration %v", d)
					return
				}
			}
		}()
	}
	wg.Wait()
}
