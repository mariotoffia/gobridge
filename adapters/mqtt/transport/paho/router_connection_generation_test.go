package paho

import (
	"context"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// A connection generation is defined by the Paho CLIENT that delivered a
// packet, not by the callback that announces the connection.
//
// Paho starts its incoming/routePublishPackets goroutines inside Client.Connect,
// while autopaho invokes OnConnectionUp only after establishServerConnection
// RETURNS. A broker that replays a queued QoS 1/2 backlog immediately after
// CONNACK therefore delivers its first publishes BEFORE beginGrace runs. Those
// publishes belong to the LIVE client and their acknowledgements are live, so
// nothing may purge them as stale, and nothing may erase their unsettled
// bookkeeping.
//
// Because autopaho only builds a replacement client after the previous one has
// fully shut down (mainLoop waits on <-cli.Done(), and an explicit recycle
// awaits ConnectionManager.Disconnect), the FIRST packet from a client the
// router has not seen before is itself proof the previous generation is dead —
// which is what makes it safe to advance the generation there.

// TestRouterGeneration_PublishFromNewClientBeforeGraceSurvives covers the
// autopaho auto-reconnect edge: the replacement client's backlog lands before
// OnConnectionUp.
//
// Counterfactual (the pre-fix purge): the publish was buffered stamped with the
// PREVIOUS epoch, then beginGrace bumped connEpoch and purgeStalePendingLocked
// discarded it un-acked while its acknowledgement belonged to the live client.
// The un-acked packet sat at the head of Paho's contiguous-prefix ack tracker,
// so every later Delivery.Ack on the session reported success while no PUBACK
// was written, and after receive_maximum such packets QoS 1/2 ingress was dead.
// PendingCount is then 0 and StalePurgedCount 1.
func TestRouterGeneration_PublishFromNewClientBeforeGraceSurvives(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk), withUnmatchedGrace(testGrace), withDispatchCapacity(4))
	t.Cleanup(r.shutdown)

	previous := &pahov5.Client{}
	replacement := &pahov5.Client{}

	// Connection 1 comes up and delivers one publish that no handler covers yet.
	r.beginGrace()
	deliver(t, r, previous, &pahov5.Publish{Topic: "backlog/one", QoS: 1, Payload: []byte("stale")})
	waitPendingCount(t, r, 1)

	// Connection 1 drops. autopaho raises its connection-down edge only after
	// that client's workers returned, and builds the replacement afterwards.
	r.noteConnectionTornDown()

	// The replacement client's CONNACK backlog arrives BEFORE autopaho gets
	// around to OnConnectionUp.
	deliver(t, r, replacement, &pahov5.Publish{Topic: "backlog/two", QoS: 1, Payload: []byte("live")})
	waitPendingTopics(t, r, "backlog/two")

	require.Equal(t, int64(1), r.StalePurgedCount(),
		"only the PREVIOUS client's entry is purged")

	// autopaho finally reports the connection up.
	r.beginGrace()

	require.Equal(t, []string{"backlog/two"}, pendingTopics(r),
		"the live client's publish must survive the connection-up callback that follows it")
	require.Equal(t, int64(1), r.StalePurgedCount(),
		"beginGrace must not purge a generation the live client already opened")
	require.Equal(t, 1, r.unsettledSnapshot(10).Count,
		"the live client's un-acked packet must stay visible to receive-window health")
}

// TestRouterGeneration_PublishFromNewClientDuringRecycleIsNotDiscarded covers
// the settlement-recovery recycle edge: quiesceForRecycle marks old-socket
// ingress discard-only, and the REPLACEMENT connection's first packet arrives
// before beginGrace clears that mark.
//
// Counterfactual (the pre-fix discard): enqueueDispatch saw discarding==true and
// dropped the publish un-acked on the stale-purge metric — stranding the very
// redelivery the recycle was performed to obtain.
func TestRouterGeneration_PublishFromNewClientDuringRecycleIsNotDiscarded(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk), withUnmatchedGrace(testGrace), withDispatchCapacity(4))
	t.Cleanup(r.shutdown)

	previous := &pahov5.Client{}
	replacement := &pahov5.Client{}

	r.beginGrace()
	deliver(t, r, previous, &pahov5.Publish{Topic: "recycle/old", QoS: 1})
	waitPendingCount(t, r, 1)

	require.NoError(t, r.quiesceForRecycle(context.Background(), context.Background(), nil))
	require.Equal(t, int64(1), r.StalePurgedCount(), "the old generation's buffered entry is purged by the recycle")

	// The recycle disconnects the old generation, then dials a replacement whose
	// redelivery lands before OnConnectionUp.
	r.noteConnectionTornDown()
	deliver(t, r, replacement, &pahov5.Publish{Topic: "recycle/new", QoS: 1})
	waitPendingTopics(t, r, "recycle/new")
	require.Equal(t, int64(1), r.StalePurgedCount(),
		"the replacement client's redelivery must not be discarded as old-socket ingress")

	r.beginGrace()
	require.Equal(t, []string{"recycle/new"}, pendingTopics(r))

	// The managed resume drains it to the handler that finally registered.
	delivered := make(chan string, 1)
	r.RegisterFiltered("rx", []string{"recycle/#"}, func(pub *pahov5.Publish, _ func() error) {
		delivered <- pub.Topic
	})
	require.NoError(t, r.resumeManagedDispatch(context.Background()))
	require.Equal(t, "recycle/new", <-delivered)
}

// TestRouterGeneration_SupersededClientCannotReopenAGeneration covers two Paho
// clients being alive at once, which happens whenever a ConnectionManager's
// Disconnect times out while the replacement is already connecting: the
// abandoned client keeps delivering into the same router.
//
// Only ONE generation may be opened per reported teardown. Without that rule a
// client pointer that merely differs from the last one seen opens a generation,
// so traffic alternating between the two live clients purges the other's
// entries un-acked on a connection that is still up — the ghosts then sit at
// the head of Paho's contiguous-prefix ack stream forever.
func TestRouterGeneration_SupersededClientCannotReopenAGeneration(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk), withUnmatchedGrace(testGrace), withDispatchCapacity(8))
	t.Cleanup(r.shutdown)

	abandoned := &pahov5.Client{}
	live := &pahov5.Client{}

	r.beginGrace()
	r.noteConnectionTornDown()
	deliver(t, r, live, &pahov5.Publish{Topic: "live/one", QoS: 1})
	waitPendingTopics(t, r, "live/one")

	// The abandoned manager's client is still delivering.
	deliver(t, r, abandoned, &pahov5.Publish{Topic: "abandoned/one", QoS: 1})
	deliver(t, r, live, &pahov5.Publish{Topic: "live/two", QoS: 1})

	waitPendingTopics(t, r, "live/one", "abandoned/one", "live/two")
	require.Zero(t, r.StalePurgedCount(),
		"a superseded client's straggler must not purge the live connection's buffered traffic")

	// The straggler must also not displace the live client: the live
	// connection's own settlements would then be read as landing on a dead
	// socket, reported as guaranteed redeliveries that never happen.
	require.False(t, r.liveConnectionSuperseded(live))
	require.True(t, r.liveConnectionSuperseded(abandoned))
}

// TestRouterGeneration_IdleConnectionDoesNotStrandItsMarker covers a connection
// that comes up and delivers NOTHING before dropping. Its connection-up
// callback opened a generation no client ever claimed, and the marker must not
// survive to be consumed by the NEXT connection's first packet — that would
// leave the replacement's live backlog stamped with the dead generation, for
// the following connection-up callback to purge un-acked.
func TestRouterGeneration_IdleConnectionDoesNotStrandItsMarker(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk), withUnmatchedGrace(testGrace), withDispatchCapacity(4))
	t.Cleanup(r.shutdown)

	r.beginGrace()             // connection 1 up; it delivers nothing
	r.noteConnectionTornDown() // connection 1 drops

	// Connection 2's CONNACK backlog beats its connection-up callback.
	deliver(t, r, &pahov5.Client{}, &pahov5.Publish{Topic: "backlog/live", QoS: 1})
	waitPendingTopics(t, r, "backlog/live")

	r.beginGrace()
	require.Equal(t, []string{"backlog/live"}, pendingTopics(r),
		"an idle connection's unclaimed marker must not make the next connection's backlog look stale")
	require.Zero(t, r.StalePurgedCount())
	require.Equal(t, 1, r.unsettledSnapshot(10).Count)
}

// TestRouterGeneration_TeardownVoidsAnUnconsumedClientMarker pins that a
// connection-up callback which never runs cannot leave the marker behind for a
// LATER connection to consume.
//
// The callback is skipped whenever the session rejects the connection edge — a
// latched terminal error, Session Present evidence a recovery demanded, or a
// generation a reload already discarded. Its client may still have opened the
// generation. The teardown report voids that marker, so the next connection's
// callback advances and purges the dead entries instead of inheriting them.
func TestRouterGeneration_TeardownVoidsAnUnconsumedClientMarker(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk), withUnmatchedGrace(testGrace), withDispatchCapacity(4))
	t.Cleanup(r.shutdown)

	r.beginGrace()
	r.noteConnectionTornDown()

	// Connection 2's first packet opens the generation, but its connection-up
	// callback is never delivered to the router.
	deliver(t, r, &pahov5.Client{}, &pahov5.Publish{Topic: "generation/two", QoS: 1})
	waitPendingTopics(t, r, "generation/two")

	// Connection 2 drops; connection 3 comes up normally.
	r.noteConnectionTornDown()
	r.beginGrace()

	require.Empty(t, pendingTopics(r),
		"connection 2's entries carry dead acknowledgements and must not survive into connection 3")
	require.Equal(t, int64(1), r.StalePurgedCount())
	require.Zero(t, r.unsettledSnapshot(10).Count)
}

// TestRouterGeneration_OldSocketPublishDuringRecycleStaysDiscarded closes the
// corner where the recycled connection had delivered NOTHING before the recycle
// began. The router then holds no client identity for it, so its first packet
// looks exactly like a replacement client's first packet — and treating it as
// one would lift the recycle's discard window while the old socket is still
// live, buffer old-socket ingress into the replacement generation, and leave it
// there with a dead acknowledgement.
//
// The session's teardown of that socket is the discriminator: until it reports
// the connection torn down, an unseen client during a recycle is the OLD one.
func TestRouterGeneration_OldSocketPublishDuringRecycleStaysDiscarded(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk), withUnmatchedGrace(testGrace), withDispatchCapacity(4))
	t.Cleanup(r.shutdown)

	// Connection up, but it delivers nothing before the recycle starts.
	r.beginGrace()
	require.NoError(t, r.quiesceForRecycle(context.Background(), context.Background(), nil))

	// The old socket finally delivers. It must still be discarded un-acked.
	oldClient := &pahov5.Client{}
	deliver(t, r, oldClient, &pahov5.Publish{Topic: "old/socket", QoS: 1})
	wait.Until(t, 2*time.Second, "old-socket ingress is discarded", func() bool {
		return r.StalePurgedCount() == 1
	})
	require.Empty(t, pendingTopics(r), "old-socket ingress must not enter the replacement generation")
	r.mu.RLock()
	discarding := r.discarding
	r.mu.RUnlock()
	require.True(t, discarding, "an old-socket publish must not lift the recycle discard window")

	// The session tears the socket down; only now can an unseen client be the
	// replacement, and its CONNACK backlog must survive.
	r.noteConnectionTornDown()
	deliver(t, r, &pahov5.Client{}, &pahov5.Publish{Topic: "replacement/backlog", QoS: 1})
	waitPendingTopics(t, r, "replacement/backlog")
	require.Equal(t, int64(1), r.StalePurgedCount(),
		"the replacement client's backlog is not old-socket ingress")

	r.beginGrace()
	require.Equal(t, []string{"replacement/backlog"}, pendingTopics(r))
}

// TestRouterGeneration_AckAcrossConnectionCycleIsCounted pins the
// reconnect-acknowledgement measurement on the connection generation rather
// than on an SDK error class. Paho's acknowledgement tracker flushes the
// acknowledged prefix asynchronously, so an Ack marked just before the
// connection dropped returns NIL and is still redelivered.
//
// Counterfactual (the pre-fix error-driven count): only paho.ErrPacketNotFound
// incremented the counter, so a settlement that raced the teardown reported
// plain success and the guaranteed duplicate went unmeasured — the metric read
// zero while a duplicate flood was in flight.
func TestRouterGeneration_AckAcrossConnectionCycleIsCounted(t *testing.T) {
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec)
	t.Cleanup(r.shutdown)

	client := &pahov5.Client{}
	r.noteLiveClient(client)
	settle := r.trackAcknowledgement(r.ackWithReconnectMapping(client, func() error { return nil }))

	// The connection is torn down between receive and settlement.
	r.noteConnectionTornDown()

	require.NoError(t, settle(), "a settlement across a connection cycle still succeeds: the broker redelivers")
	require.Equal(t, int64(1), r.AckAfterReconnectCount(),
		"a settlement whose connection cycled is a guaranteed redelivery and must be counted")
	require.Len(t, rec.FindEntries(MetricMQTTAckAfterReconnect), 1)
}

// TestRouterGeneration_AckOnLiveConnectionIsNotCounted is the negative half:
// an ordinary settlement on a stable connection must not inflate the
// reconnect-duplicate signal.
func TestRouterGeneration_AckOnLiveConnectionIsNotCounted(t *testing.T) {
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec)
	t.Cleanup(r.shutdown)

	client := &pahov5.Client{}
	r.noteLiveClient(client)
	settle := r.trackAcknowledgement(r.ackWithReconnectMapping(client, func() error { return nil }))
	require.NoError(t, settle())
	require.Zero(t, r.AckAfterReconnectCount())
	require.Empty(t, rec.FindEntries(MetricMQTTAckAfterReconnect))
	require.Zero(t, r.unsettledSnapshot(10).Count, "a successful settlement clears its unsettled entry")
}

// deliver drives one publish through the production Paho callback seam,
// stamped with the client that delivered it.
func deliver(t *testing.T, r *router, client *pahov5.Client, pub *pahov5.Publish) {
	t.Helper()
	handled, err := r.onPublishReceived(pahov5.PublishReceived{Packet: pub, Client: client})
	require.NoError(t, err)
	require.True(t, handled)
}

func pendingTopics(r *router) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	topics := make([]string, 0, len(r.pending))
	for i := range r.pending {
		topics = append(topics, r.pending[i].pub.Topic)
	}
	return topics
}

func waitPendingCount(t *testing.T, r *router, n int) {
	t.Helper()
	wait.Until(t, 2*time.Second, "pending buffer reaches expected depth", func() bool {
		return r.PendingCount() == n
	})
}

func waitPendingTopics(t *testing.T, r *router, topics ...string) {
	t.Helper()
	wait.Until(t, 2*time.Second, "pending buffer holds the expected topics", func() bool {
		got := pendingTopics(r)
		if len(got) != len(topics) {
			return false
		}
		for i := range got {
			if got[i] != topics[i] {
				return false
			}
		}
		return true
	})
}
