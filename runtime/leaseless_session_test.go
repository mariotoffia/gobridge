package runtime

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// switchableSession reports the connection state the test sets, so readiness
// can be read with the session up and with it down.
type switchableSession struct {
	*roleFakeSession
	down atomic.Bool
}

func (s *switchableSession) Health(context.Context) ports.SessionHealth {
	if s.down.Load() {
		return ports.SessionHealth{ServiceLevel: ports.ServiceLevelNone}
	}
	return ports.SessionHealth{Connected: true, Ready: true, ServiceLevel: ports.ServiceLevelFull}
}

// TestLeaselessRuntime_ExclusiveSessionIsNotLeaseManaged pins that a runtime
// with no lease store treats no session as holding or waiting for a lease. The
// builder registers a session a binding names as exclusive with a deferred
// connect, but without a lease store nothing ever grants that lease, so the
// session is fenced, classified and counted like a non-exclusive one: its DLQ
// writes go through, the instance is standalone rather than a standby that can
// never become active, and readiness follows the session's real connection
// state instead of skipping it as a standby's deferred connect.
func TestLeaselessRuntime_ExclusiveSessionIsNotLeaseManaged(t *testing.T) {
	rt := New(WithInstanceID("leaseless-exclusive"))
	sess := &switchableSession{roleFakeSession: &roleFakeSession{events: make(chan ports.SessionEvent, 1)}}
	cfg := session.Config{SessionID: "s1", Exclusive: true, ConnectAfterLease: true}
	require.NoError(t, rt.RegisterSessionSender(cfg, sess, nopRouteSender{}))
	startComponentRuntime(t, rt)
	ctx := context.Background()

	tok, held := rt.dlqToken("s1")
	assert.True(t, held, "a DLQ write for a session no lease can govern must be allowed")
	assert.Equal(t, persistence.LeaseToken{}, tok)

	assert.Equal(t, ports.RoleStandalone, rt.Role(),
		"without a lease store no session takes part in failover")
	assert.Equal(t, ports.LevelFull, rt.ReadinessLevel(ctx),
		"a connected session reaches full readiness")

	sess.down.Store(true)
	assert.Less(t, rt.ReadinessLevel(ctx), ports.LevelConnected,
		"a disconnected session must lower readiness: it never defers its connect")
}

// TestDeepHealth_NonExclusiveSessionNeverDefersItsConnect pins that readiness
// skips a session as a deferred-connect standby only when its manager really
// defers: an exclusive session in a runtime with a lease store. A non-exclusive
// session starts at once even with ConnectAfterLease set and a lease store
// present, so its connection state counts.
func TestDeepHealth_NonExclusiveSessionNeverDefersItsConnect(t *testing.T) {
	rt := New(WithInstanceID("non-exclusive-connect"), WithLeaseStore(&roleGrantingLeaseStore{}))
	sess := &switchableSession{roleFakeSession: &roleFakeSession{events: make(chan ports.SessionEvent, 1)}}
	cfg := session.Config{SessionID: "s1", ConnectAfterLease: true}
	require.NoError(t, rt.RegisterSessionSender(cfg, sess, nopRouteSender{}))
	startComponentRuntime(t, rt)
	ctx := context.Background()

	require.Equal(t, ports.LevelFull, rt.ReadinessLevel(ctx), "a connected session reaches full readiness")
	sess.down.Store(true)
	dh := rt.DeepHealth(ctx)
	require.Len(t, dh.Sessions, 1)
	assert.False(t, dh.Sessions[0].ConnectAfterLease, "a non-exclusive session never defers its connect")
	assert.Less(t, ports.ReadinessLevelFromDeepHealth(dh), ports.LevelConnected,
		"a disconnected non-exclusive session must lower readiness")
}
