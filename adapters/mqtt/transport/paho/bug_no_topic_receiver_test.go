package paho

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// Chunk 4 — MQTT plugin production-readiness (HIGH) — one focused, deterministic
// unit test per fix. Each test states the mutation (counterfactual) that would
// make it fail if the fix were reverted.
// ═══════════════════════════════════════════════════════════════════════════

// TestNoTopicReceiver_RejectedNotMatchAll pins c4-notopic-matchall
// (factory.go): a config-driven receiver with ZERO subscription topics must be
// REJECTED. The router treats an empty filter set as match-all
// (matchesAnyFilter), so a no-topic receiver on a shared session would receive
// EVERY publish, join ACK splitting, and defeat orphan cleanup — flooding the
// route with unintended traffic.
//
// Mutation: deleting the `len(filters) == 0` reject in Factory.NewReceiver
// makes err nil (a match-all receiver is silently constructed) → this test
// fails on the first assertion.
func TestNoTopicReceiver_RejectedNotMatchAll(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "c4-notopic",
	}, connectivity.SessionEphemeral, nil)

	f := &Factory{}

	// Zero topics ⇒ configuration error, not an implicit match-all.
	_, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:        "rx-notopic",
		SessionID: "s",
	}, sess)
	if err == nil {
		t.Fatal("c4-notopic-matchall: a receiver with zero topics must be rejected, " +
			"not made a match-all subscriber")
	}
	be, ok := shared.AsBridgeError(err)
	if !ok {
		t.Fatalf("expected classified *shared.BridgeError, got %T: %v", err, err)
	}
	if be.Code != shared.ErrInvalidPayload.Code {
		t.Fatalf("expected ErrInvalidPayload, got %s", be.Code)
	}

	// Guard: the rejection is specific to the zero-topic case — a receiver
	// WITH at least one topic still builds successfully.
	if _, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:            "rx-ok",
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "sensors/x", QoS: 1}},
	}, sess); err != nil {
		t.Fatalf("a receiver with a topic must be accepted: %v", err)
	}
}

// TestShortSuback_ReconcileFails pins c4-short-suback (acl_session.go),
// end-to-end through Session.reconcile: a SUBACK carrying fewer reason codes
// than requested subscriptions leaves the tail topics UNCONFIRMED by the
// broker. Reconcile must surface that as a failure (ErrProtocolError) rather
// than silently reporting success while a subscription was never established.
//
// Mutation: reverting the short-SUBACK branch to treat an out-of-range index
// as accepted makes classifySubackReasons return firstErr == nil, so Reconcile
// returns nil → this test fails.
func TestShortSuback_ReconcileFails(t *testing.T) {
	// Broker returns a single reason for a two-topic SUBSCRIBE: exactly one
	// topic is confirmed, the other is left unconfirmed. Both requested at
	// QoS 0 so the confirmed topic is NOT also a QoS downgrade (keeps the
	// failure signal clean). toSub order is map-derived (non-deterministic),
	// so we assert only the error CODE, which holds regardless of which
	// topic lands at the missing index.
	fake := &fakeReconcileConn{reasons: []byte{0x00}}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "c4-short-suback",
	}, connectivity.SessionEphemeral, nil)
	s.mu.Lock()
	s.cm = fake
	s.mu.Unlock()

	err := s.Reconcile(context.Background(), connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: "a", QoS: 0},
			{Topic: "b", QoS: 0},
		},
	})
	if err == nil {
		t.Fatal("c4-short-suback: a short SUBACK must fail Reconcile (unconfirmed subscription)")
	}
	be, ok := shared.AsBridgeError(err)
	if !ok {
		t.Fatalf("expected classified *shared.BridgeError, got %T: %v", err, err)
	}
	if be.Code != shared.ErrProtocolError.Code {
		t.Fatalf("expected ErrProtocolError for a short SUBACK, got %s", be.Code)
	}
}

// TestQoSDowngrade_ReconcileFailsAndRemainsNonFull proves that a broker
// grant below the requested QoS is not accepted as active session state.
func TestQoSDowngrade_ReconcileFailsAndRemainsNonFull(t *testing.T) {
	rec := &ports.RecordingExporter{}
	logs := &recordingLogHandler{}
	fake := &fakeReconcileConn{reasons: []byte{0x00}}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "c4-downgrade",
	}, connectivity.SessionEphemeral, slog.New(logs), rec)
	s.mu.Lock()
	s.cm = fake
	s.connected = true
	emptyApplied := connectivity.SessionPlan{}
	s.appliedPlan = &emptyApplied
	s.mu.Unlock()
	s.router.Register("rx-sensors", func(*pahov5.Publish) {})

	plan := connectivity.SessionPlan{
		Subscriptions:       []connectivity.SubscriptionPlan{{Topic: "sensors/x", QoS: 1}},
		ExpectedReceiverIDs: []string{"rx-sensors"},
	}

	err := s.Reconcile(context.Background(), plan)
	if err == nil {
		t.Fatal("a SUBACK QoS grant below the requested QoS must fail reconcile")
	}
	if !errors.Is(err, shared.ErrQoSNotSupported) {
		t.Fatalf("expected ErrQoSNotSupported, got %T: %v", err, err)
	}
	be, ok := shared.AsBridgeError(err)
	if !ok {
		t.Fatalf("expected classified *shared.BridgeError, got %T: %v", err, err)
	}
	if be.Context["topic"] != "sensors/x" || be.Context["requested_qos"] != 1 || be.Context["granted_qos"] != 0 {
		t.Fatalf("downgrade error context = %v, want topic/requested/granted", be.Context)
	}
	if got := len(rec.FindEntries(MetricMQTTQoSDowngraded)); got != 1 {
		t.Fatalf("downgrade metric count = %d, want 1", got)
	}
	s.mu.Lock()
	grant, observed := s.observedSubs["sensors/x"]
	_, active := s.activeSubs["sensors/x"]
	s.mu.Unlock()
	if !observed || grant.Requested != 1 || grant.Granted != 0 {
		t.Fatalf("broker-observed grant = %+v, present=%v; want requested=1 granted=0", grant, observed)
	}
	if active {
		t.Fatal("downgraded subscription must not be marked contract-active")
	}
	h := s.Health(context.Background())
	if got := h.ServiceLevel; got == ports.ServiceLevelFull {
		t.Fatalf("downgraded subscription health = %s, must remain non-Full", got)
	}
	if h.SubscriptionsSatisfied == nil || *h.SubscriptionsSatisfied {
		t.Fatalf("downgraded subscription satisfaction = %v, want explicit false", h.SubscriptionsSatisfied)
	}

	// The broker-observed downgrade remains deficient, but a repeated reconcile
	// must not issue another SUBSCRIBE or repeat the warning metric in a tight loop.
	err = s.Reconcile(context.Background(), plan)
	if !errors.Is(err, shared.ErrQoSNotSupported) {
		t.Fatalf("repeated reconcile error = %v, want ErrQoSNotSupported", err)
	}
	if got := fake.subscribeCallCount(); got != 1 {
		t.Fatalf("subscribe calls after unchanged retry = %d, want 1", got)
	}
	if got := len(rec.FindEntries(MetricMQTTQoSDowngraded)); got != 1 {
		t.Fatalf("downgrade metric count after unchanged retry = %d, want 1", got)
	}
	if got := logs.warnCountContaining("downgraded subscription QoS"); got != 1 {
		t.Fatalf("downgrade warning count after unchanged retry = %d, want 1", got)
	}

	// Removing the route must unsubscribe the broker-observed downgraded filter,
	// even though it was never contract-active.
	if err := s.Reconcile(context.Background(), connectivity.SessionPlan{}); err != nil {
		t.Fatalf("empty-plan cleanup reconcile: %v", err)
	}
	if got := fake.unsubscribeCallCount(); got != 1 {
		t.Fatalf("cleanup unsubscribe calls = %d, want 1", got)
	}
	unsubscribed := fake.unsubscribedTopics()
	if len(unsubscribed) != 1 || len(unsubscribed[0]) != 1 || unsubscribed[0][0] != "sensors/x" {
		t.Fatalf("cleanup unsubscribed topics = %v, want [[sensors/x]]", unsubscribed)
	}
}

// TestEmptyPlanUnsubscribesManagedSubs pins c4-remove-subs
// (session_reconcile.go): an empty target plan handed to Reconcile while
// managed subscriptions are still active must issue the UNSUBSCRIBE for the
// previously-held topics — otherwise the broker keeps delivering on stale
// subscriptions the router then ack-drops as orphans forever.
//
// Mutation: making an empty plan an UNCONDITIONAL no-op (`if
// len(plan.Subscriptions) == 0 { return nil }`, the original c4-remove-subs
// bug) short-circuits before reconcile(), so Unsubscribe is never called even
// though managed subscriptions are active → this test fails.
func TestEmptyPlanUnsubscribesManagedSubs(t *testing.T) {
	fake := &fakeReconcileConn{}
	s, _ := c7Session(t, fake, "kept", 1) // activeSubs = {kept:1}, plan = {kept}

	if err := s.Reconcile(context.Background(), connectivity.SessionPlan{}); err != nil {
		t.Fatalf("empty-plan Reconcile: %v", err)
	}
	if got := fake.unsubscribeCallCount(); got != 1 {
		t.Fatalf("c4-remove-subs: empty plan with active subs must Unsubscribe once, got %d", got)
	}
	if got := fake.subscribeCallCount(); got != 0 {
		t.Fatalf("empty plan must not Subscribe, got %d", got)
	}
	s.mu.Lock()
	n := len(s.activeSubs)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("activeSubs must be cleared after removing all subscriptions, got %d", n)
	}
}

// TestReconnectWindow_EmptyPlanUnsubscribesResumedSub pins the reconnect-
// window half of c4-remove-subs (session_reconcile.go): handleConnectionUp
// resets activeSubs to empty on every reconnect, but a clean_start=false broker
// still holds the resumed subscriptions. An empty plan reconciled in that
// post-reset/pre-resubscribe window MUST still UNSUBSCRIBE the prior desired
// topic — gating teardown on the volatile (now-empty) activeSubs would no-op
// and orphan the broker sub until the router's grace-sweep backstop fires on a
// later publish.
//
// Mutation (kills BOTH halves of the fix):
//   - revert the guard to `... && !hasActiveSubs`: in the reset window
//     hasActiveSubs is false, so Reconcile short-circuits to a no-op → no
//     UNSUBSCRIBE; OR
//   - compute toUnsub from `current` alone (drop the priorTopics union in
//     reconcile): the guard passes but activeSubs is empty so nothing is torn
//     down → no UNSUBSCRIBE.
//
// Either mutation leaves unsubCalls==0 → this test fails.
func TestReconnectWindow_EmptyPlanUnsubscribesResumedSub(t *testing.T) {
	fake := &fakeReconcileConn{}
	s, _ := c7Session(t, fake, "resumed/topic", 1) // plan={resumed/topic}, activeSubs={resumed/topic:1}

	// Simulate the post-reconnect window: handleConnectionUp reset activeSubs
	// to empty while the broker still holds the resumed subscription. s.plan
	// (the desired-state history) is deliberately NOT reset by reconnect.
	s.mu.Lock()
	s.activeSubs = map[string]byte{}
	s.mu.Unlock()

	// A config reload removed the last receiver → empty plan reconciled in the
	// reset/pre-resubscribe window.
	if err := s.Reconcile(context.Background(), connectivity.SessionPlan{}); err != nil {
		t.Fatalf("reconnect-window empty-plan Reconcile: %v", err)
	}
	if got := fake.unsubscribeCallCount(); got != 1 {
		t.Fatalf("c4-remove-subs: reconnect-window empty plan must UNSUBSCRIBE the resumed sub once, got %d", got)
	}
	unsub := fake.unsubscribedTopics()
	if len(unsub) != 1 || len(unsub[0]) != 1 || unsub[0][0] != "resumed/topic" {
		t.Fatalf("c4-remove-subs: must UNSUBSCRIBE the prior desired topic 'resumed/topic', got %v", unsub)
	}
	if got := fake.subscribeCallCount(); got != 0 {
		t.Fatalf("empty plan must not Subscribe, got %d", got)
	}
}

// TestQoS12ByteCap_NeverDropsQoS12 pins the reworked c4-qos12-overflow
// (acl_router.go bufferLocked): the pending buffer's BYTE ceiling governs QoS 0
// only — a QoS 1/2 publish is NEVER dropped for it, because dropping a QoS 1/2
// is unsafe (ack+drop loses it; un-ack+drop head-of-line-blocks paho's
// contiguous-prefix ack stream and wedges ingress). QoS 1/2 pending memory is
// bounded instead by the entry-count cap (== receive_maximum). This test drives
// a QoS 1 backlog past the byte ceiling and asserts every message is buffered
// (none dropped), then flushed and acked in arrival order once a handler
// registers (the ack stream drains — ingress is not wedged).
//
// Mutation: reintroduce a byte-cap `return false` for QoS 1/2 in bufferLocked
// (either the un-ack+drop or ack+drop variant) → PendingCount falls below n and
// the delivered set loses the tail → this test fails.
func TestQoS12ByteCap_NeverDropsQoS12(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk), withUnmatchedGrace(testGrace))
	defer r.shutdown()

	// Byte ceiling far below the backlog; the count cap keeps its default so
	// only the byte ceiling is exercised. Within grace, no handler → buffer path.
	r.mu.Lock()
	r.pendingBytesLimit = 16
	r.mu.Unlock()

	const n = 4
	payloads := make([]string, n)
	acked := make([]atomic.Int32, n)
	for i := 0; i < n; i++ {
		payloads[i] = fmt.Sprintf("p-%02d", i) // 4B payload + 3B topic = 7B each; n×7 ≫ 16B
		idx := i
		r.dispatch(&pahov5.Publish{Topic: "t/1", QoS: 1, Payload: []byte(payloads[idx])},
			func() error { acked[idx].Add(1); return nil })
	}

	// No QoS 1/2 dropped for the byte ceiling: all buffered, none acked yet.
	if got := r.PendingCount(); got != n {
		t.Fatalf("c4-qos12-overflow: expected all %d QoS 1 buffered (byte cap must not drop QoS 1/2), got %d", n, got)
	}
	if got := r.OverflowDroppedCount(); got != 0 {
		t.Fatalf("byte ceiling must not trigger a QoS 1/2 overflow drop, OverflowDroppedCount=%d", got)
	}
	for i := 0; i < n; i++ {
		if acked[i].Load() != 0 {
			t.Fatalf("buffered QoS 1 #%d must stay un-acked until delivered", i)
		}
	}

	// Register the handler: the backlog flushes in arrival order and each acks.
	var mu sync.Mutex
	var delivered []string
	r.RegisterFiltered("rx", []string{"t/1"}, func(pub *pahov5.Publish, ack func() error) {
		mu.Lock()
		delivered = append(delivered, string(pub.Payload))
		mu.Unlock()
		if ack != nil {
			_ = ack()
		}
	})
	r.Wait() // RegisterFiltered enrolled the flush in r.wg before returning

	mu.Lock()
	got := append([]string(nil), delivered...)
	mu.Unlock()
	if !slices.Equal(got, payloads) {
		t.Fatalf("delivered order = %v, want %v (every QoS 1 delivered once, in arrival order)", got, payloads)
	}
	for i := 0; i < n; i++ {
		if acked[i].Load() != 1 {
			t.Fatalf("QoS 1 #%d ack count = %d, want 1 (ack stream drains — ingress not wedged)", i, acked[i].Load())
		}
	}
}
