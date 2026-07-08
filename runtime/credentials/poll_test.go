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
	"github.com/mariotoffia/gobridge/testutil/wait"
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

// TestPollBasedWrapper_EmitsRefreshFailureMetric verifies a resolve failure
// increments shared.MetricCredentialRefreshFailures on both the initial seed
// resolve and every periodic poll, so the log-only failure is also visible as
// a metric.
func TestPollBasedWrapper_EmitsRefreshFailureMetric(t *testing.T) {
	t.Parallel()

	pull := &fakePullStore{err: errors.New("backend down")}
	fake := clocktest.New()
	rec := &ports.RecordingExporter{}
	w := NewPollBasedWrapper(pull, ports.PollBasedWrapperConfig{
		PollInterval: time.Second,
		EmitOnStart:  true,
	}, WithPollClock(fake), WithPollMetrics(rec))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := w.Watch(ctx, "file://creds/fail")
	require.NoError(t, err)

	// The seed resolve fails and emits exactly one counter before the poll
	// timer is armed (the barrier below).
	require.Eventually(t, func() bool { return fake.TimerCount() >= 1 }, time.Second, time.Millisecond)
	seed := rec.FindEntries(shared.MetricCredentialRefreshFailures)
	require.Len(t, seed, 1, "seed resolve failure must emit one counter")
	require.Equal(t, "counter", seed[0].Kind)
	require.Equal(t, int64(1), seed[0].IValue)

	// One periodic poll: it also fails and emits a second counter.
	fake.Advance(time.Second)
	require.Eventually(t, func() bool {
		return len(rec.FindEntries(shared.MetricCredentialRefreshFailures)) >= 2
	}, time.Second, time.Millisecond, "periodic resolve failure must emit another counter")
}
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

// fakeUncachedPull implements both Resolve (the cached/build-time value)
// and ResolveUncached (fresh backend reads from a queue) with separate
// counters and sources so tests can prove which path the wrapper uses and
// exercise the F1 build-window seed divergence deterministically.
//
//   - Resolve      → returns cached (what a freshly built session holds).
//   - ResolveUncached → pops fresh from an independent queue (the backend).
type fakeUncachedPull struct {
	cached        *connectivity.CredentialSet
	cachedErr     error
	fresh         fakePullStore
	cachedCalls   atomic.Int64
	uncachedCalls atomic.Int64
}

func (f *fakeUncachedPull) Resolve(_ context.Context, _ string) (*connectivity.CredentialSet, error) {
	f.cachedCalls.Add(1)
	if f.cachedErr != nil {
		return nil, f.cachedErr
	}
	return f.cached, nil
}

func (f *fakeUncachedPull) ResolveUncached(ctx context.Context, uri string) (*connectivity.CredentialSet, error) {
	f.uncachedCalls.Add(1)
	return f.fresh.Resolve(ctx, uri)
}

var _ UncachedPullCredentialStore = (*fakeUncachedPull)(nil)

// TestPollBasedWrapper_PrefersUncachedResolve verifies that when the
// wrapped store exposes ResolveUncached, the poll TICKS bypass the cached
// Resolve path — a rotation poll that reads a TTL cache cannot detect
// rotations before the TTL expires. The seed performs exactly ONE cached
// read to learn the build-time baseline (F1); all subsequent poll reads
// are uncached.
func TestPollBasedWrapper_PrefersUncachedResolve(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	// Build-time cached value == first fresh value, so the seed does not
	// diverge; the rotation surfaces on the first uncached tick.
	pull := &fakeUncachedPull{
		cached: pwd("u1", "p1"),
		fresh: fakePullStore{queue: []*connectivity.CredentialSet{
			pwd("u1", "p1"), // seed fresh (matches cached → no seed rotation)
			pwd("u2", "p2"), // rotation, surfaced on first tick
		}},
	}

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

	require.Equal(t, int64(1), pull.cachedCalls.Load(), "seed must read the cached baseline exactly once (F1); ticks must be uncached")
	require.GreaterOrEqual(t, pull.uncachedCalls.Load(), int64(2), "ticks must bypass the cache or rotations hide behind the TTL")
}

// TestPollBasedWrapper_SeedSurfacesBuildWindowRotation is the F1 regression
// test: a rotation that lands in the build→watch window (the freshly built
// session holds the STALE build-time cached value, but the backend already
// holds the ROTATED value) must be surfaced as a rotation at SEED time —
// immediately, without waiting for a poll tick — EVEN WHEN EmitOnStart is
// false (the shipped production default before this fix).
//
// Counterfactual: before the F1 fix the seed adopted the fresh uncached
// value as the dedup baseline and, with EmitOnStart=false, emitted nothing;
// every later poll then saw "no change" and the session ran on the
// rotated-out (revoked) credentials forever. With the fix, the seed compares
// the cached baseline against the fresh value and emits the divergence.
func TestPollBasedWrapper_SeedSurfacesBuildWindowRotation(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	// cached == build-time (STALE) value the session was built with.
	// fresh  == backend value AFTER the rotation that landed in the window.
	pull := &fakeUncachedPull{
		cached: pwd("stale-user", "stale-pass"),
		fresh: fakePullStore{queue: []*connectivity.CredentialSet{
			pwd("fresh-user", "fresh-pass"),
		}},
	}

	var rotations atomic.Int64
	w := NewPollBasedWrapper(pull, ports.PollBasedWrapperConfig{
		PollInterval: time.Hour, // large: prove emission is at SEED, not a tick
		EmitOnStart:  false,     // the pre-fix shipped production default
	}, WithPollClock(fake), WithOnRotation(func(string) { rotations.Add(1) }))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ch, err := w.Watch(ctx, "file://creds/a")
	require.NoError(t, err)

	// The rotation must be emitted at seed WITHOUT advancing the clock.
	select {
	case got := <-ch:
		require.Equal(t, "fresh-user", got.Password().Username(),
			"seed must emit the fresh (rotated) value, not the stale build-time value")
	case <-time.After(2 * time.Second):
		t.Fatal("F1 regression: rotation landing in the build window was silently swallowed")
	}
	require.Equal(t, int64(1), rotations.Load(), "OnRotation must fire for a build-window rotation")
}

// TestPollBasedWrapper_RefreshForcesImmediateReResolve is the F2 regression
// test: after a hard rotation the broker rejects the live credentials
// (NOT_AUTHORIZED). The transport/refresher calls Refresh(uri) to force an
// out-of-band re-resolve INSTEAD of waiting up to a full PollInterval
// (default 5m) for the timer. The fresh secret must be delivered without
// advancing the poll timer, and Refresh must be rate limited per URI so a
// reconnect storm collapses into one backend fetch per interval.
func TestPollBasedWrapper_RefreshForcesImmediateReResolve(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	pull := &fakeUncachedPull{
		cached: pwd("u1", "p1"),
		fresh: fakePullStore{queue: []*connectivity.CredentialSet{
			pwd("u1", "p1"), // seed fresh (== cached → no seed emission)
			pwd("u2", "p2"), // delivered by the reactive Refresh, NOT a tick
		}},
	}

	const reactiveInterval = time.Second
	w := NewPollBasedWrapper(pull, ports.PollBasedWrapperConfig{
		PollInterval: time.Hour, // large: prove delivery is reactive, not a tick
		EmitOnStart:  false,
	}, WithPollClock(fake), WithReactiveReResolveInterval(reactiveInterval))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ch, err := w.Watch(ctx, "file://creds/a")
	require.NoError(t, err)

	// Wait until the watch goroutine has registered its nudge channel and
	// installed its timer (the seed resolve has completed by then).
	wait.Until(t, 2*time.Second, "watch installed timer", func() bool { return fake.TimerCount() >= 1 })
	require.Equal(t, int64(1), pull.uncachedCalls.Load(), "only the seed resolve so far")

	// Reactive re-resolve — no clock advance. The fresh (rotated) value must
	// be delivered immediately.
	w.Refresh("file://creds/a")
	got := wait.RequireReceive(t, ch, 2*time.Second)
	require.Equal(t, "u2", got.Password().Username(), "Refresh must deliver the fresh credentials without waiting for a tick")
	require.Equal(t, int64(2), pull.uncachedCalls.Load(), "exactly one reactive resolve")

	// Rate limiting: a second Refresh within reactiveInterval (clock not
	// advanced) must be dropped — no extra backend fetch.
	w.Refresh("file://creds/a")
	wait.Silent(t, ch, 100*time.Millisecond)
	require.Equal(t, int64(2), pull.uncachedCalls.Load(), "Refresh must be rate limited within the interval")

	// After the interval elapses, Refresh is honoured again.
	fake.Advance(reactiveInterval)
	w.Refresh("file://creds/a")
	wait.Until(t, 2*time.Second, "reactive resolve after interval", func() bool { return pull.uncachedCalls.Load() >= 3 })
}

// TestPollBasedWrapper_RefreshUnknownURIIsNoop verifies Refresh is a safe
// no-op for a URI that is not being watched (no panic, no state recorded).
func TestPollBasedWrapper_RefreshUnknownURIIsNoop(t *testing.T) {
	t.Parallel()

	w := NewPollBasedWrapper(&fakePullStore{}, ports.PollBasedWrapperConfig{PollInterval: time.Second})
	require.NotPanics(t, func() { w.Refresh("file://never/watched") })
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
