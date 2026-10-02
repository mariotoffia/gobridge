package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// heldElsewhereLeaseStore never grants a lease: another instance holds it, so
// an exclusive manager stays a standby waiting for it.
type heldElsewhereLeaseStore struct{}

func (heldElsewhereLeaseStore) Acquire(context.Context, string, string, time.Duration, map[string]string) (persistence.LeaseToken, error) {
	return persistence.LeaseToken{}, shared.ErrAlreadyExists
}
func (heldElsewhereLeaseStore) Renew(context.Context, string, persistence.LeaseToken, time.Duration, map[string]string) (persistence.LeaseToken, error) {
	return persistence.LeaseToken{}, shared.ErrAlreadyExists
}
func (heldElsewhereLeaseStore) Release(context.Context, string, persistence.LeaseToken) error {
	return nil
}
func (heldElsewhereLeaseStore) Current(_ context.Context, leaseID string) (persistence.LeaseInfo, error) {
	return persistence.LeaseInfo{LeaseID: leaseID, Owner: "another-instance", Version: 1}, nil
}

func sessionDetail(t *testing.T, dh ports.DeepHealth, sid string) ports.SessionHealthDetail {
	t.Helper()
	for _, sh := range dh.Sessions {
		if sh.SessionID == sid {
			return sh
		}
	}
	t.Fatalf("deep health reports no session %q", sid)
	return ports.SessionHealthDetail{}
}

// A session has one manager, built from the first registration wiring reaches.
// A direct_hold route's non-exclusive session block registered before an
// exclusive session sender of the same session builds a manager that never holds
// a lease, so DLQ writes filed under that session are not fenced on one, the
// instance is standalone, and readiness counts the session's connection state.
func TestSessionManagerSource_EarlierNonExclusiveRouteDecidesFencingRoleAndReadiness(t *testing.T) {
	rt := New(WithInstanceID("manager-source-route-first"), WithLeaseStore(&roleGrantingLeaseStore{}))
	sess := &switchableSession{roleFakeSession: &roleFakeSession{events: make(chan ports.SessionEvent, 1)}}
	require.NoError(t, rt.AddRoute(componentRoute("receive-route"), newComponentReceiver(), &componentSender{}, sess,
		&session.Config{SessionID: "s1"}))
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1", Exclusive: true, ConnectAfterLease: true},
		sess, nopRouteSender{}))
	startComponentRuntime(t, rt)
	ctx := context.Background()

	tok, held := rt.dlqToken("s1")
	assert.True(t, held, "a DLQ write for a session whose manager holds no lease must not be fenced on one")
	assert.Equal(t, persistence.LeaseToken{}, tok)
	assert.Equal(t, ports.RoleStandalone, rt.Role())
	sess.down.Store(true)
	assert.Less(t, rt.ReadinessLevel(ctx), ports.LevelConnected,
		"the session's manager never defers its connect, so its connection state counts")
}

// The reverse order: a shared_outbox binding's exclusive session sender builds
// the session's manager before a later direct_hold route's non-exclusive session
// block of the same session. That manager defers its connect until it holds the
// lease, so deep health reports the session as deferred, before Start and after,
// and readiness skips it while it waits for the lease.
func TestSessionManagerSource_EarlierBindingSenderDecidesDeferredConnect(t *testing.T) {
	rt := New(WithInstanceID("manager-source-sender-first"),
		WithLeaseStore(heldElsewhereLeaseStore{}), WithOutboxStore(&graftOutboxStore{}))
	sess := &switchableSession{roleFakeSession: &roleFakeSession{events: make(chan ports.SessionEvent, 1)}}
	outbox := componentRoute("outbox-route")
	outbox.Policy.DeliveryMode = routing.DeliverySharedOutbox
	outbox.Bindings = []routing.DestinationBinding{{ID: "b1", Address: "out/addr", SessionID: "s1"}}
	require.NoError(t, rt.AddRoute(outbox, newComponentReceiver(), nopRouteSender{}, sess, nil))
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1", Exclusive: true, ConnectAfterLease: true},
		sess, nopRouteSender{}))
	require.NoError(t, rt.AddRoute(componentRoute("receive-route"), newComponentReceiver(), &componentSender{}, sess,
		&session.Config{SessionID: "s1"}))
	ctx := context.Background()

	assert.True(t, sessionDetail(t, rt.DeepHealth(ctx), "s1").ConnectAfterLease,
		"before Start the snapshot resolves the manager wiring will build")
	sess.down.Store(true)
	startComponentRuntime(t, rt)

	sh := sessionDetail(t, rt.DeepHealth(ctx), "s1")
	assert.True(t, sh.ConnectAfterLease, "the session's manager defers its connect until it holds the lease")
	assert.False(t, sh.HasLease)
	wait.Until(t, 2*time.Second, "a standby waiting for its lease reaches its subscribed cap", func() bool {
		return rt.ReadinessLevel(ctx) == ports.LevelSubscribed
	})
}
