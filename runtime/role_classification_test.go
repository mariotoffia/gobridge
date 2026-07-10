package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// roleFakeSession is a no-op ports.Session used to drive a session.Manager's
// lease lifecycle in the role-classification test.
type roleFakeSession struct{ events chan ports.SessionEvent }

func (s *roleFakeSession) Start(context.Context) error                               { return nil }
func (s *roleFakeSession) Reconcile(context.Context, connectivity.SessionPlan) error { return nil }
func (s *roleFakeSession) Health(context.Context) ports.SessionHealth {
	return ports.SessionHealth{Connected: true, Ready: true, ServiceLevel: ports.ServiceLevelFull}
}
func (s *roleFakeSession) Events() <-chan ports.SessionEvent { return s.events }
func (s *roleFakeSession) Close(context.Context) error       { return nil }

// roleGrantingLeaseStore always grants the lease to the caller, so an exclusive
// manager reaches the lease-held (active) state.
type roleGrantingLeaseStore struct{ v uint64 }

func (s *roleGrantingLeaseStore) Acquire(_ context.Context, _, ownerID string, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	s.v++
	return persistence.LeaseToken{Version: s.v, Owner: ownerID}, nil
}
func (s *roleGrantingLeaseStore) Renew(_ context.Context, _ string, token persistence.LeaseToken, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	return token, nil
}
func (s *roleGrantingLeaseStore) Release(context.Context, string, persistence.LeaseToken) error {
	return nil
}
func (s *roleGrantingLeaseStore) Current(_ context.Context, leaseID string) (persistence.LeaseInfo, error) {
	return persistence.LeaseInfo{LeaseID: leaseID, Owner: "owner-role-test", Version: s.v}, nil
}

func newRoleTestManager(exclusive bool, ls ports.LeaseStore) *session.Manager {
	return session.NewFromConfig(
		session.Config{SessionID: "s", Exclusive: exclusive},
		&roleFakeSession{events: make(chan ports.SessionEvent, 1)},
		ls, "owner-role-test", nil,
	)
}

// TestRoleUnlocked_ClassifiesByExclusiveSessionsOnly pins the role contract to
// its documentation (bridge_health.go:107-110): an instance is "standalone" when
// NO exclusive session is configured, "standby" when an exclusive session exists
// but holds no lease, and "active" when an exclusive session holds a lease. A
// NON-exclusive session takes no part in lease failover and MUST NOT make the
// instance look like a standby.
//
// Regression + mutation: revert roleUnlocked to the len(rt.sessionMgrs)-based
// version and the "non-exclusive" case flips to standby — the classification the
// readiness cap then pins below LevelFull, stranding the canonical single-session
// standalone bridge on the legacy /ready probe. The httpapi boundary test
// TestRegression_LegacyReadyGreenForStandaloneNonExclusiveBridge proves the
// end-to-end 200-vs-503 consequence.
func TestRoleUnlocked_ClassifiesByExclusiveSessionsOnly(t *testing.T) {
	t.Run("non-exclusive session is standalone (not standby)", func(t *testing.T) {
		rt := &Runtime{sessionMgrs: map[string]*session.Manager{
			"s": newRoleTestManager(false, nil),
		}}
		require.Equal(t, roleStandalone, rt.Role(),
			"a non-exclusive session must not classify the instance as standby")
	})

	t.Run("exclusive session without a lease is standby", func(t *testing.T) {
		rt := &Runtime{sessionMgrs: map[string]*session.Manager{
			"s": newRoleTestManager(true, nil),
		}}
		require.Equal(t, roleStandby, rt.Role())
	})

	t.Run("exclusive session holding a lease is active", func(t *testing.T) {
		mgr := newRoleTestManager(true, &roleGrantingLeaseStore{})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = mgr.Run(ctx) }()
		t.Cleanup(func() { cancel(); wait.RequireClosed(t, done, 2*time.Second) })

		// setToken precedes the LeaseStateAcquired event (manager_lease.go:221,232),
		// so observing the event guarantees Token() is held — no sleep-poll needed.
		ev := wait.RequireReceive(t, mgr.LeaseStateChanged(), 2*time.Second)
		require.Equal(t, session.LeaseStateAcquired, ev.State)

		rt := &Runtime{sessionMgrs: map[string]*session.Manager{"s": mgr}}
		require.Equal(t, roleActive, rt.Role())
	})
}
