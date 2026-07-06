package paho

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// Orphan broker-side subscription → whole-session ingress stall (HIGH).
//
// clean_start=false resumes the persistent broker session, so a route
// removed from config leaves an orphan subscription the broker keeps
// delivering QoS 1/2 publishes for. Under manual acknowledgment such a
// publish, buffered forever and never acked, pins the broker's
// Receive-Maximum in-flight window and (MQTT's in-order ack rule)
// head-of-line-blocks acks for every later message on the shared session.
//
// Fix: after a bounded startup grace window (deterministic via the
// injected clock), an unmatched publish is acked-and-dropped and its exact
// topic unsubscribed. During grace the legitimate CONNACK-backlog window is
// preserved (buffer + late-flush).
// ═══════════════════════════════════════════════════════════════════════════

const testGrace = 30 * time.Second

// startAt is the fixed epoch the fake clocks start from so the tests read
// deterministically.
func testClock() *clocktest.Fake { return clocktest.NewAt(time.Unix(1_700_000_000, 0)) }

// (a) An unmatched publish arriving WITHIN the grace window is buffered
// un-acked — the pre-fix behaviour for the legitimate startup backlog.
func TestOrphan_UnmatchedWithinGrace_Buffered(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk), withUnmatchedGrace(testGrace))
	defer r.shutdown()

	var acked atomic.Int32
	r.dispatch(&pahov5.Publish{Topic: "orphan/x", QoS: 1, Payload: []byte("p")},
		func() error { acked.Add(1); return nil })

	require.Equal(t, 1, r.PendingCount(), "within grace the unmatched publish is buffered")
	require.Equal(t, int32(0), acked.Load(), "buffered publish stays un-acked (broker redelivers on crash)")
	require.Len(t, rec.FindEntries(MetricMQTTRouterBuffered), 1, "buffered metric emitted")
	require.Empty(t, rec.FindEntries(MetricMQTTRouterUnmatchedDropped), "no orphan drop within grace")
	require.Equal(t, int64(0), r.UnmatchedDroppedCount())
}

// (b) An unmatched publish arriving AFTER grace is acked and dropped, its
// counter incremented, and a later MATCHED message still acks fine — the
// orphan no longer head-of-line-blocks the session.
func TestOrphan_UnmatchedAfterGrace_AckedDroppedNoHeadOfLineBlock(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk), withUnmatchedGrace(testGrace))
	defer r.shutdown()

	clk.Advance(testGrace + time.Second) // past the grace window

	var orphanAcked atomic.Int32
	r.dispatch(&pahov5.Publish{Topic: "orphan/x", QoS: 1, Payload: []byte("p")},
		func() error { orphanAcked.Add(1); return nil })

	require.Equal(t, int32(1), orphanAcked.Load(),
		"orphan publish is acked past grace (frees the broker in-flight slot)")
	require.Equal(t, 0, r.PendingCount(), "orphan publish is dropped, not buffered")
	require.Equal(t, int64(1), r.UnmatchedDroppedCount())
	require.Len(t, rec.FindEntries(MetricMQTTRouterUnmatchedDropped), 1)

	// A later matched message still settles — proving no head-of-line block
	// was left behind by the dropped orphan.
	got := make(chan struct{}, 1)
	var matchedAcked atomic.Int32
	r.RegisterFiltered("rx", []string{"sensors/#"}, func(_ *pahov5.Publish, ack func() error) {
		_ = ack()
		got <- struct{}{}
	})
	r.dispatch(&pahov5.Publish{Topic: "sensors/temp", QoS: 1, Payload: []byte("21")},
		func() error { matchedAcked.Add(1); return nil })
	<-got
	r.Wait()
	require.Equal(t, int32(1), matchedAcked.Load(),
		"matched message acks fine after the orphan drop (no head-of-line blocking)")
}

// (c) A post-grace orphan topic triggers exactly one UNSUBSCRIBE for that
// topic, deduped across repeats, while every publish is still acked-dropped.
func TestOrphan_PostGraceTopic_SingleUnsubscribeDeduped(t *testing.T) {
	clk := testClock()
	var mu sync.Mutex
	var unsubs []string
	r := newRouter(nil, nil,
		withRouterClock(clk),
		withUnmatchedGrace(testGrace),
		withUnsubscribe(func(topic string) {
			mu.Lock()
			unsubs = append(unsubs, topic)
			mu.Unlock()
		}),
	)
	defer r.shutdown()

	r.beginGrace() // connection up: starts the grace worker
	clk.Advance(testGrace + time.Second)

	for i := 0; i < 3; i++ {
		r.dispatch(&pahov5.Publish{Topic: "orphan/x", QoS: 1, Payload: []byte("p")},
			func() error { return nil })
	}

	// Ack-and-drop is synchronous in dispatch; the UNSUBSCRIBE is handled
	// by the single grace worker, so await it deterministically.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(unsubs) == 1
	}, time.Second, time.Millisecond, "orphan topic must be unsubscribed")

	require.Equal(t, int64(3), r.UnmatchedDroppedCount(),
		"every orphan publish is acked-and-dropped even though unsubscribe is deduped")

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"orphan/x"}, unsubs,
		"orphan topic unsubscribed exactly once despite repeated publishes (dedup)")
}

// (d) A handler registered LATE but still WITHIN grace receives the buffered
// messages — the legitimate startup path is not regressed by the fix.
func TestOrphan_LateHandlerWithinGrace_ReceivesBuffered(t *testing.T) {
	clk := testClock()
	r := newRouter(nil, nil, withRouterClock(clk), withUnmatchedGrace(testGrace))
	defer r.shutdown()

	var acked atomic.Int32
	r.dispatch(&pahov5.Publish{Topic: "sensors/temp", QoS: 1, Payload: []byte("21")},
		func() error { acked.Add(1); return nil })
	require.Equal(t, 1, r.PendingCount())

	clk.Advance(testGrace / 3) // time passes but stays within grace

	got := make(chan string, 1)
	r.RegisterFiltered("rx", []string{"sensors/#"}, func(pub *pahov5.Publish, ack func() error) {
		_ = ack()
		got <- pub.Topic
	})
	require.Equal(t, "sensors/temp", <-got, "buffered message flushed to the late in-grace handler")
	r.Wait()
	require.Equal(t, int32(1), acked.Load(), "flushed message settles the original protocol ack")
	require.Equal(t, int64(0), r.UnmatchedDroppedCount(), "no orphan drop on the legitimate late-handler path")
}

// (e) The grace timer proactively sweeps orphans buffered DURING grace even
// when the broker has stopped delivering (its in-flight window is now full
// of un-acked orphans and no further dispatch arrives). This is the actual
// stall-break: acking the buffered orphans frees the window.
func TestOrphan_GraceTimerSweep_AcksBufferedOrphans(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	var mu sync.Mutex
	var unsubs []string
	r := newRouter(nil, rec,
		withRouterClock(clk),
		withUnmatchedGrace(testGrace),
		withUnsubscribe(func(topic string) {
			mu.Lock()
			unsubs = append(unsubs, topic)
			mu.Unlock()
		}),
	)
	defer r.shutdown()

	// Connection up: arms the grace window + sweep worker.
	r.beginGrace()
	require.Equal(t, 1, clk.TimerCount(), "beginGrace arms exactly one grace timer")

	// Broker delivers the orphan backlog during grace: buffered, un-acked.
	var acked atomic.Int32
	for i := 0; i < 3; i++ {
		r.dispatch(&pahov5.Publish{Topic: "orphan/x", QoS: 1, Payload: []byte("p")},
			func() error { acked.Add(1); return nil })
	}
	require.Equal(t, 3, r.PendingCount(), "orphan backlog buffered during grace")
	require.Equal(t, int32(0), acked.Load(), "buffered orphans un-acked during grace")

	// Broker's in-flight window is full; delivery stops (no more dispatch).
	// The grace timer must still fire and sweep.
	clk.Advance(testGrace)

	// The sweep acks all orphans and only THEN queues the unsubscribe, so
	// observing the unsubscribe implies the sweep completed.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(unsubs) == 1
	}, time.Second, time.Millisecond, "grace timer sweep must drain and unsubscribe buffered orphans")

	require.Equal(t, int32(3), acked.Load(),
		"swept orphans are acked, freeing the broker in-flight window (stall broken)")
	require.Equal(t, 0, r.PendingCount(), "buffered orphans drained by the sweep")
	require.Equal(t, int64(3), r.UnmatchedDroppedCount())
	require.Len(t, rec.FindEntries(MetricMQTTRouterUnmatchedDropped), 3)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"orphan/x"}, unsubs, "swept orphan topic unsubscribed once")
}

// The grace window RESTARTS per connection: a reconnect re-buffers the fresh
// backlog rather than judging it against a long-expired deadline.
func TestOrphan_GraceRestartsPerConnection(t *testing.T) {
	clk := testClock()
	// swept signals the exact topic each grace sweep unsubscribes, giving a
	// deterministic "the first window's async sweep has run" edge — without
	// it the first timer fire can drain LATE and eat the second window's
	// freshly-buffered backlog (a test-only compressed-timeline race; in
	// production the grace worker consumes the fire long before a reconnect).
	swept := make(chan string, 1)
	r := newRouter(nil, nil,
		withRouterClock(clk),
		withUnmatchedGrace(testGrace),
		withUnsubscribe(func(topic string) { swept <- topic }),
	)
	defer r.shutdown()

	r.beginGrace() // first connection up

	// Seed an orphan under window 1 so its expiry sweep is observable.
	r.dispatch(&pahov5.Publish{Topic: "orphan/first", QoS: 1, Payload: []byte("x")},
		func() error { return nil })
	require.Equal(t, 1, r.PendingCount())

	clk.Advance(testGrace + time.Second) // fire window 1 → sweep drops orphan/first

	// Block until the window-1 sweep has fully run (it unsubscribes the exact
	// orphan topic from the grace worker AFTER dropping it). This drains the
	// timer fire so no stale sweep survives into the second window.
	select {
	case topic := <-swept:
		require.Equal(t, "orphan/first", topic)
	case <-time.After(3 * time.Second):
		t.Fatal("window-1 grace sweep never ran")
	}
	require.Equal(t, int64(1), r.UnmatchedDroppedCount(), "only the window-1 orphan is dropped")

	// Second connection up: a fresh window must begin from "now".
	r.beginGrace()
	require.Equal(t, 1, clk.TimerCount(), "the single grace timer is re-armed, not duplicated")

	var acked atomic.Int32
	r.dispatch(&pahov5.Publish{Topic: "sensors/temp", QoS: 1, Payload: []byte("21")},
		func() error { acked.Add(1); return nil })

	require.Equal(t, 1, r.PendingCount(),
		"the reconnect backlog is buffered under the fresh grace window, not dropped as orphan")
	require.Equal(t, int32(0), acked.Load())
	require.Equal(t, int64(1), r.UnmatchedDroppedCount(),
		"the fresh-window backlog is not swept (only the window-1 orphan was dropped)")
}

// recordingUnsubConn is a pahoConnection double that records the topics
// passed to Unsubscribe so the session-level wiring can be asserted.
type recordingUnsubConn struct {
	mu     sync.Mutex
	topics []string
}

func (c *recordingUnsubConn) AwaitConnection(context.Context) error { return nil }
func (c *recordingUnsubConn) Disconnect(context.Context) error      { return nil }
func (c *recordingUnsubConn) Subscribe(context.Context, []subscribeSpec) ([]byte, error) {
	return nil, nil
}

func (c *recordingUnsubConn) Unsubscribe(_ context.Context, topics []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.topics = append(c.topics, topics...)
	return nil
}

func (c *recordingUnsubConn) PublishEnvelope(
	context.Context, *messaging.Envelope, string, SenderOptions, clock.Clock,
) (publishResult, error) {
	return publishResult{}, nil
}
func (c *recordingUnsubConn) Underlying() *autopaho.ConnectionManager { return nil }

func (c *recordingUnsubConn) unsubscribed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.topics))
	copy(out, c.topics)
	return out
}

var _ pahoConnection = (*recordingUnsubConn)(nil)

// End-to-end (session) wiring: handleConnectionUp starts the grace window
// on the router, and a post-grace orphan publish routes through
// Session.unsubscribeOrphan to cm.Unsubscribe with the exact orphan topic.
func TestOrphan_SessionWiring_UnsubscribesExactTopic(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	sess := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
		ClientID:       "orphan-wiring",
		UnmatchedGrace: testGrace,
		Clock:          clk,
	}, connectivity.SessionPersistent, nil, rec)

	fake := &recordingUnsubConn{}
	sess.mu.Lock()
	sess.cm = fake
	sess.mu.Unlock()

	// Simulate a (re)connect: this arms the router grace window.
	sess.handleConnectionUp()
	require.Equal(t, 1, clk.TimerCount(), "connection up arms the grace timer")

	// Past the window an unmatched publish is an orphan.
	clk.Advance(testGrace + time.Second)
	sess.Router().dispatch(&pahov5.Publish{Topic: "removed/route/1", QoS: 1, Payload: []byte("x")},
		func() error { return nil })

	require.Eventually(t, func() bool {
		return len(fake.unsubscribed()) == 1
	}, time.Second, time.Millisecond, "the orphan's exact topic must be unsubscribed")

	require.Equal(t, []string{"removed/route/1"}, fake.unsubscribed(),
		"the orphan's exact topic is unsubscribed to converge broker state")
	require.Len(t, rec.FindEntries(MetricMQTTRouterUnmatchedDropped), 1)

	require.NoError(t, sess.Close(context.Background()))
}

// topicCoveredLocked is the guard that stops the orphan-unsubscribe from
// killing a legitimate route whose handler simply registers late. It must
// match a concrete publish topic against the FILTERS the session still
// wants (active broker subs + plan), including wildcards — so wildcard
// coverage is by design, not by accident.
func TestSession_TopicCoveredLocked(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "cov",
	}, connectivity.SessionPersistent, nil)

	sess.mu.Lock()
	defer sess.mu.Unlock()

	// Nothing configured yet: every unmatched topic is a genuine orphan.
	require.False(t, sess.topicCoveredLocked("a/b"))

	// Active broker subscription (wildcard) covers a concrete topic.
	sess.activeSubs["sensors/+/temp"] = 1
	require.True(t, sess.topicCoveredLocked("sensors/kitchen/temp"),
		"a wildcard active subscription covers the concrete topic")
	require.False(t, sess.topicCoveredLocked("actuators/kitchen/set"))

	// Desired plan filter (wildcard) covers even before it is re-subscribed
	// (activeSubs is reset on every reconnect; the plan is not).
	sess.plan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "actuators/#", QoS: 1}},
	}
	require.True(t, sess.topicCoveredLocked("actuators/kitchen/set"),
		"a desired plan filter covers the concrete topic")
	require.False(t, sess.topicCoveredLocked("removed/route/1"),
		"a topic covered by neither active subs nor the plan is a true orphan")
}

// NEW-DEFECT regression: a topic COVERED by a still-desired subscription
// whose handler registers later than the grace window must be
// acked-and-dropped (unavoidable, the broker already acked) but NEVER
// unsubscribed — unsubscribing would silently kill a live route until the
// next reconcile. A genuine orphan on a different topic is still
// unsubscribed. The single FIFO unsubscribe worker orders the covered topic
// before the orphan, giving a deterministic "already handled" barrier.
func TestOrphan_CoveredTopicNotUnsubscribed_TrueOrphanStillUnsubscribed(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	sess := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
		ClientID:       "covered-guard",
		UnmatchedGrace: testGrace,
		Clock:          clk,
	}, connectivity.SessionPersistent, nil, rec)

	fake := &recordingUnsubConn{}
	sess.mu.Lock()
	sess.cm = fake
	// The desired plan still wants sensors/temp; its receiver has just not
	// registered its handler on the router yet (Start→Reconcile→Run lag).
	sess.plan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "sensors/temp", QoS: 1}},
	}
	sess.mu.Unlock()

	sess.handleConnectionUp() // arms grace (also resets activeSubs)
	clk.Advance(testGrace + time.Second)

	// Covered topic first, then a genuine orphan on a different topic. The
	// single FIFO grace worker processes the covered one (skip) before the
	// orphan (unsubscribe), so the orphan appearing is proof the covered
	// topic was already handled.
	sess.Router().dispatch(&pahov5.Publish{Topic: "sensors/temp", QoS: 1, Payload: []byte("21")},
		func() error { return nil })
	sess.Router().dispatch(&pahov5.Publish{Topic: "removed/route/1", QoS: 1, Payload: []byte("x")},
		func() error { return nil })

	require.Eventually(t, func() bool {
		for _, tp := range fake.unsubscribed() {
			if tp == "removed/route/1" {
				return true
			}
		}
		return false
	}, time.Second, time.Millisecond, "the true orphan must be unsubscribed")

	require.Equal(t, []string{"removed/route/1"}, fake.unsubscribed(),
		"the covered topic must NOT be unsubscribed; only the true orphan is")
	require.Equal(t, int64(2), sess.Router().UnmatchedDroppedCount(),
		"both publishes are acked-and-dropped (the covered one unrecoverably, per protocol)")

	// The still-live route works once its handler registers: subsequent
	// publishes are delivered rather than dropped — proof it was not killed.
	got := make(chan string, 1)
	sess.Router().RegisterFiltered("rx", []string{"sensors/temp"},
		func(pub *pahov5.Publish, ack func() error) { _ = ack(); got <- pub.Topic })
	sess.Router().dispatch(&pahov5.Publish{Topic: "sensors/temp", QoS: 1, Payload: []byte("22")},
		func() error { return nil })
	require.Equal(t, "sensors/temp", <-got,
		"the covered route still delivers after the handler registers (route not killed)")

	require.NoError(t, sess.Close(context.Background()))
}
