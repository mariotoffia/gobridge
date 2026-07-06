package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
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
