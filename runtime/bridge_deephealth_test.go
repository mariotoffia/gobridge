package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// fixedHealthSession is a ports.Session whose Health reports a fixed snapshot,
// so a test can follow each reported field into the deep-health projection.
type fixedHealthSession struct{ health ports.SessionHealth }

func (s *fixedHealthSession) Start(context.Context) error                               { return nil }
func (s *fixedHealthSession) Reconcile(context.Context, connectivity.SessionPlan) error { return nil }
func (s *fixedHealthSession) Health(context.Context) ports.SessionHealth                { return s.health }
func (s *fixedHealthSession) Events() <-chan ports.SessionEvent                         { return nil }
func (s *fixedHealthSession) Close(context.Context) error                               { return nil }

// TestDeepHealth_SessionDetailCarriesReportedHealth pins the session projection:
// what a session reports through Health reaches its SessionHealthDetail
// unchanged — including the subscriptions it accepted below the requested QoS
// as best effort, which an operator can read from the detail and nowhere else.
func TestDeepHealth_SessionDetailCarriesReportedHealth(t *testing.T) {
	satisfied := true
	sess := &fixedHealthSession{health: ports.SessionHealth{
		Connected:                true,
		SubscriptionsWanted:      2,
		SubscriptionsActive:      2,
		SubscriptionsSatisfied:   &satisfied,
		UnsettledCount:           3,
		OldestUnsettledAge:       4 * time.Second,
		ReceiveWindowUtilization: 0.75,
		RecoveryRecycleCount:     5,
		Ready:                    true,
		ServiceLevel:             ports.ServiceLevelFull,
		ActiveTopics:             []string{"sensors/x", "sensors/y"},
		BestEffortTopics:         []string{"sensors/x"},
	}}
	rt := &Runtime{
		// Never advanced, so the shared probe deadline cannot fire and the sweep
		// always reads the session's own report.
		clk:             clocktest.NewAt(time.Unix(0, 0)),
		componentErrors: make(map[string]error),
		running:         true,
		healthy:         true,
		sessionSenders:  map[string]*sessionSenderEntry{},
		sessionMgrs:     map[string]*session.Manager{},
		entries: []*routeEntry{{
			config:  RouteConfig{ID: "r1"},
			session: sess,
			sessCfg: &session.Config{SessionID: "s1"},
		}},
	}

	dh := rt.DeepHealth(context.Background())

	require.Len(t, dh.Sessions, 1)
	assert.Equal(t, ports.SessionHealthDetail{
		SessionID:                "s1",
		Connected:                true,
		SubscriptionsWanted:      2,
		SubscriptionsActive:      2,
		SubscriptionsSatisfied:   &satisfied,
		ActiveTopics:             []string{"sensors/x", "sensors/y"},
		BestEffortTopics:         []string{"sensors/x"},
		Ready:                    true,
		ServiceLevel:             ports.ServiceLevelFull,
		UnsettledCount:           3,
		OldestUnsettledAge:       4 * time.Second,
		ReceiveWindowUtilization: 0.75,
		RecoveryRecycleCount:     5,
	}, dh.Sessions[0])
}

// newSessionSenderHealthRuntime is a running runtime with one session, s1,
// registered as an egress target with cfg, whose Health reports health. Its
// lease store never grants a lease, so an exclusive deferred-connect s1 is a
// standby.
func newSessionSenderHealthRuntime(cfg session.Config, health ports.SessionHealth) *Runtime {
	return &Runtime{
		// Never advanced, so the shared probe deadline cannot fire.
		clk:             clocktest.NewAt(time.Unix(0, 0)),
		componentErrors: make(map[string]error),
		running:         true,
		healthy:         true,
		leaseStore:      heldElsewhereLeaseStore{},
		sessionSenders: map[string]*sessionSenderEntry{
			"s1": {config: cfg, session: &fixedHealthSession{health: health}},
		},
		sessionMgrs: map[string]*session.Manager{},
	}
}

func unrecoverableSessionFault() error {
	return fmt.Errorf("%w: %w", session.ErrSessionUnrecoverable, shared.ErrTransportClosedPermanently)
}

// A session whose supervisor recorded an unrecoverable failure is not ready and
// serves nothing, whatever its own Health still reports, and the instance does
// not advertise itself ready for traffic until a rebuild clears the fault.
func TestDeepHealth_UnrecoverableSessionIsNotReady(t *testing.T) {
	healthy := ports.SessionHealth{Connected: true, Ready: true, ServiceLevel: ports.ServiceLevelFull}

	rt := newSessionSenderHealthRuntime(session.Config{SessionID: "s1"}, healthy)
	before := rt.DeepHealth(context.Background())
	require.Len(t, before.Sessions, 1)
	require.True(t, before.Sessions[0].Ready, "precondition: the session reports itself ready")
	require.True(t, before.ReadyForTraffic, "precondition: a ready session leaves the instance ready")

	rt.componentErrors["session:s1"] = unrecoverableSessionFault()
	dh := rt.DeepHealth(context.Background())

	require.Len(t, dh.Sessions, 1)
	assert.False(t, dh.Sessions[0].Ready)
	assert.Equal(t, ports.ServiceLevelNone, dh.Sessions[0].ServiceLevel)
	assert.True(t, dh.Sessions[0].Connected, "the rest of the session's own report is kept")
	assert.False(t, dh.ReadyForTraffic)
	assert.Equal(t, ports.ServiceLevelNone, dh.ServiceLevel)
}

// A deferred-connect standby is excused from the ready aggregate because it
// stays disconnected until it wins the lease. A failed exclusive session holds
// no lease either, so it looks like such a standby; the recorded unrecoverable
// fault must still keep the instance from advertising itself ready.
func TestDeepHealth_UnrecoverableDeferredStandbyIsNotExcused(t *testing.T) {
	standby := ports.SessionHealth{Ready: false, ServiceLevel: ports.ServiceLevelNone}
	cfg := session.Config{SessionID: "s1", Exclusive: true, ConnectAfterLease: true}

	rt := newSessionSenderHealthRuntime(cfg, standby)
	before := rt.DeepHealth(context.Background())
	require.Len(t, before.Sessions, 1)
	require.True(t, before.Sessions[0].ConnectAfterLease, "precondition: the session defers its connect")
	require.False(t, before.Sessions[0].HasLease, "precondition: the session holds no lease")
	require.True(t, before.ReadyForTraffic, "precondition: a deferred-connect standby is excused")
	require.Equal(t, ports.LevelFull, ports.ReadinessLevelFromDeepHealth(before),
		"precondition: an excused standby does not lower the instance's readiness level")

	rt.componentErrors["session:s1"] = unrecoverableSessionFault()
	dh := rt.DeepHealth(context.Background())

	require.Len(t, dh.Sessions, 1)
	assert.False(t, dh.Sessions[0].Ready)
	assert.Equal(t, ports.ServiceLevelNone, dh.Sessions[0].ServiceLevel)
	assert.False(t, dh.ReadyForTraffic, "a failed session must not be excused as a standby")
	assert.Less(t, ports.ReadinessLevelFromDeepHealth(dh), ports.LevelConnected,
		"the readiness levels must not excuse a failed session as a standby either")
}
