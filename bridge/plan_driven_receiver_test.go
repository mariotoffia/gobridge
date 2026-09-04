package bridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A plan-driven transport (one advertising ports.CapPlanDrivenSubscriptions —
// MQTT, AMQP 0-9-1) subscribes only when a session manager reconciles the
// session plan, so a receiver bound to a session nothing manages is silently
// inert. The receiver's own binding to the session is what makes its
// subscriptions matter, so it is what manages the session when no route
// session block and no binding does: the builder registers the session as an
// ingress session with a plain manager — no lease, no outbox partition — which
// is exactly the shape a direct_hold route holds its source in.

// capTransportFactory is a fakeTransportFactory whose Capabilities() are
// configurable, letting a test advertise (or omit) ports.CapPlanDrivenSubscriptions
// without importing a real transport adapter (forbidden to the bridge package).
type capTransportFactory struct {
	fakeTransportFactory
	caps []ports.Capability
}

func (f *capTransportFactory) Capabilities() []ports.Capability { return f.caps }

// countingSession records every Start and Reconcile, so a test can tell one
// manager from two and read the plan the manager handed the session.
type countingSession struct {
	fakeSession
	mu         sync.Mutex
	starts     int
	plans      []connectivity.SessionPlan
	reconciled chan struct{}
}

func newCountingSession() *countingSession {
	return &countingSession{reconciled: make(chan struct{}, 8)}
}

func (s *countingSession) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts++
	return nil
}

func (s *countingSession) Reconcile(_ context.Context, plan connectivity.SessionPlan) error {
	s.mu.Lock()
	s.plans = append(s.plans, plan)
	s.mu.Unlock()
	select {
	case s.reconciled <- struct{}{}:
	default:
	}
	return nil
}

func (s *countingSession) startCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

func (s *countingSession) reconciledPlans() []connectivity.SessionPlan {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]connectivity.SessionPlan(nil), s.plans...)
}

// ingressTransportFactory hands out one shared countingSession for every
// session it is asked for, and advertises the given capabilities.
type ingressTransportFactory struct {
	capTransportFactory
	session *countingSession
}

func (f *ingressTransportFactory) NewSession(context.Context, ports.SessionSpec) (ports.Session, error) {
	return f.session, nil
}

var planDrivenCaps = []ports.Capability{
	ports.CapStatefulSession, ports.CapPlanDrivenSubscriptions, ports.CapSourceRedelivery,
}

// ingressOnlyConfig is a direct_hold route whose receiver subscribes on the
// session "s-src" and whose destination is a session-less sink: no route
// session block and no binding names s-src, so only the receiver does.
func ingressOnlyConfig(sessionMode string) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b-ingress"},
		// Stores are present so a test can prove the session takes no lease,
		// rather than that none could be taken.
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "memory"},
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
		Sessions: []ports.SessionDef{
			{ID: "s-src", Transport: "planned", SessionMode: sessionMode},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "rx", Transport: "planned", SessionID: "s-src", Topics: []ports.SubscriptionDef{
				{Topic: "topic/in", QoS: 1},
			}},
		},
		Senders: []ports.SenderDef{
			{ID: "tx", Transport: "sink"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx", Address: "out/addr"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "rx",
				DeliveryMode: "direct_hold",
				Bindings:     []string{"b1"},
				Policy:       ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop", AllowUnfenced: true},
			},
		},
	}
}

func buildAndStart(t *testing.T, cfg *ports.BridgeConfig, caps []ports.Capability) (*countingSession, interface {
	Role() string
	LeaseStatus() map[string]bool
	DeepHealth(context.Context) ports.DeepHealth
}) {
	t.Helper()
	sess := newCountingSession()
	tf := &ingressTransportFactory{capTransportFactory: capTransportFactory{caps: caps}, session: sess}
	rt, err := NewBuilder(cfg).
		RegisterTransportFactory("planned", tf).
		RegisterTransportFactory("sink", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())
	require.NoError(t, err)
	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = rt.Stop(stopCtx)
	})
	return sess, rt
}

func TestBuilder_ReceiverBindingManagesPlanDrivenIngressSession(t *testing.T) {
	sess, rt := buildAndStart(t, ingressOnlyConfig("persistent"), planDrivenCaps)

	select {
	case <-sess.reconciled:
	case <-time.After(2 * time.Second):
		t.Fatal("the ingress session was never reconciled: nothing manages it")
	}
	plans := sess.reconciledPlans()
	require.Len(t, plans[0].Subscriptions, 1)
	assert.Equal(t, "topic/in", plans[0].Subscriptions[0].Topic,
		"the manager must reconcile the receiver's own subscriptions")
	assert.Equal(t, 1, sess.startCount(), "exactly one manager started the session")

	// No lease, no partition: the instance is standalone and the session holds
	// nothing.
	assert.Equal(t, ports.RoleStandalone, rt.Role())
	held, listed := rt.LeaseStatus()["s-src"]
	assert.True(t, listed, "the ingress session is managed")
	assert.False(t, held, "the ingress session holds no lease")
	dh := rt.DeepHealth(context.Background())
	require.Len(t, dh.Sessions, 1)
	assert.Equal(t, "s-src", dh.Sessions[0].SessionID)
	assert.False(t, dh.Sessions[0].HasLease)
	assert.False(t, dh.Sessions[0].ConnectAfterLease)
}

// An exclusive session is lease-held by declaration, and a receiver cannot
// hold a lease, so the refusal stays for that shape — and names the shapes
// that do work.
func TestBuilder_ExclusiveIngressSessionNamedOnlyByItsReceiverIsRefused(t *testing.T) {
	tf := &ingressTransportFactory{capTransportFactory: capTransportFactory{caps: planDrivenCaps}, session: newCountingSession()}
	_, err := NewBuilder(ingressOnlyConfig("exclusive")).
		RegisterTransportFactory("planned", tf).
		RegisterTransportFactory("sink", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), `receiver "rx"`)
	assert.Contains(t, err.Error(), `session "s-src"`)
	assert.Contains(t, err.Error(), "exclusive")
	assert.Contains(t, err.Error(), "session block", "the lease-bearing way out is named")
	assert.Contains(t, err.Error(), "declare the session persistent", "the lease-less way out is named")
}

// distributedStoreFactory is a fakeStoreFactory that declares cross-process
// coordination, which a clustered deployment requires of its lease store.
type distributedStoreFactory struct{ fakeStoreFactory }

func (*distributedStoreFactory) IsDistributed() bool { return true }

// In a clustered deployment a persistent session is not a way out: every
// replica would connect with the same client id. The refusal must not suggest
// it there.
func TestBuilder_ExclusiveIngressSessionRefusalOmitsPersistentWhenClustered(t *testing.T) {
	cfg := ingressOnlyConfig("exclusive")
	cfg.Bridge.DeploymentMode = "clustered"
	tf := &ingressTransportFactory{capTransportFactory: capTransportFactory{caps: planDrivenCaps}, session: newCountingSession()}
	_, err := NewBuilder(cfg).
		RegisterTransportFactory("planned", tf).
		RegisterTransportFactory("sink", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &distributedStoreFactory{}).
		Build(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "session block")
	assert.Contains(t, err.Error(), "same client id", "it says why persistent is not the way out here")
	assert.NotContains(t, err.Error(), "declare the session persistent")
}

// A session already managed through a route session block or a binding keeps
// that single manager: the receiver's binding does not add a second one.
func TestBuilder_IngressSessionAlreadyManagedKeepsOneManager(t *testing.T) {
	sharedOutbox := func(mutate func(*ports.BridgeConfig)) *ports.BridgeConfig {
		cfg := ingressOnlyConfig("exclusive")
		cfg.Senders = append(cfg.Senders, ports.SenderDef{ID: "tx-src", Transport: "planned", SessionID: "s-src"})
		cfg.Bindings = []ports.BindingDef{{ID: "b1", SenderID: "tx-src", SessionID: "s-src", Address: "out/addr"}}
		cfg.Routes[0].DeliveryMode = "shared_outbox"
		cfg.Routes[0].Policy = ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"}
		mutate(cfg)
		return cfg
	}
	cases := map[string]*ports.BridgeConfig{
		"route session block": sharedOutbox(func(cfg *ports.BridgeConfig) {
			cfg.Routes[0].Session = &ports.RouteSessionDef{SessionID: "s-src", SenderID: "tx-src"}
		}),
		"binding session": sharedOutbox(func(*ports.BridgeConfig) {}),
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			sess, rt := buildAndStart(t, cfg, planDrivenCaps)
			select {
			case <-sess.reconciled:
			case <-time.After(2 * time.Second):
				t.Fatal("the session was never reconciled")
			}
			assert.Equal(t, 1, sess.startCount(), "one manager, not one per path")
			// The one manager is the lease-bearing one the existing path wired.
			assert.Equal(t, ports.RoleActive, rt.Role())
		})
	}
}

// A self-establishing transport (amqp10, whose receivers attach links on start
// independently of the plan) does not advertise plan-driven subscriptions and
// gets no manager from its receiver — the false positive that keeps this a
// capability check rather than a blanket rule.
func TestBuilder_SelfEstablishingReceiverGetsNoManager(t *testing.T) {
	sess, rt := buildAndStart(t, ingressOnlyConfig("persistent"), []ports.Capability{
		ports.CapStatefulSession, ports.CapSourceRedelivery, ports.CapVisibilityExtension,
	})
	assert.Equal(t, 0, sess.startCount(), "a self-establishing session is not started by a manager")
	assert.Empty(t, rt.LeaseStatus(), "and is not managed at all")
}
