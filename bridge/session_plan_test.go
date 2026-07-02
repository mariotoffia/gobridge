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

// ---------------------------------------------------------------------------
// White-box unit tests for sessionPlanFor (the helper that fixes F1: the
// bridge never assembled a connectivity.SessionPlan, so every broker session
// reconciled an empty plan and subscribed to nothing).
// ---------------------------------------------------------------------------

// TestSessionPlanFor_UnionAcrossReceiversOnSharedSession proves the plan is
// the per-session UNION of every receiver bound to the session — the
// SHARED-SESSION case that must be deterministic so the runtime's first-wins
// session-manager dedup stays correct. Receivers on a different session, and
// receivers with no topics, contribute nothing.
func TestSessionPlanFor_UnionAcrossReceiversOnSharedSession(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Sessions: []ports.SessionDef{
			{ID: "s1", Transport: "mqtt", SessionMode: "exclusive"},
			{ID: "s2", Transport: "mqtt"},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "rx1", Transport: "mqtt", SessionID: "s1", Topics: []ports.SubscriptionDef{
				{Topic: "topic/a", QoS: 1},
				{Topic: "topic/c", QoS: 0},
			}},
			{ID: "rx2", Transport: "mqtt", SessionID: "s1", Topics: []ports.SubscriptionDef{
				{Topic: "topic/b", QoS: 2},
			}},
			{ID: "rx-no-topics", Transport: "mqtt", SessionID: "s1"},
			{ID: "rx-other", Transport: "mqtt", SessionID: "s2", Topics: []ports.SubscriptionDef{
				{Topic: "topic/z", QoS: 1},
			}},
		},
	}

	plan := sessionPlanFor(cfg, "s1")

	// Union of rx1 + rx2, in deterministic (receiver then topic) order.
	require.Len(t, plan.Subscriptions, 3)
	assert.Equal(t, "topic/a", plan.Subscriptions[0].Topic)
	assert.Equal(t, 1, plan.Subscriptions[0].QoS)
	assert.Equal(t, "topic/c", plan.Subscriptions[1].Topic)
	assert.Equal(t, 0, plan.Subscriptions[1].QoS)
	assert.Equal(t, "topic/b", plan.Subscriptions[2].Topic)
	assert.Equal(t, 2, plan.Subscriptions[2].QoS)

	// Publishers is intentionally empty (documented transport-neutral
	// boundary: the exchange name lives in the opaque typed sender config).
	assert.Empty(t, plan.Publishers)
}

// TestSessionPlanFor_PassesTypedSubscriptionConfigThrough proves the per-topic
// typed PluginConfig is forwarded verbatim — identical to what
// receiverSpecFrom puts on the ReceiverSpec — so adapters (e.g. amqp091) read
// the same Subscription config on the spec and on the reconcile plan.
func TestSessionPlanFor_PassesTypedSubscriptionConfigThrough(t *testing.T) {
	subCfg := &testCredConfig{URI: "marker"}
	cfg := &ports.BridgeConfig{
		Receivers: []ports.ReceiverDef{
			{ID: "rx1", Transport: "amqp091", SessionID: "s1", Topics: []ports.SubscriptionDef{
				{Topic: "q1", QoS: 3, Config: subCfg},
			}},
		},
	}

	plan := sessionPlanFor(cfg, "s1")

	require.Len(t, plan.Subscriptions, 1)
	assert.Equal(t, "q1", plan.Subscriptions[0].Topic)
	assert.Equal(t, 3, plan.Subscriptions[0].QoS)
	assert.Same(t, subCfg, plan.Subscriptions[0].Config,
		"typed subscription Config must pass through verbatim (same pointer)")
}

// TestSessionPlanFor_EmptyAndDegenerateInputs covers the nil/empty guards and
// confirms Publishers is always empty (the documented boundary), even when a
// sender is bound to the session.
func TestSessionPlanFor_EmptyAndDegenerateInputs(t *testing.T) {
	assert.Empty(t, sessionPlanFor(nil, "s1").Subscriptions, "nil cfg")
	assert.Empty(t, sessionPlanFor(&ports.BridgeConfig{}, "").Subscriptions, "empty sessionID")

	noMatch := &ports.BridgeConfig{
		Receivers: []ports.ReceiverDef{
			{ID: "rx", SessionID: "other", Topics: []ports.SubscriptionDef{{Topic: "t"}}},
		},
	}
	assert.Empty(t, sessionPlanFor(noMatch, "s1").Subscriptions, "no receiver on session")

	withSender := &ports.BridgeConfig{
		Senders: []ports.SenderDef{{ID: "tx", Transport: "amqp091", SessionID: "s1"}},
		Receivers: []ports.ReceiverDef{
			{ID: "rx", SessionID: "s1", Topics: []ports.SubscriptionDef{{Topic: "t"}}},
		},
	}
	got := sessionPlanFor(withSender, "s1")
	assert.Len(t, got.Subscriptions, 1)
	assert.Empty(t, got.Publishers, "Publishers stays empty even with a sender on the session")
}

// ---------------------------------------------------------------------------
// End-to-end regression test: the assembled plan must be THREADED through the
// builder into the session.Config the session manager reconciles. This is the
// true F1 regression guard — it FAILS before the fix (manager reconciles an
// empty plan) and PASSES after.
// ---------------------------------------------------------------------------

// planCapturingSession records the connectivity.SessionPlan handed to
// Reconcile and signals a channel so the test can synchronize without sleeping.
type planCapturingSession struct {
	fakeSession
	mu         sync.Mutex
	plan       connectivity.SessionPlan
	reconciled chan struct{}
}

func (s *planCapturingSession) Reconcile(_ context.Context, plan connectivity.SessionPlan) error {
	s.mu.Lock()
	s.plan = plan
	s.mu.Unlock()
	select {
	case s.reconciled <- struct{}{}:
	default:
	}
	return nil
}

func (s *planCapturingSession) snapshotPlan() connectivity.SessionPlan {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.plan
}

// planCapturingTransportFactory returns the shared planCapturingSession from
// NewSession; receivers/senders fall back to the embedded fake behaviour.
type planCapturingTransportFactory struct {
	fakeTransportFactory
	session *planCapturingSession
}

func (f *planCapturingTransportFactory) NewSession(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
	return f.session, nil
}

// TestBuilder_AssemblesAndThreadsSessionPlan_SharedSessionUnion builds a
// BridgeConfig with one exclusive broker session shared by TWO receivers (each
// carrying a topic) plus a sender, runs the full Build + Start path, and
// asserts the plan the session manager reconciles contains BOTH subscriptions.
//
// Before the fix sessCfg.Plan was never set, so the manager reconciled an
// empty plan and this assertion fails (zero subscriptions). After the fix the
// per-session union is threaded through and both topics appear.
func TestBuilder_AssemblesAndThreadsSessionPlan_SharedSessionUnion(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "bridge-plan", DrainTimeout: "1s"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "memory"},
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
		Sessions: []ports.SessionDef{
			{ID: "s1", Transport: "mqtt", SessionMode: "exclusive"},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "rx1", Transport: "mqtt", SessionID: "s1", Topics: []ports.SubscriptionDef{
				{Topic: "topic/a", QoS: 1},
			}},
			{ID: "rx2", Transport: "mqtt", SessionID: "s1", Topics: []ports.SubscriptionDef{
				{Topic: "topic/b", QoS: 2},
			}},
		},
		Senders: []ports.SenderDef{
			{ID: "tx1", Transport: "mqtt", SessionID: "s1"},
		},
		Bindings: []ports.BindingDef{
			{ID: "bnd1", SenderID: "tx1", SessionID: "s1", Address: "topic/out"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "rx1",
				DeliveryMode: "shared_outbox",
				Policy:       ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
				Bindings:     []string{"bnd1"},
				Session:      &ports.RouteSessionDef{SessionID: "s1", SenderID: "tx1"},
			},
			{
				ID:           "r2",
				ReceiverID:   "rx2",
				DeliveryMode: "shared_outbox",
				Policy:       ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
				Bindings:     []string{"bnd1"},
				Session:      &ports.RouteSessionDef{SessionID: "s1", SenderID: "tx1"},
			},
		},
	}

	sess := &planCapturingSession{reconciled: make(chan struct{}, 1)}
	tf := &planCapturingTransportFactory{session: sess}

	rt, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", tf).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())
	require.NoError(t, err)
	require.NotNil(t, rt)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = rt.Stop(stopCtx)
	})

	// Handshake: the session manager calls Reconcile once the (fake) lease is
	// acquired. The safety timeout only fires on a regression (empty/absent
	// plan never reconciled) — no time.Sleep is used.
	select {
	case <-sess.reconciled:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for session.Reconcile (plan never threaded to the session manager)")
	}

	plan := sess.snapshotPlan()

	got := make(map[string]int, len(plan.Subscriptions))
	for _, s := range plan.Subscriptions {
		got[s.Topic] = s.QoS
	}
	assert.Equal(t, map[string]int{"topic/a": 1, "topic/b": 2}, got,
		"reconciled plan must carry the per-session union of both receivers' topics")
	assert.Empty(t, plan.Publishers, "Publishers intentionally empty (documented boundary)")
}
