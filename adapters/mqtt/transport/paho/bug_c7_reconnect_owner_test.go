package paho

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ═══════════════════════════════════════════════════════════════════════════
// C7 — Single owner of reconnect reconciliation
//
// Finding C7: paho OnConnectionUp reconciled
// subscriptions inline AND the runtime session manager reconciled again on
// the same SessionConnected event. The real defect was silent message loss:
//   - OnConnectionUp emitted SessionConnected BEFORE resetting activeSubs,
//     so the manager's reconcile could observe the pre-drop subscriptions
//     (== desired), compute an empty delta, issue no SUBSCRIBE, and return
//     nil — never noticing the broker no longer held the subscription; then
//   - OnConnectionUp's inline reconcile failed (e.g. ACL change) and
//     SWALLOWED the error (Warn log only), restoring activeSubs.
//   Net: broker unsubscribed, no error propagated, finding S9 never fired.
//
// Fix: the runtime session manager is the SINGLE owner of reconnect
// reconciliation. OnConnectionUp (handleConnectionUp) only resets activeSubs
// to empty BEFORE signalling SessionConnected and never reconciles inline.
// The manager-driven Reconcile re-subscribes the full plan and its
// success/failure is authoritative; SessionReconciled is emitted from that
// single owner (Session.Reconcile) on success.
// ═══════════════════════════════════════════════════════════════════════════

// fakeReconcileConn is a controllable pahoConnection seam. It records
// Subscribe / Unsubscribe calls and returns canned SUBACK reason codes so
// C7 tests can drive the reconcile path without a live broker. It is the
// only test double that actually exercises cm.Subscribe (the sentinel
// fakeCM used elsewhere is non-functional and panics if called).
type fakeReconcileConn struct {
	mu          sync.Mutex
	subCalls    int
	subTopics   [][]string
	unsubCalls  int
	unsubTopics [][]string

	// reasons, when non-nil, is returned as the SUBACK reason vector for
	// every Subscribe. A byte >= 0x80 marks a rejected topic (mapped to a
	// BridgeError by classifySubackReasons). When nil, all requested topics
	// are accepted (reason 0x00).
	reasons []byte
}

func (f *fakeReconcileConn) AwaitConnection(context.Context) error { return nil }
func (f *fakeReconcileConn) Disconnect(context.Context) error      { return nil }

func (f *fakeReconcileConn) Subscribe(_ context.Context, subs []subscribeSpec) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subCalls++
	topics := make([]string, len(subs))
	for i, s := range subs {
		topics[i] = s.Topic
	}
	f.subTopics = append(f.subTopics, topics)
	if f.reasons != nil {
		return f.reasons, nil
	}
	// Default: accept every subscription at the REQUESTED QoS (granted ==
	// requested, no downgrade). Echoing the requested QoS keeps the SUBACK
	// realistic now that reconcile persists the GRANTED QoS and surfaces
	// downgrades (c4-qos-downgrade).
	granted := make([]byte, len(subs))
	for i, s := range subs {
		granted[i] = s.QoS
	}
	return granted, nil
}

func (f *fakeReconcileConn) Unsubscribe(_ context.Context, topics []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unsubCalls++
	f.unsubTopics = append(f.unsubTopics, append([]string(nil), topics...))
	return nil
}

func (f *fakeReconcileConn) PublishEnvelope(
	context.Context, *messaging.Envelope, string, SenderOptions, clock.Clock,
) (publishResult, error) {
	return publishResult{}, nil
}

func (f *fakeReconcileConn) Underlying() *autopaho.ConnectionManager { return nil }

func (f *fakeReconcileConn) subscribeCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subCalls
}

func (f *fakeReconcileConn) unsubscribeCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unsubCalls
}

// unsubscribedTopics returns a copy of the topic slices passed to each
// Unsubscribe call, in call order.
func (f *fakeReconcileConn) unsubscribedTopics() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.unsubTopics))
	for i, t := range f.unsubTopics {
		out[i] = append([]string(nil), t...)
	}
	return out
}

var _ pahoConnection = (*fakeReconcileConn)(nil)

// c7Session builds a Session wired to the given fake connection with a
// single-topic plan already reconciled and active — i.e. the state just
// before a connection drop.
func c7Session(t *testing.T, fake pahoConnection, topic string, qos byte) (*Session, connectivity.SessionPlan) {
	t.Helper()
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "c7-" + topic,
	}, connectivity.SessionEphemeral, nil)

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: int(qos)}},
	}
	s.mu.Lock()
	s.cm = fake
	s.plan = &plan
	s.activeSubs = map[string]byte{topic: qos} // active before the drop
	s.mu.Unlock()
	return s, plan
}

// TestC7_OnConnectionUp_ResetsActiveSubsBeforeSignal pins the new
// OnConnectionUp contract: handleConnectionUp resets activeSubs to empty and
// emits SessionConnected, and does NOT reconcile inline. The reset is
// observable as empty the moment SessionConnected is seen, which is what
// guarantees the manager's reconcile (triggered by that event) sees an empty
// set and performs a full re-subscribe.
func TestC7_OnConnectionUp_ResetsActiveSubsBeforeSignal(t *testing.T) {
	fake := &fakeReconcileConn{}
	s, _ := c7Session(t, fake, "t/x", 1)

	s.handleConnectionUp()

	ev := wait.RequireReceive(t, s.Events(), time.Second)
	if ev.Type != ports.SessionConnected {
		t.Fatalf("expected SessionConnected, got %v", ev.Type)
	}

	s.mu.Lock()
	n := len(s.activeSubs)
	connected := s.connected
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("C7: activeSubs must be reset to empty before SessionConnected, got %d entries", n)
	}
	if !connected {
		t.Fatal("C7: connected must be true after OnConnectionUp")
	}

	// OnConnectionUp must not reconcile inline: no Subscribe, no
	// SessionReconciled emitted by the callback itself.
	if got := fake.subscribeCallCount(); got != 0 {
		t.Fatalf("C7: OnConnectionUp must not Subscribe inline, got %d Subscribe calls", got)
	}
	wait.Silent(t, s.Events(), 100*time.Millisecond)
}

// TestC7_ReconnectResubscribe_SingleSubscribeAndReconciledOnce is the C7
// happy path: after a reconnect, exactly ONE effective re-subscribe reaches
// the broker (no duplicate SUBSCRIBE from a second owner) and exactly ONE
// SessionReconciled is emitted — from the manager-driven Reconcile.
func TestC7_ReconnectResubscribe_SingleSubscribeAndReconciledOnce(t *testing.T) {
	fake := &fakeReconcileConn{} // accept all
	s, plan := c7Session(t, fake, "sensors/a", 1)

	// autopaho fires OnConnectionUp on reconnect: reset + signal.
	s.handleConnectionUp()
	if ev := wait.RequireReceive(t, s.Events(), time.Second); ev.Type != ports.SessionConnected {
		t.Fatalf("expected SessionConnected, got %v", ev.Type)
	}

	// The manager reacts to SessionConnected by reconciling the plan.
	if err := s.Reconcile(context.Background(), plan); err != nil {
		t.Fatalf("reconnect Reconcile: %v", err)
	}

	// Exactly one SessionReconciled from the single reconcile.
	if ev := wait.RequireReceive(t, s.Events(), time.Second); ev.Type != ports.SessionReconciled {
		t.Fatalf("expected SessionReconciled after reconnect reconcile, got %v", ev.Type)
	}
	wait.Silent(t, s.Events(), 100*time.Millisecond) // no second Reconciled

	// One effective re-subscribe reached the broker (empty activeSubs after
	// the reset means the full plan was re-issued exactly once).
	if got := fake.subscribeCallCount(); got != 1 {
		t.Fatalf("expected exactly 1 Subscribe on reconnect, got %d", got)
	}

	s.mu.Lock()
	active := len(s.activeSubs)
	_, hasTopic := s.activeSubs["sensors/a"]
	s.mu.Unlock()
	if active != 1 || !hasTopic {
		t.Fatalf("expected activeSubs to hold sensors/a after reconcile, got %d entries", active)
	}
}

// TestC7_ReconnectResubscribeFailure_PropagatesViaManagerReconcile is the C7
// regression: on a reconnect whose re-subscribe is REJECTED by the broker
// (ACL change), the failure PROPAGATES out of Session.Reconcile — which the
// runtime session manager drives on SessionConnected — instead of being
// swallowed by an inline reconcile in OnConnectionUp.
//
// Contrast TestC7_StaleActiveSubs_ZeroDeltaMasksBrokerRejection: the ONLY
// difference is that C7 resets activeSubs to empty BEFORE signalling, so the
// reconcile actually issues SUBSCRIBE and observes the rejection.
func TestC7_ReconnectResubscribeFailure_PropagatesViaManagerReconcile(t *testing.T) {
	fake := &fakeReconcileConn{reasons: []byte{0x87}} // 0x87 = not authorized
	s, plan := c7Session(t, fake, "acl/denied", 1)

	// Reconnect: OnConnectionUp resets activeSubs and signals connected.
	s.handleConnectionUp()
	if ev := wait.RequireReceive(t, s.Events(), time.Second); ev.Type != ports.SessionConnected {
		t.Fatalf("expected SessionConnected, got %v", ev.Type)
	}

	// Manager-driven reconcile re-subscribes the full plan and hits the ACL
	// rejection, which must surface.
	err := s.Reconcile(context.Background(), plan)
	if err == nil {
		t.Fatal("C7 regression: reconnect re-subscribe rejection must propagate from Reconcile")
	}
	be, ok := shared.AsBridgeError(err)
	if !ok {
		t.Fatalf("expected classified *shared.BridgeError, got %T: %v", err, err)
	}
	if be.Code != shared.ErrForbidden.Code {
		t.Fatalf("expected ErrForbidden (0x87 not authorized), got %s", be.Code)
	}

	// The rejection was seen because a real SUBSCRIBE was issued.
	if got := fake.subscribeCallCount(); got != 1 {
		t.Fatalf("expected exactly 1 Subscribe attempt on reconnect, got %d", got)
	}

	// A failed reconcile must NOT emit SessionReconciled.
	wait.Silent(t, s.Events(), 100*time.Millisecond)
}

// TestC7_StaleActiveSubs_ZeroDeltaMasksBrokerRejection documents the pre-C7
// silent-loss mechanism this fix eliminates. With the OLD ordering,
// OnConnectionUp emitted SessionConnected BEFORE resetting activeSubs, so the
// manager's reconcile could run while activeSubs still equalled the desired
// plan. The delta is then empty, no SUBSCRIBE is issued, and the broker's ACL
// rejection is never observed — Reconcile returns nil and falsely emits
// SessionReconciled while the broker holds no subscription.
//
// This is the "before" half of the regression: same rejecting broker and
// plan as TestC7_ReconnectResubscribeFailure_PropagatesViaManagerReconcile;
// only the activeSubs state (stale vs C7-reset) differs.
func TestC7_StaleActiveSubs_ZeroDeltaMasksBrokerRejection(t *testing.T) {
	fake := &fakeReconcileConn{reasons: []byte{0x87}} // broker would reject a real SUBSCRIBE
	s, plan := c7Session(t, fake, "acl/denied", 1)
	// NOTE: no handleConnectionUp() — activeSubs stays stale (== desired),
	// modelling the old emit-before-reset ordering's race outcome.

	err := s.Reconcile(context.Background(), plan)
	if err != nil {
		t.Fatalf("stale-state reconcile unexpectedly errored (masking mechanism changed): %v", err)
	}
	if got := fake.subscribeCallCount(); got != 0 {
		t.Fatalf("stale delta must not issue SUBSCRIBE (that is why the rejection was masked); got %d", got)
	}
	// The masked path even falsely signals reconciled.
	if ev := wait.RequireReceive(t, s.Events(), time.Second); ev.Type != ports.SessionReconciled {
		t.Fatalf("expected the (falsely) successful SessionReconciled on the masked path, got %v", ev.Type)
	}
}

// TestC7_Reconcile_EmptyPlanRemovesManagedSubs asserts the intentional
// "remove all subscriptions" semantics (c4-remove-subs): an empty plan handed
// to Reconcile while managed subscriptions are still active MUST unsubscribe
// them (converging broker state) and emit SessionReconciled — it does real
// work. The prior behaviour treated this as a silent no-op, leaving the broker
// delivering on stale subscriptions the router then ack-drops as orphans
// forever.
func TestC7_Reconcile_EmptyPlanRemovesManagedSubs(t *testing.T) {
	fake := &fakeReconcileConn{}
	s, _ := c7Session(t, fake, "kept", 0) // activeSubs = {kept:0}, plan = {kept}

	if err := s.Reconcile(context.Background(), connectivity.SessionPlan{}); err != nil {
		t.Fatalf("empty-plan Reconcile error: %v", err)
	}
	// The managed subscription must be UNSUBSCRIBED, not left alive.
	if got := fake.unsubscribeCallCount(); got != 1 {
		t.Fatalf("empty plan with active subs must Unsubscribe once, got %d", got)
	}
	if got := fake.subscribeCallCount(); got != 0 {
		t.Fatalf("empty plan must not Subscribe, got %d", got)
	}
	s.mu.Lock()
	n := len(s.activeSubs)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("activeSubs must be empty after removing all subscriptions, got %d", n)
	}
	// A reconcile that actually converged broker state signals reconciled.
	if ev := wait.RequireReceive(t, s.Events(), time.Second); ev.Type != ports.SessionReconciled {
		t.Fatalf("expected SessionReconciled after removing all subs, got %v", ev.Type)
	}
}

// TestC7_Reconcile_InitialEmptyPlan_EmitsReconciled covers the sender-only
// initial-connect case: an empty plan with no prior plan runs a genuine
// (zero-delta) reconcile and therefore signals reconciled once.
func TestC7_Reconcile_InitialEmptyPlan_EmitsReconciled(t *testing.T) {
	fake := &fakeReconcileConn{}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "c7-sender-only",
	}, connectivity.SessionEphemeral, nil)
	s.mu.Lock()
	s.cm = fake // no prior plan
	s.mu.Unlock()

	if err := s.Reconcile(context.Background(), connectivity.SessionPlan{}); err != nil {
		t.Fatalf("initial empty-plan Reconcile error: %v", err)
	}
	if ev := wait.RequireReceive(t, s.Events(), time.Second); ev.Type != ports.SessionReconciled {
		t.Fatalf("initial empty-plan reconcile should emit SessionReconciled, got %v", ev.Type)
	}
	if got := fake.subscribeCallCount(); got != 0 {
		t.Fatalf("empty plan must not Subscribe, got %d", got)
	}
}
