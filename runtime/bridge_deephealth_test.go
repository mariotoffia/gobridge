package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
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
