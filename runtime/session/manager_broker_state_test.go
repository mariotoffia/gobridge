package session

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

		_, ok := mgr.MayEndBrokerState()
		assert.True(t, ok)
	})

	t.Run("held lease past its deadline", func(t *testing.T) {
		fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		mgr := newManager(fake, true)
		holdLease(mgr, fake)
		fake.Advance(leaseTTL)

		_, ok := mgr.MayEndBrokerState()
		assert.False(t, ok, "the lease may already be held by another instance")
	})

	t.Run("lease not held", func(t *testing.T) {
		fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		mgr := newManager(fake, true)

		_, ok := mgr.MayEndBrokerState()
		assert.False(t, ok, "a standby never connected as the broker identity")
	})

	t.Run("non-exclusive session", func(t *testing.T) {
		fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		mgr := newManager(fake, false)

		_, ok := mgr.MayEndBrokerState()
		assert.True(t, ok)
	})
}

// endingCountingSession is a countingSession whose broker state can be ended;
// it records the deadline it was asked to end that state by.
type endingCountingSession struct {
	*countingSession
	askMu  sync.Mutex
	asked  bool
	before time.Time
}

func (s *endingCountingSession) EndBrokerStateOnClose(before time.Time) {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	s.asked, s.before = true, before
}

func (s *endingCountingSession) ask() (bool, time.Time) {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	return s.asked, s.before
}

var _ ports.BrokerStateEnder = (*endingCountingSession)(nil)

// TestManager_CloseEndingBrokerState_BoundsTheEndingByTheLeaseDeadline pins
// that the manager asks its session to finish ending its broker state by the
// local deadline of the lease it holds, past which a standby may own the broker
// identity, and leaves the ending unbounded for a session that takes part in
// no lease-based failover.
//
// Mutation check: ask with a zero deadline in MayEndBrokerState and the
// exclusive case fails.
func TestManager_CloseEndingBrokerState_BoundsTheEndingByTheLeaseDeadline(t *testing.T) {
	const leaseTTL = 5 * time.Second
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newManager := func(exclusive bool) (*Manager, *endingCountingSession) {
		fake := clocktest.NewAt(start)
		sess := &endingCountingSession{countingSession: newCountingSession()}
		cfg := Config{SessionID: "s1", Exclusive: exclusive, LeaseTTL: leaseTTL}
		mgr := NewWithMetrics(cfg, sess, newLeaseLossStore(100, nil), "owner-1", nil,
			&ports.NoopExporter{}, clock.Clock(fake))
		fake.Advance(time.Second)
		return mgr, sess
	}

	t.Run("exclusive session", func(t *testing.T) {
		mgr, sess := newManager(true)
		mgr.recordLeaseDeadline(start)
		mgr.setToken(persistence.LeaseToken{Version: 1, Owner: "owner-1"})

		require.NoError(t, mgr.CloseEndingBrokerState(t.Context()))

		asked, before := sess.ask()
		assert.True(t, asked)
		assert.Equal(t, start.Add(leaseTTL), before)
	})

	t.Run("non-exclusive session", func(t *testing.T) {
		mgr, sess := newManager(false)

		require.NoError(t, mgr.CloseEndingBrokerState(t.Context()))

		asked, before := sess.ask()
		assert.True(t, asked)
		assert.Zero(t, before, "a session no lease guards is not bounded by one")
	})
}
