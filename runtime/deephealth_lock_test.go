package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// blockingHealthSession is a ports.Session whose Health blocks until released,
// modelling a wedged broker client. Used to prove DeepHealth does not hold rt.mu
// across the (potentially blocking) plugin Health call (finding L10).
type blockingHealthSession struct {
	release chan struct{}
	entered chan struct{}
}

func (s *blockingHealthSession) Start(context.Context) error { return nil }
func (s *blockingHealthSession) Reconcile(context.Context, connectivity.SessionPlan) error {
	return nil
}
func (s *blockingHealthSession) Health(context.Context) ports.SessionHealth {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release
	return ports.SessionHealth{Connected: true, Ready: true, ServiceLevel: ports.ServiceLevelFull}
}
func (s *blockingHealthSession) Events() <-chan ports.SessionEvent { return nil }
func (s *blockingHealthSession) Close(context.Context) error       { return nil }

// Finding L10: DeepHealth must snapshot under rt.mu and invoke the blocking
// plugin Session.Health OUTSIDE the lock, so a wedged broker client cannot stall
// every other rt.mu user (Role, /live, /ready, Stop) for the duration. This test
// blocks Health and proves Role() still returns promptly.
func TestDeepHealth_BlockingSessionHealthDoesNotHoldLock(t *testing.T) {
	sess := &blockingHealthSession{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	rt := &Runtime{
		clk:             clock.System,
		componentErrors: make(map[string]error),
		running:         true,
		healthy:         true,
		sessionSenders:  map[string]*sessionSenderEntry{},
		sessionMgrs:     map[string]*session.Manager{},
		entries: []*routeEntry{
			{
				config:  RouteConfig{ID: "r1"},
				session: sess,
				sessCfg: &session.Config{SessionID: "s1"},
			},
		},
	}

	dhDone := make(chan struct{})
	go func() {
		defer close(dhDone)
		_ = rt.DeepHealth(context.Background())
	}()

	// Wait until DeepHealth is blocked inside Health (lock already released).
	select {
	case <-sess.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("DeepHealth never entered Session.Health")
	}

	// Role() acquires rt.mu; it must NOT be blocked by the in-flight Health.
	roleDone := make(chan string, 1)
	go func() { roleDone <- rt.Role() }()
	select {
	case role := <-roleDone:
		assert.Equal(t, roleStandalone, role)
	case <-time.After(1 * time.Second):
		t.Fatal("Role() blocked while Session.Health was in flight — rt.mu held across a plugin call")
	}

	close(sess.release)
	select {
	case <-dhDone:
	case <-time.After(2 * time.Second):
		t.Fatal("DeepHealth did not complete after Health released")
	}
	require.True(t, true)
}
