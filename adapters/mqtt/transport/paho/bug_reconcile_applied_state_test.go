package paho

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// blocking-#1: a QoS 1 publish RETAINED as covered past the grace window becomes
// a TRUE ORPHAN the instant config removes its covering subscription. A
// successful Reconcile that REMOVES that coverage must RE-SWEEP the pending
// buffer so the now-orphan entry is acked-and-dropped — otherwise it stays
// un-acked forever, pinning the broker Receive-Maximum window and wedging
// ingress.
//
// blocking-#2: a FAILED reconcile must NOT be committed as applied history. The
// empty-plan no-op and the reconnect-window teardown key off the last
// SUCCESSFULLY applied plan, so a timed-out Unsubscribe must be RETRIED by the
// next reconcile (not short-circuited as a no-op).
// ═══════════════════════════════════════════════════════════════════════════

// TestBug_Reconcile_RemovingCoverage_ReclassifiesRetainedPending pins
// blocking-#1: a covered QoS 1 publish retained past grace is reclassified as an
// orphan (acked + dropped) once a successful Reconcile removes its coverage, so
// it no longer pins the receive window.
//
// Mutation killed: delete the `s.router.reclassifyPending()` call from
// Reconcile. Then the retained publish is never re-swept: it stays un-acked
// (acked==0) and pinned (PendingCount==1) and both require checks below FAIL.
func TestBug_Reconcile_RemovingCoverage_ReclassifiesRetainedPending(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	sess := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
		ClientID:       "reclassify",
		UnmatchedGrace: testGrace,
		Clock:          clk,
	}, connectivity.SessionPersistent, nil, rec)

	fake := &recordingUnsubConn{}
	// Model a session that already SUCCESSFULLY reconciled a plan wanting
	// live/route: the plan is desired AND applied AND the broker sub is active.
	sess.mu.Lock()
	sess.cm = fake
	sess.plan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "live/route", QoS: 1}},
	}
	sess.appliedPlan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "live/route", QoS: 1}},
	}
	sess.activeSubs = map[string]byte{"live/route": 1}
	sess.mu.Unlock()

	// Past the grace window a covered publish whose handler has not registered
	// is RETAINED un-acked (HIGH-1) — this is the pinned entry.
	clk.Advance(testGrace + time.Second)
	var acked int32
	var ackMu sync.Mutex
	sess.Router().dispatch(&pahov5.Publish{Topic: "live/route", QoS: 1, Payload: []byte("x")},
		func() error { ackMu.Lock(); acked++; ackMu.Unlock(); return nil })

	require.Equal(t, 1, sess.Router().PendingCount(),
		"the covered QoS 1 publish is retained un-acked past grace")
	ackMu.Lock()
	require.Equal(t, int32(0), acked, "a retained covered publish is NOT acked (it pins the window)")
	ackMu.Unlock()
	require.Equal(t, int64(1), sess.Router().CoveredRetainedCount())

	// Config removes the live/route receiver: Reconcile(empty) SUCCEEDS
	// (unsubscribes live/route). The retained publish is now a true orphan.
	require.NoError(t, sess.Reconcile(context.Background(), connectivity.SessionPlan{}))

	require.Equal(t, []string{"live/route"}, fake.unsubscribed(),
		"the removed subscription is unsubscribed on the broker")

	// blocking-#1: the reconcile re-swept the pending buffer, so the now-orphan
	// retained publish was acked-and-dropped and no longer pins the window.
	ackMu.Lock()
	require.Equal(t, int32(1), acked,
		"the orphaned retained publish must be ACKED so it no longer pins the receive window")
	ackMu.Unlock()
	require.Equal(t, 0, sess.Router().PendingCount(),
		"the orphaned retained publish must be DROPPED from the pending buffer")
	require.Equal(t, int64(1), sess.Router().UnmatchedDroppedCount(),
		"the reclassified entry is counted as orphan cleanup")
}

// failThenOKUnsubConn fails its FIRST Unsubscribe (simulating a timed-out broker
// op) and succeeds on every later call, recording each attempt. Subscribe is a
// no-op success.
type failThenOKUnsubConn struct {
	mu          sync.Mutex
	unsubCalls  int
	unsubTopics []string
}

func (c *failThenOKUnsubConn) AwaitConnection(context.Context) error { return nil }
func (c *failThenOKUnsubConn) Disconnect(context.Context) error      { return nil }
func (c *failThenOKUnsubConn) Subscribe(context.Context, []subscribeSpec) ([]byte, error) {
	return nil, nil
}

func (c *failThenOKUnsubConn) Unsubscribe(_ context.Context, topics []string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unsubCalls++
	if c.unsubCalls == 1 {
		return nil, errors.New("mqtt: unsubscribe timed out")
	}
	c.unsubTopics = append(c.unsubTopics, topics...)
	return make([]byte, len(topics)), nil
}

func (c *failThenOKUnsubConn) PublishEnvelope(
	context.Context, *messaging.Envelope, string, SenderOptions, clock.Clock,
) (publishResult, error) {
	return publishResult{}, nil
}
func (c *failThenOKUnsubConn) Underlying() *autopaho.ConnectionManager { return nil }

func (c *failThenOKUnsubConn) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unsubCalls
}

var _ pahoConnection = (*failThenOKUnsubConn)(nil)

// TestBug_Reconcile_FailedUnsubscribe_RetriedNotNoOped pins blocking-#2: a
// reconcile whose Unsubscribe fails must NOT advance the applied history, so the
// NEXT empty reconcile RETRIES the unsubscribe instead of short-circuiting as a
// no-op.
//
// Mutation killed: base the empty-plan no-op (and priorPlanTopics) on the
// DESIRED plan (s.plan) again instead of appliedPlan — i.e. set
// s.appliedPlan on every call OR overwrite it before the broker op. Then after
// the first failed reconcile the desired plan is already empty, the second
// Reconcile(empty) no-ops, the unsubscribe is never retried (calls stays 1),
// and the require.Equal(2, ...) below FAILs.
func TestBug_Reconcile_FailedUnsubscribe_RetriedNotNoOped(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "retry-unsub",
	}, connectivity.SessionEphemeral, nil)

	fake := &failThenOKUnsubConn{}
	// Model an already-applied plan wanting topic A.
	sess.mu.Lock()
	sess.cm = fake
	sess.plan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "A", QoS: 1}},
	}
	sess.appliedPlan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "A", QoS: 1}},
	}
	sess.activeSubs = map[string]byte{"A": 1}
	sess.mu.Unlock()

	// First empty reconcile: Unsubscribe(A) FAILS. The reconcile returns the
	// error and MUST NOT advance applied history.
	err := sess.Reconcile(context.Background(), connectivity.SessionPlan{})
	require.Error(t, err, "a failed Unsubscribe must surface as a reconcile error")
	require.Equal(t, 1, fake.calls(), "the first reconcile attempted the unsubscribe once")

	sess.mu.Lock()
	stillApplied := sess.appliedPlan != nil && len(sess.appliedPlan.Subscriptions) == 1
	stillActive := len(sess.activeSubs) == 1
	sess.mu.Unlock()
	require.True(t, stillApplied,
		"a FAILED reconcile must NOT commit the empty plan as applied history (blocking-#2)")
	require.True(t, stillActive,
		"a failed Unsubscribe must not delete the topic from activeSubs")

	// Second empty reconcile: because applied history still holds A, this must
	// RETRY the unsubscribe (NOT no-op) — and it succeeds this time.
	require.NoError(t, sess.Reconcile(context.Background(), connectivity.SessionPlan{}))
	require.Equal(t, 2, fake.calls(),
		"the next reconcile must RETRY the failed unsubscribe, not short-circuit as a no-op")

	// Applied state finally advances to the (empty) plan and activeSubs is clear.
	sess.mu.Lock()
	defer sess.mu.Unlock()
	require.NotNil(t, sess.appliedPlan)
	require.Len(t, sess.appliedPlan.Subscriptions, 0,
		"applied history advances to the empty plan only after the unsubscribe SUCCEEDS")
	require.Len(t, sess.activeSubs, 0, "activeSubs is cleared once the unsubscribe succeeds")
}

func TestReconcile_RetainedPlansDoNotAliasCallerSlices(t *testing.T) {
	fake := &fakeReconcileConn{}
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "plan-copy",
	}, connectivity.SessionEphemeral, nil)
	sess.mu.Lock()
	sess.cm = fake
	sess.mu.Unlock()
	plan := connectivity.SessionPlan{
		Subscriptions:       []connectivity.SubscriptionPlan{{Topic: "original/sub", QoS: 1}},
		Publishers:          []connectivity.PublisherPlan{{Topic: "original/pub", QoS: 1}},
		ExpectedReceiverIDs: []string{"original-rx"},
	}

	require.NoError(t, sess.Reconcile(context.Background(), plan))

	plan.Subscriptions[0].Topic = "mutated/sub"
	plan.Publishers[0].Topic = "mutated/pub"
	plan.ExpectedReceiverIDs[0] = "mutated-rx"

	sess.mu.Lock()
	desired := *sess.plan
	applied := *sess.appliedPlan
	sess.mu.Unlock()
	for name, retained := range map[string]connectivity.SessionPlan{
		"desired": desired,
		"applied": applied,
	} {
		assert.Equal(t, "original/sub", retained.Subscriptions[0].Topic, name)
		assert.Equal(t, "original/pub", retained.Publishers[0].Topic, name)
		assert.Equal(t, "original-rx", retained.ExpectedReceiverIDs[0], name)
	}
}
