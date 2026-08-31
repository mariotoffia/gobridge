package paho

import (
	"log/slog"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// The dispatch budget (queueReserved, sized to Receive Maximum) is the single
// ownership counter shared by the serialized dispatch queue and the
// pre-registration pending buffer. Two properties must hold for ingress to stay
// alive:
//
//  1. Every branch that refuses or drops a publish RELEASES its reservation.
//     A branch that drops without releasing leaks capacity permanently: after
//     receive_maximum such drops nothing can be admitted again and KeepAlive
//     dies with the process still "connected".
//  2. A QoS 1/2 publish is never parked behind grace-buffered QoS 0. QoS 0
//     entries hold reservations but are excluded from the broker's in-flight
//     window, so a budget saturated by QoS 0 would block Paho's single publish
//     callback goroutine — which also reads PINGRESP — instead of shedding the
//     traffic that carries no delivery contract.

// TestRouterQueueBudget_CoveredQoS0DropReturnsCapacity drives the
// covered-topic QoS 0 refusal through enqueueDispatch (the production entry
// point) and asserts the dispatch budget is fully reusable afterwards.
//
// Counterfactual (the pre-fix leak): retainCovered's covered QoS 0
// buffer-refusal branch dropped the publish WITHOUT releasing its reservation,
// so after dispatchSize drops queueReserved stayed pinned at the ceiling — the
// third publish was refused at the budget gate (counted as a generic drop) and
// no later publish could ever be admitted. CoveredDroppedCount is then 2, not
// 3, and queueReserved is 2, not 0.
func TestRouterQueueBudget_CoveredQoS0DropReturnsCapacity(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec,
		withRouterClock(clk),
		withUnmatchedGrace(testGrace),
		withDispatchCapacity(2),
		withCovered(func(string) bool { return true }),
	)
	t.Cleanup(r.shutdown)

	// A byte ceiling below any real payload makes every covered QoS 0 publish
	// hit the buffer refusal that retainCovered has to release.
	r.mu.Lock()
	r.pendingBytesLimit = 1
	r.mu.Unlock()

	clk.Advance(testGrace + time.Second) // past grace: unmatched publishes are settled, not buffered

	for range 3 {
		r.enqueueDispatch(&pahov5.Publish{Topic: "live/route", QoS: 0, Payload: []byte("payload")}, nil)
	}

	require.Equal(t, int64(3), r.CoveredDroppedCount(),
		"every covered QoS 0 the buffer refused is a covered drop; a leaked reservation would refuse the third at the budget gate instead")
	require.Len(t, rec.FindEntries(MetricMQTTRouterCoveredDropped), 3)
	assertRouterReservations(t, r, 0)

	// The budget is genuinely reusable: a fresh QoS 1 publish is admitted.
	admitted := &pahov5.Publish{Topic: "live/route", QoS: 1, Payload: []byte("later")}
	require.True(t, r.reserveQueueSlot(admitted, admitted.QoS),
		"dispatch capacity released by the covered QoS 0 drops must be reusable")
}

// TestRouterQueueBudget_QoS1EvictsGraceBufferedQoS0 pins the admission policy
// for a QoS 1/2 publish arriving while the whole dispatch budget is held by
// grace-buffered QoS 0: the oldest QoS 0 is evicted (best-effort, no delivery
// contract) and the QoS 1/2 is admitted immediately.
//
// Counterfactual (the pre-fix park): reserveQueueSlot waited on queueChanged
// for a release that could only come from handler progress. With no handler
// registered for the buffered topics, Paho's single routePublishPackets
// goroutine parked forever inside the callback: PINGRESP stopped being read,
// keepalive killed the connection, and because the callback never returned
// autopaho saw neither client shutdown nor OnConnectionDown. The goroutine
// running enqueueDispatch never completes and the wait below fails.
func TestRouterQueueBudget_QoS1EvictsGraceBufferedQoS0(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec,
		withRouterClock(clk),
		withUnmatchedGrace(testGrace),
		withDispatchCapacity(2),
	)
	t.Cleanup(r.shutdown)
	r.setPendingLimit(2)

	// Two QoS 0 publishes inside the grace window with no handler registered:
	// both are buffered and each pins a reservation.
	first := &pahov5.Publish{Topic: "grace/one", QoS: 0, Payload: []byte("a")}
	second := &pahov5.Publish{Topic: "grace/two", QoS: 0, Payload: []byte("b")}
	r.enqueueDispatch(first, nil)
	r.enqueueDispatch(second, nil)
	require.Equal(t, 2, r.PendingCount())
	assertRouterReservations(t, r, 2)

	admitted := make(chan struct{})
	go func() {
		defer close(admitted)
		r.enqueueDispatch(&pahov5.Publish{Topic: "grace/three", QoS: 1, Payload: []byte("c")}, nil)
	}()

	wait.Until(t, 2*time.Second, "QoS 1 admitted without parking the publish callback", func() bool {
		select {
		case <-admitted:
			return true
		default:
			return false
		}
	})

	require.Equal(t, int64(1), r.dropCount.Load(), "exactly one QoS 0 is shed to make room")
	require.Len(t, rec.FindEntries(MetricMQTTRouterDropped), 1)

	r.mu.RLock()
	topics := make([]string, 0, len(r.pending))
	for i := range r.pending {
		topics = append(topics, r.pending[i].pub.Topic)
	}
	r.mu.RUnlock()
	require.Equal(t, []string{"grace/two", "grace/three"}, topics,
		"the OLDEST QoS 0 is evicted; the QoS 1/2 publish is buffered in arrival order behind the survivor")
}

// TestRouterQueueBudget_QoS0RefusedOnExhaustedBudgetIsAttributed pins the drop
// attribution: a QoS 0 refused because the dispatch budget is exhausted is
// reported as a budget refusal, not as "pending buffer full during startup
// grace" — the two have different operator remedies (a stalled receiver versus
// an undersized receive_maximum).
func TestRouterQueueBudget_QoS0RefusedOnExhaustedBudgetIsAttributed(t *testing.T) {
	logs := &recordingLogHandler{}
	rec := &ports.RecordingExporter{}
	r := newRouter(slog.New(logs), rec, withDispatchCapacity(1))
	t.Cleanup(r.shutdown)

	held := &pahov5.Publish{Topic: "held", QoS: 1}
	require.True(t, r.reserveQueueSlot(held, held.QoS))

	r.enqueueDispatch(&pahov5.Publish{Topic: "shed", QoS: 0}, nil)

	require.Equal(t, int64(1), r.dropCount.Load())
	require.Len(t, rec.FindEntries(MetricMQTTRouterDropped), 1)
	require.Equal(t, 1, logs.warnCountContaining("dispatch budget exhausted"),
		"a budget refusal must name the dispatch budget, not the startup grace buffer")
	require.Zero(t, logs.warnCountContaining("pending buffer full during startup grace"))
}

// TestRouterQueueBudget_ClosingRefusalIsMetered pins that ingress refused while
// the session is closing is never silent. Close stops the router BEFORE it
// disconnects (otherwise a parked callback pins the SDK teardown for the whole
// deadline), so publishes keep arriving for the length of the disconnect and
// are released un-acked. QoS 1/2 is redelivered on session resume; QoS 0 has no
// redelivery and is a loss. Both are counted, and neither is attributed to a
// budget that is not actually full.
func TestRouterQueueBudget_ClosingRefusalIsMetered(t *testing.T) {
	rec := &ports.RecordingExporter{}
	logs := &recordingLogHandler{}
	r := newRouter(slog.New(logs), rec, withDispatchCapacity(8))
	r.shutdown()

	r.enqueueDispatch(&pahov5.Publish{Topic: "closing/durable", QoS: 1}, nil)
	r.enqueueDispatch(&pahov5.Publish{Topic: "closing/besteffort", QoS: 0}, nil)

	require.Equal(t, int64(1), r.StalePurgedCount(),
		"a QoS 1/2 released at close is redelivered on resume, and must still be counted")
	require.Len(t, rec.FindEntries(MetricMQTTRouterStalePurged), 1)
	require.Equal(t, int64(1), r.dropCount.Load(), "a QoS 0 released at close is a loss")
	require.Len(t, rec.FindEntries(MetricMQTTRouterDropped), 1)
	require.Equal(t, 1, logs.warnCountContaining("session is closing"))
	require.Zero(t, logs.warnCountContaining("dispatch budget exhausted"),
		"a closing session must not be reported as an undersized receive_maximum")
}

// TestRouterQueueBudget_EvictedCoveredQoS0KeepsItsCoveredAttribution pins that
// reclaiming a slot from a COVERED QoS 0 — one retained past the grace window
// for a receiver that registered late — is reported on the covered-drop metric.
// It is the signal an operator wires for "a live route is losing messages
// because its handler is slow to register", and folding it into the generic
// backpressure counter would silence exactly that alert.
func TestRouterQueueBudget_EvictedCoveredQoS0KeepsItsCoveredAttribution(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec,
		withRouterClock(clk),
		withUnmatchedGrace(testGrace),
		withDispatchCapacity(1),
		withCovered(func(string) bool { return true }),
	)
	t.Cleanup(r.shutdown)
	r.setPendingLimit(2)

	clk.Advance(testGrace + time.Second) // past grace: an unmatched publish is retained as covered
	r.enqueueDispatch(&pahov5.Publish{Topic: "live/route", QoS: 0, Payload: []byte("covered")}, nil)
	require.Equal(t, int64(1), r.CoveredRetainedCount())
	assertRouterReservations(t, r, 1)

	// A QoS 1/2 arrival must reclaim the budget rather than park.
	r.enqueueDispatch(&pahov5.Publish{Topic: "live/route", QoS: 1, Payload: []byte("durable")}, nil)

	require.Equal(t, int64(1), r.CoveredDroppedCount(),
		"the reclaimed entry was a covered retention, so it is a covered loss")
	require.Len(t, rec.FindEntries(MetricMQTTRouterCoveredDropped), 1)
	require.Empty(t, rec.FindEntries(MetricMQTTRouterDropped),
		"a covered loss must not be reported as generic backpressure")
}
