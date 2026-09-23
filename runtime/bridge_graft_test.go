package runtime

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// graftLeaseStore grants every lease to its caller. It is safe for the
// concurrent session managers of a runtime and the part grafted onto it.
type graftLeaseStore struct {
	mu      sync.Mutex
	version uint64
}

func (s *graftLeaseStore) Acquire(_ context.Context, _, ownerID string, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	return persistence.LeaseToken{Version: s.version, Owner: ownerID}, nil
}

func (*graftLeaseStore) Renew(_ context.Context, _ string, token persistence.LeaseToken, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	return token, nil
}

func (*graftLeaseStore) Release(context.Context, string, persistence.LeaseToken) error { return nil }

func (*graftLeaseStore) Current(_ context.Context, leaseID string) (persistence.LeaseInfo, error) {
	return persistence.LeaseInfo{LeaseID: leaseID}, nil
}

// graftOutboxStore holds no records and counts how often it is closed.
type graftOutboxStore struct{ closes atomic.Int32 }

func (*graftOutboxStore) Persist(context.Context, []*persistence.OutboxRecord) error { return nil }

func (*graftOutboxStore) Claim(context.Context, string, persistence.LeaseToken, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

func (*graftOutboxStore) Complete(context.Context, []string, persistence.LeaseToken) error {
	return nil
}

func (*graftOutboxStore) Expire(context.Context, time.Time, string, persistence.LeaseToken) (int, error) {
	return 0, nil
}

func (*graftOutboxStore) QueryPending(context.Context, string, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

func (s *graftOutboxStore) Close() error { s.closes.Add(1); return nil }

// graftDLQStore counts the entries written to it.
type graftDLQStore struct{ writes atomic.Int32 }

func (s *graftDLQStore) Write(context.Context, routing.DLQEntry) error { s.writes.Add(1); return nil }

func (*graftDLQStore) Get(context.Context, string) (routing.DLQEntry, error) {
	return routing.DLQEntry{}, nil
}

func (*graftDLQStore) List(context.Context, routing.DLQFilter) ([]routing.DLQEntry, error) {
	return nil, nil
}

func (*graftDLQStore) Delete(context.Context, []string) (int, error)                  { return 0, nil }
func (*graftDLQStore) DeleteByFilter(context.Context, routing.DLQFilter) (int, error) { return 0, nil }
func (*graftDLQStore) Purge(context.Context, time.Time) (int, error)                  { return 0, nil }

// graftSession is a no-op session that counts how often it is closed.
type graftSession struct {
	roleFakeSession
	closes atomic.Int32
}

func newGraftSession() *graftSession {
	return &graftSession{roleFakeSession: roleFakeSession{events: make(chan ports.SessionEvent, 1)}}
}

func (s *graftSession) Close(context.Context) error { s.closes.Add(1); return nil }

// graftStores is one set of store instances a runtime and its parts share.
type graftStores struct {
	lease  *graftLeaseStore
	outbox *graftOutboxStore
	dlq    *graftDLQStore
}

func newGraftStores() graftStores {
	return graftStores{lease: &graftLeaseStore{}, outbox: &graftOutboxStore{}, dlq: &graftDLQStore{}}
}

// options builds a runtime over the stores; a part adds WithSharedStores.
func (s graftStores) options(extra ...Option) []Option {
	return append([]Option{WithLeaseStore(s.lease), WithOutboxStore(s.outbox), WithDLQStore(s.dlq)}, extra...)
}

// newGraftHost starts a runtime over stores with a direct_hold route per id.
func newGraftHost(t *testing.T, stores graftStores, routeIDs ...string) *Runtime {
	t.Helper()
	host := New(stores.options(WithInstanceID("graft-host"))...)
	for _, id := range routeIDs {
		require.NoError(t, host.AddRoute(componentRoute(id), newComponentReceiver(), &componentSender{}, nil, nil))
	}
	startComponentRuntime(t, host)
	return host
}

// outboxRoute is a shared_outbox route whose one binding drains through the
// route's own session.
func outboxRoute(id string) RouteConfig {
	return RouteConfig{
		ID:       id,
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
		Bindings: []routing.DestinationBinding{{ID: id + "-binding", Address: "devices/" + id}},
	}
}

func routeIDs(rt *Runtime) []string {
	var ids []string
	for _, info := range rt.Routes() {
		ids = append(ids, info.ID)
	}
	return ids
}

func stopRuntime(t *testing.T, rt *Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, rt.Stop(ctx))
}

// TestGraft_StartsPartRoutesOnRunningRuntime pins that a grafted part's route
// delivers through the running runtime and is listed beside the routes the
// runtime already had.
func TestGraft_StartsPartRoutesOnRunningRuntime(t *testing.T) {
	stores := newGraftStores()
	host := newGraftHost(t, stores, "r1")
	part := New(stores.options(WithSharedStores())...)
	recv, sender := newComponentReceiver(), &componentSender{}
	require.NoError(t, part.AddRoute(componentRoute("r2"), recv, sender, nil, nil))

	require.NoError(t, host.Graft(part))

	recv.deliver(t, "through-the-grafted-route")
	wait.Until(t, 2*time.Second, "the grafted route delivers", func() bool { return sender.sent.Load() == 1 })
	assert.ElementsMatch(t, []string{"r1", "r2"}, routeIDs(host))
}

// TestGraft_RefusesDuplicateRouteOrSessionID pins that a part may not reuse a
// route id or a session id the runtime already has, and that a refused part is
// left as it was, still owning its session, for its caller to stop.
func TestGraft_RefusesDuplicateRouteOrSessionID(t *testing.T) {
	cases := []struct {
		name  string
		want  string
		build func(t *testing.T, part *Runtime, sess ports.Session)
	}{
		{"route id", `route "r1" is already registered`, func(t *testing.T, part *Runtime, sess ports.Session) {
			require.NoError(t, part.AddRoute(componentRoute("r1"), newComponentReceiver(), &componentSender{}, sess, nil))
		}},
		{"session sender id", `session "s1" is already registered`, func(t *testing.T, part *Runtime, sess ports.Session) {
			require.NoError(t, part.RegisterSessionSender(session.Config{SessionID: "s1"}, sess, nopRouteSender{}))
		}},
		{"route session naming a session sender", `session "s1" is already registered`, func(t *testing.T, part *Runtime, sess ports.Session) {
			sessCfg := session.Config{SessionID: "s1"}
			require.NoError(t, part.AddRoute(componentRoute("r2"), newComponentReceiver(), &componentSender{}, sess, &sessCfg))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stores := newGraftStores()
			host := New(stores.options()...)
			require.NoError(t, host.AddRoute(componentRoute("r1"), newComponentReceiver(), &componentSender{}, nil, nil))
			require.NoError(t, host.RegisterSessionSender(session.Config{SessionID: "s1"}, newGraftSession(), nopRouteSender{}))
			startComponentRuntime(t, host)
			part := New(stores.options(WithSharedStores())...)
			sess := newGraftSession()
			tc.build(t, part, sess)
			before := routeIDs(part)

			require.ErrorContains(t, host.Graft(part), tc.want)

			assert.Equal(t, before, routeIDs(part), "a refused part keeps its routes")
			assert.Equal(t, []string{"r1"}, routeIDs(host))
			stopRuntime(t, part)
			assert.Equal(t, int32(1), sess.closes.Load(), "a refused part still owns its session, so its Stop closes it")
		})
	}
}

// TestGraft_RefusesWhenHostNotRunning pins that only a running runtime takes a
// part: one never started, stopped or fenced has no live work context to run
// the part's components under.
func TestGraft_RefusesWhenHostNotRunning(t *testing.T) {
	cases := map[string]func(t *testing.T, host *Runtime){
		"never started": func(*testing.T, *Runtime) {},
		"stopped": func(t *testing.T, host *Runtime) {
			require.NoError(t, host.Start(context.Background()))
			stopRuntime(t, host)
		},
		"fenced": func(t *testing.T, host *Runtime) {
			startComponentRuntime(t, host)
			host.Fence()
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			stores := newGraftStores()
			host := New(stores.options()...)
			prepare(t, host)
			part := New(stores.options(WithSharedStores())...)
			require.NoError(t, part.AddRoute(componentRoute("r2"), newComponentReceiver(), &componentSender{}, nil, nil))

			require.ErrorContains(t, host.Graft(part), "runtime is not running")

			assert.Equal(t, []string{"r2"}, routeIDs(part), "a refused part keeps its routes")
			assert.Empty(t, routeIDs(host))
		})
	}
}

// TestGraft_RefusesPartOverDifferentStores pins that a part must be built over
// the runtime's own store instances and say so with WithSharedStores: grafted
// components run against the runtime's stores, so a part built over other
// instances, or one whose Stop would close the runtime's stores, is refused.
func TestGraft_RefusesPartOverDifferentStores(t *testing.T) {
	stores := newGraftStores()
	host := newGraftHost(t, stores)
	other := stores
	other.lease = &graftLeaseStore{}
	parts := map[string]*Runtime{
		"different lease store":    New(other.options(WithSharedStores())...),
		"without WithSharedStores": New(stores.options()...),
	}
	for name, part := range parts {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, part.AddRoute(componentRoute("r2"), newComponentReceiver(), &componentSender{}, nil, nil))

			require.ErrorContains(t, host.Graft(part), "must be built over this runtime's stores with WithSharedStores")

			assert.Equal(t, []string{"r2"}, routeIDs(part), "a refused part keeps its routes")
		})
	}
	assert.Empty(t, routeIDs(host))
}

// TestGraft_RefusesPartNotClosedOverItsSessions pins that a part must bring
// every session its routes ride on or bind to, and may not bring one a route of
// the runtime uses: the wiring pass gives no drainer or settlement barrier
// across that line.
func TestGraft_RefusesPartNotClosedOverItsSessions(t *testing.T) {
	cases := []struct {
		name, want string
		hostRoute  RouteConfig
		build      func(t *testing.T, part *Runtime)
	}{
		{"part route rides on a runtime session", `part route "r2" uses session "s1" of the runtime`, componentRoute("r1"),
			func(t *testing.T, part *Runtime) {
				require.NoError(t, part.AddRoute(ridingRoute("r2", "s1"), newComponentReceiver(), &componentSender{}, nil, nil))
			}},
		{"part route binds to a runtime session", `part route "r2" uses session "s1" of the runtime`, componentRoute("r1"),
			func(t *testing.T, part *Runtime) {
				cfg := componentRoute("r2")
				cfg.Bindings = []routing.DestinationBinding{{ID: "b2", Address: "devices/r2", SessionID: "s1"}}
				require.NoError(t, part.AddRoute(cfg, newComponentReceiver(), &componentSender{}, nil, nil))
			}},
		{"runtime route rides on a part session", `route "r1" uses session "p1" the part brings`, ridingRoute("r1", "p1"),
			func(t *testing.T, part *Runtime) {
				require.NoError(t, part.RegisterSessionSender(session.Config{SessionID: "p1"}, newGraftSession(), nopRouteSender{}))
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stores := newGraftStores()
			host := New(stores.options()...)
			require.NoError(t, host.AddRoute(tc.hostRoute, newComponentReceiver(), &componentSender{}, nil, nil))
			require.NoError(t, host.RegisterSessionSender(session.Config{SessionID: "s1"}, newGraftSession(), nopRouteSender{}))
			startComponentRuntime(t, host)
			part := New(stores.options(WithSharedStores())...)
			tc.build(t, part)

			require.ErrorContains(t, host.Graft(part), tc.want)

			assert.Equal(t, []string{"r1"}, routeIDs(host))
		})
	}
}

// TestGraft_RefusesPartStartWouldRefuse pins that a graft runs the route checks
// Start runs, so a part whose route a Start would refuse is never started.
func TestGraft_RefusesPartStartWouldRefuse(t *testing.T) {
	stores := newGraftStores()
	host := newGraftHost(t, stores)
	part := New(stores.options(WithSharedStores())...)
	// A shared_outbox route over a session that holds no lease never drains.
	sessCfg := session.Config{SessionID: "part-session"}
	require.NoError(t, part.AddRoute(outboxRoute("r2"), newComponentReceiver(), &componentSender{}, newGraftSession(), &sessCfg))

	var invalid *ValidationError
	require.ErrorAs(t, host.Graft(part), &invalid)

	assert.Equal(t, []string{"r2"}, routeIDs(part), "a refused part keeps its routes")
	assert.Empty(t, routeIDs(host))
}

// TestGraft_ConsumesPart pins that a grafted part holds nothing afterwards: it
// cannot start, and its Stop closes nothing, because the session it was built
// with now belongs to the runtime it joined.
func TestGraft_ConsumesPart(t *testing.T) {
	stores := newGraftStores()
	host := newGraftHost(t, stores)
	part := New(stores.options(WithSharedStores())...)
	sess := newGraftSession()
	sessCfg := session.Config{SessionID: "s2"}
	require.NoError(t, part.AddRoute(componentRoute("r2"), newComponentReceiver(), &componentSender{}, sess, &sessCfg))
	require.NoError(t, host.Graft(part))

	require.ErrorContains(t, part.Start(context.Background()), "grafted onto another runtime")
	stopRuntime(t, part)

	assert.Zero(t, sess.closes.Load(), "the grafted session belongs to the runtime the part joined")
	assert.Empty(t, routeIDs(part))
	assert.Equal(t, []string{"r2"}, routeIDs(host))
}

// TestStop_SharedStoresAreLeftOpen pins that a runtime built WithSharedStores
// leaves its stores open on Stop, because they belong to the runtime it
// borrowed them from, while a runtime that owns its stores still closes them.
func TestStop_SharedStoresAreLeftOpen(t *testing.T) {
	cases := []struct {
		name   string
		shared bool
		closes int32
	}{
		{"shared", true, 0},
		{"owned", false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outbox := &graftOutboxStore{}
			opts := []Option{WithOutboxStore(outbox)}
			if tc.shared {
				opts = append(opts, WithSharedStores())
			}
			rt := New(opts...)
			require.NoError(t, rt.Start(context.Background()))

			stopRuntime(t, rt)

			assert.Equal(t, tc.closes, outbox.closes.Load())
		})
	}
}

// TestStop_StopsGraftedComponents pins that Stop tears a grafted part down with
// the runtime's own components: its route runner ends, its session is closed
// once, and the credential hooks of both are closed once each.
func TestStop_StopsGraftedComponents(t *testing.T) {
	stores := newGraftStores()
	host := New(stores.options()...)
	var hostHook, partHook atomic.Int32
	host.AttachCredentialCloser(func(context.Context) { hostHook.Add(1) })
	startComponentRuntime(t, host)

	part := New(stores.options(WithSharedStores())...)
	sess := newGraftSession()
	require.NoError(t, part.RegisterSessionSender(session.Config{SessionID: "s2"}, sess, nopRouteSender{}))
	require.NoError(t, part.AddRoute(componentRoute("r2"), newComponentReceiver(), &componentSender{}, nil, nil))
	part.AttachCredentialCloser(func(context.Context) { partHook.Add(1) })
	require.NoError(t, host.Graft(part))
	grafted := routeRunAt(host, 0)

	stopRuntime(t, host)

	wait.Until(t, 2*time.Second, "the grafted route runner ends", func() bool { return isClosed(grafted.done) })
	assert.Equal(t, int32(1), sess.closes.Load())
	assert.Equal(t, int32(1), hostHook.Load())
	assert.Equal(t, int32(1), partHook.Load())
}

// TestStart_InstallsDLQTokenFnWithoutSessionManagers pins that Start installs
// the DLQ router's token function even when it builds no session manager:
// sessions grafted on later write through the same router, so a write for an
// exclusive session this instance does not manage must still be refused.
func TestStart_InstallsDLQTokenFnWithoutSessionManagers(t *testing.T) {
	dlqStore := &graftDLQStore{}
	rt := New(WithInstanceID("dlq-token-no-managers"), WithDLQStore(dlqStore))
	startComponentRuntime(t, rt)
	rt.mu.Lock()
	managers := len(rt.sessionMgrs)
	rt.exclusiveSessions["owned-elsewhere"] = true
	router := rt.dlqRouter
	rt.mu.Unlock()
	require.Zero(t, managers)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "dead", Subject: "dlq-token"})
	cause := errors.New("send failed")
	err := router.Route(context.Background(), env, "route", "", "", "owned-elsewhere", "", cause, 1)
	require.ErrorIs(t, err, shared.ErrUnavailable, "a write for an exclusive session managed elsewhere is refused")
	assert.Zero(t, dlqStore.writes.Load())

	require.NoError(t, router.Route(context.Background(), env, "route", "", "", "", "", cause, 1))
	assert.Equal(t, int32(1), dlqStore.writes.Load(), "a write with no owning session goes through")
}

// TestGraft_WiresOnlyThePartsSessions pins that the wiring pass a graft runs
// builds and starts managers and drainers for the part's sessions alone. The
// runtime's own runs are left as they are, and a manager of the runtime that
// has no run is not started by a pass that did not build it.
func TestGraft_WiresOnlyThePartsSessions(t *testing.T) {
	stores := newGraftStores()
	host := New(stores.options()...)
	hostCfg := session.DefaultConfig("host-session", true)
	require.NoError(t, host.AddRoute(outboxRoute("host-route"), newComponentReceiver(), &componentSender{}, newGraftSession(), &hostCfg))
	require.NoError(t, host.RegisterIngressSession(session.Config{SessionID: "host-ingress"}, newGraftSession()))
	startComponentRuntime(t, host)

	host.mu.Lock()
	hostRun := host.sessionRuns["host-session"]
	drainersBefore := slices.Clone(host.drainers)
	// A path that stops one manager alone leaves the manager without a run.
	delete(host.sessionRuns, "host-ingress")
	host.mu.Unlock()
	require.Len(t, drainersBefore, 1)
	hostDrainerDone := drainersBefore[0].run.done

	part := New(stores.options(WithSharedStores())...)
	partCfg := session.DefaultConfig("part-session", true)
	require.NoError(t, part.AddRoute(outboxRoute("part-route"), newComponentReceiver(), &componentSender{}, newGraftSession(), &partCfg))
	require.NoError(t, host.Graft(part))

	host.mu.Lock()
	managers := len(host.sessionMgrs)
	runs := maps.Clone(host.sessionRuns)
	drainersAfter := slices.Clone(host.drainers)
	host.mu.Unlock()

	assert.Equal(t, 3, managers)
	assert.Equal(t, hostRun.done, runs["host-session"].done, "the runtime's own manager keeps its run")
	assert.False(t, isClosed(hostRun.done))
	assert.NotContains(t, runs, "host-ingress", "a graft starts only the managers it built")
	assert.Contains(t, runs, "part-session")
	require.Len(t, drainersAfter, 2)
	assert.Same(t, drainersBefore[0], drainersAfter[0])
	assert.Equal(t, hostDrainerDone, drainersAfter[0].run.done, "the runtime's own drainer keeps its run")
	assert.False(t, isClosed(hostDrainerDone))
	assert.Equal(t, "part-session", drainersAfter[1].sessionID)
	assert.NotNil(t, drainersAfter[1].run.done, "the part's drainer is started")
}

// TestAttachCredentialForget_PairsWithTheLastCloser pins how the two halves of
// one credential refresher meet in one hook: the builder attaches a
// refresher's closer and then its forget, so a forget joins the last hook while
// that hook has none, and otherwise starts a hook of its own.
func TestAttachCredentialForget_PairsWithTheLastCloser(t *testing.T) {
	rt := New()
	rt.AttachCredentialCloser(func(context.Context) {})
	rt.AttachCredentialForget(func([]any) bool { return true })
	rt.AttachCredentialForget(func([]any) bool { return false })
	rt.AttachCredentialForget(nil)

	rt.mu.Lock()
	hooks := slices.Clone(rt.credHooks)
	rt.mu.Unlock()

	require.Len(t, hooks, 2)
	assert.NotNil(t, hooks[0].close)
	assert.True(t, hooks[0].forget(nil), "the first forget pairs with the closer attached before it")
	assert.Nil(t, hooks[1].close)
	assert.False(t, hooks[1].forget(nil), "a second forget starts a hook of its own")
}
