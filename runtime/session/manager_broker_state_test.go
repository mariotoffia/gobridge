package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
)

// TestManager_MayEndBrokerState_OnlyUnderALiveLease pins that an exclusive
// session ends its broker state only while its lease is held and its local,
// fail-closed deadline has not passed: past that deadline another instance may
// already hold the lease and be connected as the broker identity. A session
// that takes part in no lease-based failover may always end it.
func TestManager_MayEndBrokerState_OnlyUnderALiveLease(t *testing.T) {
	const leaseTTL = 5 * time.Second
	newManager := func(fake *clocktest.Fake, exclusive bool) *Manager {
		cfg := Config{SessionID: "s1", Exclusive: exclusive, LeaseTTL: leaseTTL}
		return NewWithMetrics(cfg, newCountingSession(), newLeaseLossStore(100, nil), "owner-1", nil,
			&ports.NoopExporter{}, clock.Clock(fake))
	}
	holdLease := func(mgr *Manager, fake *clocktest.Fake) {
		mgr.recordLeaseDeadline(fake.Now())
		mgr.setToken(persistence.LeaseToken{Version: 1, Owner: "owner-1"})
	}

	t.Run("held lease within its deadline", func(t *testing.T) {
		fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		mgr := newManager(fake, true)
		holdLease(mgr, fake)
		fake.Advance(leaseTTL - time.Nanosecond)

		assert.True(t, mgr.MayEndBrokerState())
	})

	t.Run("held lease past its deadline", func(t *testing.T) {
		fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		mgr := newManager(fake, true)
		holdLease(mgr, fake)
		fake.Advance(leaseTTL)

		assert.False(t, mgr.MayEndBrokerState(), "the lease may already be held by another instance")
	})

	t.Run("lease not held", func(t *testing.T) {
		fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		mgr := newManager(fake, true)

		assert.False(t, mgr.MayEndBrokerState(), "a standby never connected as the broker identity")
	})

	t.Run("non-exclusive session", func(t *testing.T) {
		fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		mgr := newManager(fake, false)

		assert.True(t, mgr.MayEndBrokerState())
	})
}
