package session

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// Finding M-close: Manager.Close must release a held lease under a DETACHED,
// bounded context, so an embedder that calls Close AFTER cancelling the ctx it
// passed still releases the lease. Otherwise a cancelled ctx silently skips
// Release and the partition stays owned for a full TTL before a standby can take
// over.
func TestManager_Close_ReleasesLeaseWithCancelledCtx(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	store := newLeaseLossStore(100, nil)
	sess := newCountingSession()
	cfg := Config{
		SessionID:     "sess-close",
		Exclusive:     true,
		LeaseTTL:      5 * time.Second,
		RenewInterval: 500 * time.Millisecond,
		MaxRenewFails: 1,
		StepDownGrace: 20 * time.Millisecond,
	}
	mgr := NewWithMetrics(cfg, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	// Simulate a live term that holds the lease.
	mgr.mu.Lock()
	mgr.hasLease = true
	mgr.token = persistence.LeaseToken{Version: 1, Owner: "owner-1"}
	mgr.mu.Unlock()

	// The caller's ctx is ALREADY cancelled when Close is invoked.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, mgr.Close(ctx))

	assert.Equal(t, int32(1), store.releaseCount(),
		"Close must release a held lease even when the caller ctx is cancelled")
	select {
	case <-sess.closedCh:
	default:
		t.Fatal("Close must close the underlying session")
	}
}

// TestManager_Close_ClosesSourceBeforeReleasingLease pins the teardown ORDER on
// the manager's own shutdown path, matching the discipline the session-failure,
// step-down and activation-failure paths already enforce.
//
// Releasing first hands the partition to a standby while THIS node's source
// session is still connected and subscribed for the whole duration of
// session.Close: the standby activates and consumes alongside an owner that has
// not stopped yet. MQTT masks it through client-ID takeover, but for an
// exclusive AMQP 0-9-1 / 1.0 consumer it is a real dual-consumer window.
//
// The assertion is ordering-based: Close's global-ordering sequence must be
// strictly BEFORE the lease Release's.
func TestManager_Close_ClosesSourceBeforeReleasingLease(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	var seq atomic.Int64
	store := newSeqLeaseStore(&seq)
	sess := newFailReconcileSession(&seq)

	mgr := NewWithMetrics(Config{
		SessionID:     "sess-close-order",
		Exclusive:     true,
		LeaseTTL:      5 * time.Second,
		RenewInterval: 500 * time.Millisecond,
		MaxRenewFails: 1,
		StepDownGrace: 20 * time.Millisecond,
	}, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	mgr.mu.Lock()
	mgr.hasLease = true
	mgr.token = persistence.LeaseToken{Version: 1, Owner: "owner-1"}
	mgr.mu.Unlock()

	require.NoError(t, mgr.Close(context.Background()))

	closedAt := sess.closeOrder()
	releasedAt := store.releaseOrder()
	require.Positive(t, closedAt, "Close must close the source session")
	require.Positive(t, releasedAt, "Close must release a held lease")
	assert.Less(t, closedAt, releasedAt,
		"the source must stop consuming BEFORE the lease becomes seizable by a standby")
}

// TestManager_Close_WedgedSourceSkipsLeaseRelease pins the wedged-close
// discipline on the manager's shutdown path: when the transport's Close IGNORES
// its context and only the manager's hard ceiling unblocks it, the source is
// STILL subscribed. Handing the lease to a standby then guarantees the overlap
// the lease exists to prevent, so the release is skipped and the lease expires
// only by natural TTL — after the process has exited and the OS has torn the
// socket down.
func TestManager_Close_WedgedSourceSkipsLeaseRelease(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	var seq atomic.Int64
	store := newSeqLeaseStore(&seq)
	sess := newWedgedCloseSession()
	t.Cleanup(func() { close(sess.release) })

	mgr := NewWithMetrics(Config{
		SessionID:     "sess-close-wedged",
		Exclusive:     true,
		LeaseTTL:      5 * time.Second,
		RenewInterval: 500 * time.Millisecond,
		MaxRenewFails: 1,
		// The bounded-close ceiling equals releaseTimeout == StepDownGrace.
		StepDownGrace: 20 * time.Millisecond,
	}, sess, store, "owner-1", nil, &ports.NoopExporter{}, clock.Clock(fake))

	mgr.mu.Lock()
	mgr.hasLease = true
	mgr.token = persistence.LeaseToken{Version: 1, Owner: "owner-1"}
	mgr.mu.Unlock()

	closeErr := make(chan error, 1)
	go func() { closeErr <- mgr.Close(context.Background()) }()

	wait.RequireReceive(t, sess.closeEntered, 2*time.Second)
	waitTimerCount(t, fake, 1, 2*time.Second)
	fake.Advance(20 * time.Millisecond)

	err := wait.RequireReceive(t, closeErr, 3*time.Second)
	require.Error(t, err, "a source Close that never returned must be reported, not swallowed")
	assert.Zero(t, store.releaseOrder(),
		"a wedged, still-subscribed source must not hand its lease to a standby")
}
