package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
)

// fakePullStore returns a new CredentialSet from a queue on each
// Resolve call. Once the queue drains it keeps returning the last
// value, mirroring a stable backend.
type fakePullStore struct {
	mu    sync.Mutex
	queue []*domain.CredentialSet
	calls int64
	err   error
}

func (f *fakePullStore) Resolve(_ context.Context, _ string) (*domain.CredentialSet, error) {
	atomic.AddInt64(&f.calls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if len(f.queue) == 0 {
		return nil, domain.ErrNotFound
	}
	head := f.queue[0]
	if len(f.queue) > 1 {
		f.queue = f.queue[1:]
	}
	return head, nil
}

func pwd(user, pass string) *domain.CredentialSet {
	return &domain.CredentialSet{Password: &domain.PasswordCredential{Username: user, Password: pass}}
}

// TestPollBasedWrapper_EmitsOnChange verifies that the wrapper publishes
// a new credential set only when Resolve returns a value that differs
// from the previously observed one, and suppresses duplicates.
func TestPollBasedWrapper_EmitsOnChange(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	pull := &fakePullStore{queue: []*domain.CredentialSet{
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
		require.Equal(t, "u1", got.Password.Username)
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
		t.Fatalf("unexpected emission on duplicate: %+v", got.Password)
	case <-time.After(100 * time.Millisecond):
	}

	// Second tick: change to (u2,p2) — must emit.
	require.Eventually(t, func() bool { return fake.TimerCount() >= 1 }, time.Second, time.Millisecond)
	fake.Advance(time.Second)
	select {
	case got := <-ch:
		require.Equal(t, "u2", got.Password.Username)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for changed emission")
	}

	// Third tick: duplicate — must NOT emit.
	require.Eventually(t, func() bool { return fake.TimerCount() >= 1 }, time.Second, time.Millisecond)
	fake.Advance(time.Second)
	select {
	case got := <-ch:
		t.Fatalf("unexpected emission: %+v", got.Password)
	case <-time.After(100 * time.Millisecond):
	}

	// Fourth tick: change to (u3,p3) — must emit.
	require.Eventually(t, func() bool { return fake.TimerCount() >= 1 }, time.Second, time.Millisecond)
	fake.Advance(time.Second)
	select {
	case got := <-ch:
		require.Equal(t, "u3", got.Password.Username)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for final emission")
	}
}

// TestPollBasedWrapper_ContextCancel verifies the channel is closed
// once the caller cancels the context.
func TestPollBasedWrapper_ContextCancel(t *testing.T) {
	t.Parallel()

	pull := &fakePullStore{queue: []*domain.CredentialSet{pwd("u", "p")}}
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
