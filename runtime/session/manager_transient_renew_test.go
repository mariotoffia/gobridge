package session

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// transientRenewStore fails exactly one Renew (the failRenewAt-th) with a
// TRANSIENT (non-definitive) error and returns a Current row the test configures
// up front. It drives the authoritative-read decision: after the transient
// failure hits MaxRenewFails, renewLoop calls Current to decide whether the
// lease was really lost.
type transientRenewStore struct {
	mu          sync.Mutex
	version     uint64
	acquires    int32
	renews      int32
	releases    int32
	failRenewAt int32
	// failDefinitiveOnce makes the NEXT Renew report a definitive lease loss,
	// which is how a test ends one term and re-acquires into the next.
	failDefinitiveOnce atomic.Bool
	onRenew            chan struct{}
	onCurrent          chan struct{}

	curOwner   string
	curExpires time.Time
	curErr     error
}

func (s *transientRenewStore) setCurrent(owner string, expires time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.curOwner = owner
	s.curExpires = expires
	s.curErr = err
}

func (s *transientRenewStore) Acquire(_ context.Context, _ string, ownerID string, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	atomic.AddInt32(&s.acquires, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	return persistence.LeaseToken{Version: s.version, Owner: ownerID}, nil
}

func (s *transientRenewStore) acquireCount() int32 { return atomic.LoadInt32(&s.acquires) }

func (s *transientRenewStore) Renew(_ context.Context, _ string, token persistence.LeaseToken, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	n := atomic.AddInt32(&s.renews, 1)
	if s.onRenew != nil {
		select {
		case s.onRenew <- struct{}{}:
		default:
		}
	}
	if s.failDefinitiveOnce.Swap(false) {
		// A DEFINITIVE loss: the lease is provably no longer ours, so the owner
		// steps down at once and the caller re-acquires in place.
		return persistence.LeaseToken{}, shared.ErrStaleFencingToken
	}
	if n == atomic.LoadInt32(&s.failRenewAt) {
		// Transient store error (NOT a definitive lease loss): timeout/throttle.
		return persistence.LeaseToken{}, shared.ErrUnavailable
	}
	return persistence.LeaseToken{Version: token.Version, Owner: token.Owner}, nil
}

func (s *transientRenewStore) Release(_ context.Context, _ string, _ persistence.LeaseToken) error {
	atomic.AddInt32(&s.releases, 1)
	return nil
}

func (s *transientRenewStore) Current(_ context.Context, leaseID string) (persistence.LeaseInfo, error) {
	if s.onCurrent != nil {
		select {
		case s.onCurrent <- struct{}{}:
		default:
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.curErr != nil {
		return persistence.LeaseInfo{}, s.curErr
	}
	return persistence.LeaseInfo{LeaseID: leaseID, Owner: s.curOwner, ExpiresAt: s.curExpires}, nil
}

func (s *transientRenewStore) releaseCount() int32 { return atomic.LoadInt32(&s.releases) }

// TestSessionManager_TransientRenewFailure_ChecksBeforeStepDown is the
// regression test for a run of TRANSIENT renew failures that reaches
// MaxRenewFails must NOT step down blindly. renewLoop first does one
// authoritative Current read; it steps down only when the lease is provably lost
// (or unverifiable), and treats a still-held lease as a no-op — converting a
// transient store blip (which, for a single-use MQTT owner, would otherwise
// restart the process fleet-wide) into a continuation.
func TestSessionManager_TransientRenewFailure_ChecksBeforeStepDown(t *testing.T) {
	t.Run("still_owner_no_stepdown", func(t *testing.T) {
		fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		renewCh := make(chan struct{}, 8)
		currentCh := make(chan struct{}, 8)
		store := &transientRenewStore{failRenewAt: 2, onRenew: renewCh, onCurrent: currentCh}
		// Authoritative read still shows us as the unexpired owner.
		store.setCurrent("owner-1", fake.Now().Add(time.Hour), nil)
		sess := newCountingSession()

		const renewInterval = 500 * time.Millisecond
		cfg := Config{
			SessionID:     "sess-f7a",
			Exclusive:     true,
			LeaseTTL:      5 * time.Second,
			RenewInterval: renewInterval,
			RenewJitter:   0,
			MaxRenewFails: 1,
			StepDownGrace: 20 * time.Millisecond,
		}
		mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

		ctx, cancel := context.WithCancel(context.Background())
		var runWG sync.WaitGroup
		runWG.Add(1)
		go func() { defer runWG.Done(); _ = mgr.Run(ctx) }()
		defer func() { cancel(); runWG.Wait(); _ = mgr.Close(context.Background()) }()

		wait.RequireReceive(t, sess.startedCh, 2*time.Second)
		wait.RequireReceive(t, sess.reconciledCh, 2*time.Second)
		wait.RequireReceive(t, sess.eventsReadCh, 2*time.Second)
		wait.Until(t, 2*time.Second, "renew timer registered", func() bool { return fake.TimerCount() >= 1 })

		// Renew #1 succeeds.
		fake.Advance(renewInterval)
		wait.RequireReceive(t, renewCh, 2*time.Second)
		wait.Until(t, 2*time.Second, "renew timer reset", func() bool { return fake.TimerCount() >= 1 })

		// Renew #2 fails transiently -> MaxRenewFails reached -> Current read.
		fake.Advance(renewInterval)
		wait.RequireReceive(t, renewCh, 2*time.Second)
		wait.RequireReceive(t, currentCh, 2*time.Second) // authoritative read ran

		// Still owner -> NO step-down: the loop resets its timer and keeps going.
		// Renew #3 (recovery) fires, proving the lease was retained.
		wait.Until(t, 2*time.Second, "renew timer reset no-op", func() bool { return fake.TimerCount() >= 1 })
		fake.Advance(renewInterval)
		wait.RequireReceive(t, renewCh, 2*time.Second)

		if store.releaseCount() != 0 {
			t.Fatalf("lease must NOT be released when the authoritative read still shows us as owner, releases=%d", store.releaseCount())
		}
		select {
		case <-sess.closedCh:
			t.Fatal("session must NOT be closed (no step-down) when the lease is still held")
		default:
		}
		if got := sess.startCount(); got != 1 {
			t.Fatalf("session must not have been restarted, startCount=%d", got)
		}
	})

	t.Run("lost_to_other_owner_steps_down", func(t *testing.T) {
		fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		renewCh := make(chan struct{}, 8)
		currentCh := make(chan struct{}, 8)
		store := &transientRenewStore{failRenewAt: 2, onRenew: renewCh, onCurrent: currentCh}
		// Authoritative read reveals a DIFFERENT owner: the lease is genuinely
		// lost, so the transient failure must escalate to a step-down.
		store.setCurrent("owner-2", fake.Now().Add(time.Hour), nil)
		sess := newCountingSession()

		const renewInterval = 500 * time.Millisecond
		cfg := Config{
			SessionID:     "sess-f7b",
			Exclusive:     true,
			LeaseTTL:      5 * time.Second,
			RenewInterval: renewInterval,
			RenewJitter:   0,
			MaxRenewFails: 1,
			StepDownGrace: 20 * time.Millisecond,
		}
		mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

		ctx, cancel := context.WithCancel(context.Background())
		var runWG sync.WaitGroup
		runWG.Add(1)
		go func() { defer runWG.Done(); _ = mgr.Run(ctx) }()
		defer func() { cancel(); runWG.Wait(); _ = mgr.Close(context.Background()) }()

		wait.RequireReceive(t, sess.startedCh, 2*time.Second)
		wait.RequireReceive(t, sess.reconciledCh, 2*time.Second)
		wait.RequireReceive(t, sess.eventsReadCh, 2*time.Second)
		wait.Until(t, 2*time.Second, "renew timer registered", func() bool { return fake.TimerCount() >= 1 })

		fake.Advance(renewInterval) // renew #1 success
		wait.RequireReceive(t, renewCh, 2*time.Second)
		wait.Until(t, 2*time.Second, "renew timer reset", func() bool { return fake.TimerCount() >= 1 })

		fake.Advance(renewInterval) // renew #2 fails transiently -> read
		wait.RequireReceive(t, renewCh, 2*time.Second)
		wait.RequireReceive(t, currentCh, 2*time.Second)

		// Lease lost -> step down: session closed and lease released.
		wait.RequireReceive(t, sess.closedCh, 3*time.Second)
		wait.Until(t, 3*time.Second, "lease released on genuine loss", func() bool { return store.releaseCount() >= 1 })
	})

	t.Run("current_error_steps_down", func(t *testing.T) {
		fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		renewCh := make(chan struct{}, 8)
		currentCh := make(chan struct{}, 8)
		store := &transientRenewStore{failRenewAt: 2, onRenew: renewCh, onCurrent: currentCh}
		// Authoritative read is UNVERIFIABLE (store unreachable). The
		// exclusive-safety posture is fail-closed: an unverifiable ownership check
		// after a transient streak must step down, not continue blind.
		store.setCurrent("", time.Time{}, shared.ErrUnavailable)
		sess := newCountingSession()

		const renewInterval = 500 * time.Millisecond
		cfg := Config{
			SessionID:     "sess-f7c",
			Exclusive:     true,
			LeaseTTL:      5 * time.Second,
			RenewInterval: renewInterval,
			RenewJitter:   0,
			MaxRenewFails: 1,
			StepDownGrace: 20 * time.Millisecond,
		}
		mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

		ctx, cancel := context.WithCancel(context.Background())
		var runWG sync.WaitGroup
		runWG.Add(1)
		go func() { defer runWG.Done(); _ = mgr.Run(ctx) }()
		defer func() { cancel(); runWG.Wait(); _ = mgr.Close(context.Background()) }()

		wait.RequireReceive(t, sess.startedCh, 2*time.Second)
		wait.RequireReceive(t, sess.reconciledCh, 2*time.Second)
		wait.RequireReceive(t, sess.eventsReadCh, 2*time.Second)
		wait.Until(t, 2*time.Second, "renew timer registered", func() bool { return fake.TimerCount() >= 1 })

		fake.Advance(renewInterval) // renew #1 success
		wait.RequireReceive(t, renewCh, 2*time.Second)
		wait.Until(t, 2*time.Second, "renew timer reset", func() bool { return fake.TimerCount() >= 1 })

		fake.Advance(renewInterval) // renew #2 fails transiently -> read
		wait.RequireReceive(t, renewCh, 2*time.Second)
		wait.RequireReceive(t, currentCh, 2*time.Second)

		// Unverifiable ownership -> fail-closed step-down.
		wait.RequireReceive(t, sess.closedCh, 3*time.Second)
		wait.Until(t, 3*time.Second, "lease released when ownership is unverifiable", func() bool { return store.releaseCount() >= 1 })
	})

	t.Run("expired_same_owner_steps_down", func(t *testing.T) {
		fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		renewCh := make(chan struct{}, 8)
		currentCh := make(chan struct{}, 8)
		store := &transientRenewStore{failRenewAt: 2, onRenew: renewCh, onCurrent: currentCh}
		// Authoritative read still NAMES us as owner but the row has already
		// EXPIRED: a same-owner-but-expired lease is lost (a standby may seize it
		// at any moment), so leaseStillHeld returns false and we must step down.
		store.setCurrent("owner-1", fake.Now().Add(-time.Hour), nil)
		sess := newCountingSession()

		const renewInterval = 500 * time.Millisecond
		cfg := Config{
			SessionID:     "sess-f7d",
			Exclusive:     true,
			LeaseTTL:      5 * time.Second,
			RenewInterval: renewInterval,
			RenewJitter:   0,
			MaxRenewFails: 1,
			StepDownGrace: 20 * time.Millisecond,
		}
		mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

		ctx, cancel := context.WithCancel(context.Background())
		var runWG sync.WaitGroup
		runWG.Add(1)
		go func() { defer runWG.Done(); _ = mgr.Run(ctx) }()
		defer func() { cancel(); runWG.Wait(); _ = mgr.Close(context.Background()) }()

		wait.RequireReceive(t, sess.startedCh, 2*time.Second)
		wait.RequireReceive(t, sess.reconciledCh, 2*time.Second)
		wait.RequireReceive(t, sess.eventsReadCh, 2*time.Second)
		wait.Until(t, 2*time.Second, "renew timer registered", func() bool { return fake.TimerCount() >= 1 })

		fake.Advance(renewInterval) // renew #1 success
		wait.RequireReceive(t, renewCh, 2*time.Second)
		wait.Until(t, 2*time.Second, "renew timer reset", func() bool { return fake.TimerCount() >= 1 })

		fake.Advance(renewInterval) // renew #2 fails transiently -> read
		wait.RequireReceive(t, renewCh, 2*time.Second)
		wait.RequireReceive(t, currentCh, 2*time.Second)

		// Same owner but expired -> lease lost -> step down.
		wait.RequireReceive(t, sess.closedCh, 3*time.Second)
		wait.Until(t, 3*time.Second, "lease released when the owned lease has expired", func() bool { return store.releaseCount() >= 1 })
	})
}

var _ ports.LeaseStore = (*transientRenewStore)(nil)
