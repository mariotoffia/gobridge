package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestGraft_RefusesASessionObjectSharedAcrossTheBoundary pins that a route
// rides on a session by object as well as by id: a hand-wired route added with
// a session and no session block of its own rides on the ingress session or
// session sender registered with that object, even when its configuration names
// no session. A part route riding on a session object of the runtime, or a
// runtime route riding on one the part brings, would be wired without that
// session's manager or settlement barrier, so the graft refuses it although no
// id is shared.
func TestGraft_RefusesASessionObjectSharedAcrossTheBoundary(t *testing.T) {
	hostIngress, hostSender, partIngress := newGraftSession(), newGraftSession(), newGraftSession()
	cases := []struct {
		name  string
		want  string
		host  func(t *testing.T, host *Runtime)
		build func(t *testing.T, part *Runtime)
	}{
		{"part route rides on the runtime's ingress session object", `part route "r2" uses a session object of the runtime`,
			func(t *testing.T, host *Runtime) {
				require.NoError(t, host.RegisterIngressSession(session.Config{SessionID: "host-ingress"}, hostIngress))
			},
			func(t *testing.T, part *Runtime) {
				require.NoError(t, part.AddRoute(componentRoute("r2"), newComponentReceiver(), &componentSender{}, hostIngress, nil))
			}},
		{"part route rides on the runtime's session sender object", `part route "r2" uses a session object of the runtime`,
			func(t *testing.T, host *Runtime) {
				require.NoError(t, host.RegisterSessionSender(session.Config{SessionID: "host-sender"}, hostSender, nopRouteSender{}))
			},
			func(t *testing.T, part *Runtime) {
				require.NoError(t, part.AddRoute(componentRoute("r2"), newComponentReceiver(), &componentSender{}, hostSender, nil))
			}},
		{"runtime route rides on a session object the part brings", `route "r1" uses a session object the part brings`,
			func(t *testing.T, host *Runtime) {
				require.NoError(t, host.AddRoute(componentRoute("r1"), newComponentReceiver(), &componentSender{}, partIngress, nil))
			},
			func(t *testing.T, part *Runtime) {
				require.NoError(t, part.RegisterIngressSession(session.Config{SessionID: "part-ingress"}, partIngress))
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stores := newGraftStores()
			host := New(stores.options()...)
			tc.host(t, host)
			startComponentRuntime(t, host)
			before := routeIDs(host)
			part := New(stores.options(WithSharedStores())...)
			tc.build(t, part)

			require.ErrorContains(t, host.Graft(part), tc.want)

			assert.Equal(t, before, routeIDs(host), "a refused graft leaves the runtime as it was")
		})
	}
}

// TestGraft_PartRidingOnItsOwnSessionObjectGrafts pins the other side of the
// rule: a part route that rides by object on a session the part itself brings
// is closed over its sessions and grafts beside a runtime doing the same.
func TestGraft_PartRidingOnItsOwnSessionObjectGrafts(t *testing.T) {
	stores := newGraftStores()
	host := New(stores.options()...)
	hostIngress := newGraftSession()
	require.NoError(t, host.RegisterIngressSession(session.Config{SessionID: "host-ingress"}, hostIngress))
	require.NoError(t, host.AddRoute(componentRoute("r1"), newComponentReceiver(), &componentSender{}, hostIngress, nil))
	startComponentRuntime(t, host)

	part := New(stores.options(WithSharedStores())...)
	partIngress := newGraftSession()
	recv, sender := newComponentReceiver(), &componentSender{}
	require.NoError(t, part.RegisterIngressSession(session.Config{SessionID: "part-ingress"}, partIngress))
	require.NoError(t, part.AddRoute(componentRoute("r2"), recv, sender, partIngress, nil))

	require.NoError(t, host.Graft(part))

	recv.deliver(t, "through-the-grafted-route")
	wait.Until(t, 2*time.Second, "the grafted route delivers", func() bool { return sender.sent.Load() == 1 })
	assert.ElementsMatch(t, []string{"r1", "r2"}, routeIDs(host))
}
