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

// partitionRenewStore models the split-brain-inducing asymmetric partition
// (finding: renew-write-fails / Current-read-succeeds). Every Renew fails with a
// TRANSIENT error (write path partitioned), while Current keeps returning THIS
// owner with a far-future ExpiresAt (read path healthy / stale). Without the
// local-lease-deadline gate the renew loop's authoritative-read mitigation
// would re-arm on every Current success and keep the owner "active" indefinitely
// past its own expiry — the ~97s dual-consumer window. With the fix the owner
// must step down once its local lease deadline (last successful acquire/renew +
// LeaseTTL) passes, regardless of the still-optimistic Current read.
type partitionRenewStore struct {
	mu        sync.Mutex
	version   uint64
	releases  int32
	onRenew   chan struct{}
	onCurrent chan struct{}
	curOwner  string
	curExp    time.Time
}

func (s *partitionRenewStore) Acquire(_ context.Context, _ string, ownerID string, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	return persistence.LeaseToken{Version: s.version, Owner: ownerID}, nil
}

func (s *partitionRenewStore) Renew(_ context.Context, _ string, _ persistence.LeaseToken, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	if s.onRenew != nil {
		select {
		case s.onRenew <- struct{}{}:
		default:
		}
	}
	// Write path is partitioned: renewal always fails transiently.
	return persistence.LeaseToken{}, shared.ErrUnavailable
}

func (s *partitionRenewStore) Release(_ context.Context, _ string, _ persistence.LeaseToken) error {
	atomic.AddInt32(&s.releases, 1)
	return nil
}

func (s *partitionRenewStore) Current(_ context.Context, leaseID string) (persistence.LeaseInfo, error) {
	if s.onCurrent != nil {
		select {
		case s.onCurrent <- struct{}{}:
		default:
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Read path stays healthy AND optimistic: still names us, still unexpired.
	return persistence.LeaseInfo{LeaseID: leaseID, Owner: s.curOwner, ExpiresAt: s.curExp}, nil
}

func (s *partitionRenewStore) releaseCount() int32 { return atomic.LoadInt32(&s.releases) }

var _ ports.LeaseStore = (*partitionRenewStore)(nil)

// TestSessionManager_RenewFailReadSucceed_ForcesStepDownPastDeadline is the
// regression test for the CRITICAL split-brain finding
// (manager_lease.go:495): a write-fails / read-succeeds partition must NOT keep
// an EXPIRED owner active on the strength of the authoritative Current read. The
// owner is bounded by its own local lease deadline and must fail closed (step
// down: close the source session + release the lease) once that deadline passes.
func TestSessionManager_RenewFailReadSucceed_ForcesStepDownPastDeadline(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	renewCh := make(chan struct{}, 8)
	currentCh := make(chan struct{}, 8)
	store := &partitionRenewStore{
		onRenew:   renewCh,
		onCurrent: currentCh,
		curOwner:  "owner-1",
		// Current NEVER reports us as expired — the whole point of the split brain.
		curExp: fake.Now().Add(time.Hour),
	}
	sess := newCountingSession()

	const (
		leaseTTL      = 5 * time.Second
		renewInterval = 1 * time.Second
	)
	cfg := Config{
		SessionID:     "sess-splitbrain",
		Exclusive:     true,
		LeaseTTL:      leaseTTL,
		RenewInterval: renewInterval,
		RenewJitter:   0,
		MaxRenewFails: 1,
		StepDownGrace: 20 * time.Millisecond,
	}
	rec := &ports.RecordingExporter{}
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, rec, clock.Clock(fake))

	ctx, cancel := context.WithCancel(context.Background())
	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() { defer runWG.Done(); _ = mgr.Run(ctx) }()
	defer func() { cancel(); runWG.Wait(); _ = mgr.Close(context.Background()) }()

	wait.RequireReceive(t, sess.startedCh, 2*time.Second)
	wait.RequireReceive(t, sess.reconciledCh, 2*time.Second)
	wait.RequireReceive(t, sess.eventsReadCh, 2*time.Second)
	wait.Until(t, 2*time.Second, "renew timer registered", func() bool { return fake.TimerCount() >= 1 })

	// One renew cycle BEFORE expiry: the write fails, MaxRenewFails is reached,
	// and the authoritative read (Current) still names us — so PRE-expiry the
	// owner legitimately keeps the lease (proves the read path is succeeding).
	fake.Advance(renewInterval)
	wait.RequireReceive(t, renewCh, 2*time.Second)
	wait.RequireReceive(t, currentCh, 2*time.Second)
	if store.releaseCount() != 0 {
		t.Fatalf("pre-expiry: lease must NOT be released while still within its deadline, releases=%d", store.releaseCount())
	}
	select {
	case <-sess.closedCh:
		t.Fatal("pre-expiry: session must NOT be closed while still within its deadline")
	default:
	}
	wait.Until(t, 2*time.Second, "renew timer reset no-op", func() bool { return fake.TimerCount() >= 1 })

	// Now cross the local lease deadline (last successful acquire at t0 + TTL).
	// The write path is still partitioned and Current would STILL name us, but
	// the fail-closed deadline gate must force a step-down regardless.
	fake.Advance(2 * leaseTTL)

	wait.RequireReceive(t, sess.closedCh, 3*time.Second)
	wait.Until(t, 3*time.Second, "lease released on fail-closed deadline step-down", func() bool { return store.releaseCount() >= 1 })

	// The fail-closed deadline gate must also emit the lease-expiry counter so
	// operators can alert on split-brain-preventing forced step-downs.
	wait.Until(t, 2*time.Second, "LeaseExpiries counter emitted on deadline-gate step-down",
		func() bool { return len(rec.FindEntries(shared.MetricLeaseExpiries)) >= 1 })
}

// TestSessionManager_TransientBlipBeforeDeadline_NoStepDown proves the deadline
// gate does NOT cause a spurious step-down: a transient renew failure that
// recovers BEFORE the local lease deadline (each successful renew extending the
// deadline) keeps the lease. This is the counterpart to the split-brain test —
// the fix must fail closed only past expiry, never punish a recoverable blip.
func TestSessionManager_TransientBlipBeforeDeadline_NoStepDown(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	renewCh := make(chan struct{}, 8)
	currentCh := make(chan struct{}, 8)
	// Fails exactly renew #2 transiently, then recovers on renew #3.
	store := &transientRenewStore{failRenewAt: 2, onRenew: renewCh, onCurrent: currentCh}
	store.setCurrent("owner-1", fake.Now().Add(time.Hour), nil)
	sess := newCountingSession()

	const renewInterval = 500 * time.Millisecond
	cfg := Config{
		SessionID:     "sess-blip",
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

	// Renew #1 succeeds (deadline extends well before any advance nears TTL).
	fake.Advance(renewInterval)
	wait.RequireReceive(t, renewCh, 2*time.Second)
	wait.Until(t, 2*time.Second, "renew timer reset", func() bool { return fake.TimerCount() >= 1 })

	// Renew #2 fails transiently -> Current read still owner -> no step-down.
	fake.Advance(renewInterval)
	wait.RequireReceive(t, renewCh, 2*time.Second)
	wait.RequireReceive(t, currentCh, 2*time.Second)
	wait.Until(t, 2*time.Second, "renew timer reset no-op", func() bool { return fake.TimerCount() >= 1 })

	// Renew #3 recovers, all still within the (extended) deadline.
	fake.Advance(renewInterval)
	wait.RequireReceive(t, renewCh, 2*time.Second)

	if store.releaseCount() != 0 {
		t.Fatalf("recoverable blip must NOT release the lease, releases=%d", store.releaseCount())
	}
	select {
	case <-sess.closedCh:
		t.Fatal("recoverable blip must NOT close the session (no step-down)")
	default:
	}
	if got := sess.startCount(); got != 1 {
		t.Fatalf("session must not have been restarted, startCount=%d", got)
	}
	if mgr.leaseDeadlinePassed() {
		t.Fatal("local lease deadline must NOT be reported passed within the lease window")
	}
}

// TestNewManager_StepDownGraceClampedBelowLeaseTTL is the regression test for
// the HIGH finding (manager.go:160): the construction path must not accept a
// StepDownGrace >= LeaseTTL (a stepping-down owner would drain past its own
// lease and overlap the new owner). Config.Validate rejects it as a hard error;
// newManager — which returns no error — must defensively clamp it below the TTL.
func TestNewManager_StepDownGraceClampedBelowLeaseTTL(t *testing.T) {
	t.Run("clamped_on_construction", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			grace time.Duration
		}{
			{"grace_equal_ttl", 10 * time.Second},
			{"grace_over_ttl", 30 * time.Second},
		} {
			t.Run(tc.name, func(t *testing.T) {
				cfg := Config{
					SessionID:     "sess-grace",
					Exclusive:     true,
					LeaseTTL:      10 * time.Second,
					StepDownGrace: tc.grace,
				}
				mgr := NewFromConfig(cfg, newCountingSession(), &partitionRenewStore{}, "owner-1", nil)
				if mgr.stepDownGrace >= mgr.leaseTTL {
					t.Fatalf("StepDownGrace=%s must be clamped below LeaseTTL=%s, got %s",
						tc.grace, mgr.leaseTTL, mgr.stepDownGrace)
				}
				if want := mgr.leaseTTL / 2; mgr.stepDownGrace != want {
					t.Fatalf("clamped StepDownGrace: got %s, want %s", mgr.stepDownGrace, want)
				}
			})
		}
	})

	t.Run("valid_grace_untouched", func(t *testing.T) {
		cfg := Config{
			SessionID:     "sess-grace-ok",
			Exclusive:     true,
			LeaseTTL:      10 * time.Second,
			StepDownGrace: 2 * time.Second,
		}
		mgr := NewFromConfig(cfg, newCountingSession(), &partitionRenewStore{}, "owner-1", nil)
		if mgr.stepDownGrace != 2*time.Second {
			t.Fatalf("valid StepDownGrace must be untouched, got %s", mgr.stepDownGrace)
		}
	})

	t.Run("validate_rejects", func(t *testing.T) {
		cfg := Config{
			SessionID:     "sess-grace",
			Exclusive:     true,
			LeaseTTL:      10 * time.Second,
			StepDownGrace: 10 * time.Second,
		}
		if err := cfg.Validate(); err == nil {
			t.Fatal("Config.Validate must reject StepDownGrace >= LeaseTTL")
		}
	})
}
