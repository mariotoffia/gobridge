package bridge

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// White-box unit tests for sessionPlanFor (the helper that fixes: the
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

	plan := sessionPlanFor(cfg, "s1", nil)

	// Union of rx1 + rx2, in deterministic (receiver then topic) order.
	require.Len(t, plan.Subscriptions, 3)
	assert.Equal(t, "topic/a", plan.Subscriptions[0].Topic)
	assert.Equal(t, 1, plan.Subscriptions[0].QoS)
	assert.Equal(t, "topic/c", plan.Subscriptions[1].Topic)
	assert.Equal(t, 0, plan.Subscriptions[1].QoS)
	assert.Equal(t, "topic/b", plan.Subscriptions[2].Topic)
	assert.Equal(t, 2, plan.Subscriptions[2].QoS)

	// Publishers is empty because this cfg declares no senders — there is
	// nothing implementing ports.PublishingConfig to advertise an exchange.
	assert.Empty(t, plan.Publishers)
}

// TestSessionPlanFor_PassesTypedSubscriptionConfigThrough proves the per-topic
// typed PluginConfig is forwarded verbatim — identical to what
// receiverSpecFrom puts on the ReceiverSpec — so adapters (e.g. amqp091) read
// the same Subscription config on the spec and on the reconcile plan.
func TestSessionPlanFor_ExpectedReceiverIDsSortedAndDeduplicated(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Receivers: []ports.ReceiverDef{
			{ID: "rx-z", SessionID: "s1", Topics: []ports.SubscriptionDef{{Topic: "z"}}},
			{ID: "rx-a", SessionID: "s1"},
			{ID: "rx-z", SessionID: "s1", Topics: []ports.SubscriptionDef{{Topic: "z/duplicate"}}},
			{ID: "rx-other", SessionID: "s2", Topics: []ports.SubscriptionDef{{Topic: "other"}}},
		},
	}

	plan := sessionPlanFor(cfg, "s1", nil)

	assert.Equal(t, []string{"rx-a", "rx-z"}, plan.ExpectedReceiverIDs)
}

func TestSessionPlanFor_PassesTypedSubscriptionConfigThrough(t *testing.T) {
	subCfg := &testCredConfig{URI: "marker"}
	cfg := &ports.BridgeConfig{
		Receivers: []ports.ReceiverDef{
			{ID: "rx1", Transport: "amqp091", SessionID: "s1", Topics: []ports.SubscriptionDef{
				{Topic: "q1", QoS: 3, Config: subCfg},
			}},
		},
	}

	plan := sessionPlanFor(cfg, "s1", nil)

	require.Len(t, plan.Subscriptions, 1)
	assert.Equal(t, "q1", plan.Subscriptions[0].Topic)
	assert.Equal(t, 3, plan.Subscriptions[0].QoS)
	assert.Same(t, subCfg, plan.Subscriptions[0].Config,
		"typed subscription Config must pass through verbatim (same pointer)")
}

// TestSessionPlanFor_EmptyAndDegenerateInputs covers the nil/empty guards and
// confirms Publishers stays empty when a sender is bound to the session but its
// typed config (here nil) does not implement ports.PublishingConfig.
func TestSessionPlanFor_EmptyAndDegenerateInputs(t *testing.T) {
	assert.Empty(t, sessionPlanFor(nil, "s1", nil).Subscriptions, "nil cfg")
	assert.Empty(t, sessionPlanFor(&ports.BridgeConfig{}, "", nil).Subscriptions, "empty sessionID")

	noMatch := &ports.BridgeConfig{
		Receivers: []ports.ReceiverDef{
			{ID: "rx", SessionID: "other", Topics: []ports.SubscriptionDef{{Topic: "t"}}},
		},
	}
	assert.Empty(t, sessionPlanFor(noMatch, "s1", nil).Subscriptions, "no receiver on session")

	withSender := &ports.BridgeConfig{
		Senders: []ports.SenderDef{{ID: "tx", Transport: "amqp091", SessionID: "s1"}},
		Receivers: []ports.ReceiverDef{
			{ID: "rx", SessionID: "s1", Topics: []ports.SubscriptionDef{{Topic: "t"}}},
		},
	}
	got := sessionPlanFor(withSender, "s1", nil)
	assert.Len(t, got.Subscriptions, 1)
	assert.Empty(t, got.Publishers, "empty because this sender's nil Config doesn't implement ports.PublishingConfig")
}

// ---------------------------------------------------------------------------
// End-to-end regression test: the assembled plan must be THREADED through the
// builder into the session.Config the session manager reconciles. This is the
// true regression guard — it FAILS before the fix (manager reconciles an
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
			{ID: "s1", Transport: "mqtt", SessionMode: "ephemeral"},
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
	assert.Empty(t, plan.Publishers, "empty because the mqtt sender's config doesn't implement ports.PublishingConfig")
}

// ---------------------------------------------------------------------------
// White-box unit tests for the Publishers side of sessionPlanFor: a
// sender whose typed config implements ports.PublishingConfig advertises its
// exchange, which the bridge threads into PublisherPlan.Topic so amqp091's
// declarePublisher auto-declares it.
// ---------------------------------------------------------------------------

// pubDeclConfig is a LOCAL test stub implementing both ports.PluginConfig and
// the OPTIONAL ports.PublishingConfig. It stands in for a transport's typed
// sender config (e.g. amqp091.Config) so the bridge test can exercise the
// Publishers path WITHOUT importing any adapter — the bridge package must never
// depend on an adapter (.go-arch-lint.yml), not even in tests.
type pubDeclConfig struct {
	topic   string
	topoKey string
}

func (*pubDeclConfig) Kind() string                   { return "test.pubdecl" }
func (*pubDeclConfig) Validate() error                { return nil }
func (c *pubDeclConfig) PublisherTopic() string       { return c.topic }
func (c *pubDeclConfig) PublisherTopologyKey() string { return c.topoKey }

// TestSessionPlanFor_PublishersFromDeclarer proves sessionPlanFor derives
// Publishers from senders bound to the session whose typed config implements
// ports.PublishingConfig: the exchange name is threaded verbatim, deduped by
// name, and empty/other-session senders contribute nothing.
func TestSessionPlanFor_PublishersFromDeclarer(t *testing.T) {
	t.Run("declarer contributes one publisher with verbatim config", func(t *testing.T) {
		stub := &pubDeclConfig{topic: "ex.orders"}
		cfg := &ports.BridgeConfig{
			Senders: []ports.SenderDef{
				{ID: "tx1", Transport: "amqp091", SessionID: "s1", Config: stub},
			},
		}

		plan := sessionPlanFor(cfg, "s1", nil)

		require.Len(t, plan.Publishers, 1)
		assert.Equal(t, "ex.orders", plan.Publishers[0].Topic)
		assert.Same(t, stub, plan.Publishers[0].Config,
			"typed sender Config must pass through verbatim (same pointer)")
	})

	t.Run("deduped by exchange name", func(t *testing.T) {
		cfg := &ports.BridgeConfig{
			Senders: []ports.SenderDef{
				{ID: "tx1", Transport: "amqp091", SessionID: "s1", Config: &pubDeclConfig{topic: "ex.orders"}},
				{ID: "tx2", Transport: "amqp091", SessionID: "s1", Config: &pubDeclConfig{topic: "ex.orders"}},
			},
		}

		plan := sessionPlanFor(cfg, "s1", nil)

		require.Len(t, plan.Publishers, 1, "two senders on the same exchange collapse to one publisher")
		assert.Equal(t, "ex.orders", plan.Publishers[0].Topic)
	})

	t.Run("empty topic contributes nothing", func(t *testing.T) {
		cfg := &ports.BridgeConfig{
			Senders: []ports.SenderDef{
				{ID: "tx1", Transport: "amqp091", SessionID: "s1", Config: &pubDeclConfig{topic: ""}},
			},
		}

		assert.Empty(t, sessionPlanFor(cfg, "s1", nil).Publishers,
			"an empty PublisherTopic() means no exchange to declare")
	})

	t.Run("sender on a different session contributes nothing", func(t *testing.T) {
		cfg := &ports.BridgeConfig{
			Senders: []ports.SenderDef{
				{ID: "tx1", Transport: "amqp091", SessionID: "other", Config: &pubDeclConfig{topic: "ex.orders"}},
			},
		}

		assert.Empty(t, sessionPlanFor(cfg, "s1", nil).Publishers,
			"only senders bound to the target session contribute publishers")
	})
}

// TestBuilder_ThreadsSessionPlan_Path2SessionSender_F1P4 is the end-to-end
// regression guard: a session registered ONLY via the builder's
// Path-2 session-sender loop (a shared_outbox binding whose session is no
// route's primary) must reconcile its receivers' subscriptions, not the empty
// plan that session.DefaultConfig carries.
//
// The dedicated session (s-ded) runs on a distinct transport factory so it is
// the ONLY session backed by the plan-capturing fake — the route's primary
// session (s-primary) uses the plain fake and never touches the capture. A
// receiver (rx-ded) bound to s-ded carries the topic the reconciled plan must
// contain.
//
// Before the fix builder_complete.go built the session-sender config with
// session.DefaultConfig(...) (empty plan) and never threaded sessionPlanFor,
// so the capture would hold zero subscriptions and this test FAILS. After the
// fix (sc.Plan = sessionPlanFor(b.cfg, bd.SessionID, b.logger)) it PASSES.
func TestBuilder_ThreadsSessionPlan_Path2SessionSender_F1P4(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "bridge-p4", DrainTimeout: "1s"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "memory"},
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
		Sessions: []ports.SessionDef{
			{ID: "s-primary", Transport: "plain", SessionMode: "exclusive"},
			{ID: "s-ded", Transport: "cap", SessionMode: "exclusive"},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "rx-main", Transport: "plain"},
			// rx-ded is bound to the DEDICATED (Path-2) session and carries the
			// topic the reconciled plan must contain.
			{ID: "rx-ded", Transport: "cap", SessionID: "s-ded", Topics: []ports.SubscriptionDef{
				{Topic: "topic/dedicated", QoS: 1},
			}},
		},
		Senders: []ports.SenderDef{
			{ID: "tx-primary", Transport: "plain", SessionID: "s-primary"},
			{ID: "tx-ded", Transport: "cap", SessionID: "s-ded"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b-primary", SenderID: "tx-primary", SessionID: "s-primary", Address: "topic/a"},
			// b-ded targets s-ded, a dedicated fan-out session that is no route's
			// primary, so it is reached ONLY via the Path-2 session-sender loop.
			{ID: "b-ded", SenderID: "tx-ded", SessionID: "s-ded", Address: "topic/b"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "rx-main",
				DeliveryMode: "shared_outbox",
				DispatchMode: "fanout",
				Policy:       ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
				Bindings:     []string{"b-primary", "b-ded"},
				Session:      &ports.RouteSessionDef{SessionID: "s-primary", SenderID: "tx-primary"},
			},
		},
	}

	dedSess := &planCapturingSession{reconciled: make(chan struct{}, 1)}
	capTF := &planCapturingTransportFactory{session: dedSess}

	rt, err := NewBuilder(cfg).
		RegisterTransportFactory("plain", &fakeTransportFactory{}).
		RegisterTransportFactory("cap", capTF).
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

	// Handshake: the Path-2 session manager reconciles once its (fake) lease is
	// acquired. The safety timeout only fires on a regression (empty/absent
	// plan). No time.Sleep is used.
	select {
	case <-dedSess.reconciled:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for the Path-2 session.Reconcile (plan never threaded to the session-sender)")
	}

	plan := dedSess.snapshotPlan()

	got := make(map[string]int, len(plan.Subscriptions))
	for _, s := range plan.Subscriptions {
		got[s.Topic] = s.QoS
	}
	assert.Equal(t, map[string]int{"topic/dedicated": 1}, got,
		"a Path-2 session-sender must reconcile its receivers' subscriptions, not an empty plan")
}

// warnCountingHandler is a minimal slog.Handler that records only Warn-level (or
// above) entries, so a test can assert exactly-one-warn semantics without
// parsing formatted log output.
type warnCountingHandler struct {
	mu    sync.Mutex
	warns []slog.Record
}

func (h *warnCountingHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= slog.LevelWarn
}

func (h *warnCountingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.warns = append(h.warns, r)
	h.mu.Unlock()
	return nil
}

func (h *warnCountingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *warnCountingHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *warnCountingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.warns)
}

// TestSessionPlanFor_WarnsOnDivergentPublisherTopology is the focused guard for
// REV-2-topowarn: two senders naming the SAME exchange collapse to the FIRST
// (broker first-declare-wins), but a sibling whose publisher.* topology ACTUALLY
// DIFFERS from the kept first is otherwise silently discarded — a misconfig with
// no signal. sessionPlanFor must warn on genuine divergence and stay silent on a
// legitimate identical re-declaration (a plain fan-out of two senders).
func TestSessionPlanFor_WarnsOnDivergentPublisherTopology(t *testing.T) {
	t.Run("divergent topology on same exchange -> one warn + first-wins plan", func(t *testing.T) {
		h := &warnCountingHandler{}
		cfg := &ports.BridgeConfig{
			Senders: []ports.SenderDef{
				{ID: "tx1", Transport: "amqp091", SessionID: "s1",
					Config: &pubDeclConfig{topic: "ex.orders", topoKey: "type=direct;durable=true"}},
				{ID: "tx2", Transport: "amqp091", SessionID: "s1",
					Config: &pubDeclConfig{topic: "ex.orders", topoKey: "type=fanout;durable=false"}},
			},
		}

		plan := sessionPlanFor(cfg, "s1", slog.New(h))

		require.Len(t, plan.Publishers, 1, "first-wins: the two senders collapse to a single publisher")
		assert.Equal(t, "ex.orders", plan.Publishers[0].Topic)
		first, ok := plan.Publishers[0].Config.(*pubDeclConfig)
		require.True(t, ok)
		assert.Equal(t, "type=direct;durable=true", first.topoKey, "the FIRST sender's topology is kept")
		assert.Equal(t, 1, h.count(), "exactly one warn for the genuine topology divergence")
	})

	t.Run("identical topology on same exchange -> no warn", func(t *testing.T) {
		h := &warnCountingHandler{}
		cfg := &ports.BridgeConfig{
			Senders: []ports.SenderDef{
				{ID: "tx1", Transport: "amqp091", SessionID: "s1",
					Config: &pubDeclConfig{topic: "ex.orders", topoKey: "type=direct;durable=true"}},
				{ID: "tx2", Transport: "amqp091", SessionID: "s1",
					Config: &pubDeclConfig{topic: "ex.orders", topoKey: "type=direct;durable=true"}},
			},
		}

		plan := sessionPlanFor(cfg, "s1", slog.New(h))

		require.Len(t, plan.Publishers, 1)
		assert.Equal(t, 0, h.count(),
			"an identical re-declaration is a legitimate fan-out and must stay silent")
	})
}
