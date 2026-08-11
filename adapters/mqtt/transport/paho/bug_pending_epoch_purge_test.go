package paho

import (
	"sync"
	"sync/atomic"
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// (HIGH): the pre-registration pending buffer is PURGED of stale entries on
// reconnect, so a broker-redelivered QoS 1/2 is never ack-dropped as a bogus
// overflow — at-least-once survives a reconnect that races receiver startup.
//
// Sequence the fix defends against:
//  1. Connection #1 comes up (beginGrace, epoch 1). The broker delivers QoS 1
//     backlog before the receiver has registered its handler, so the router
//     buffers it. Each entry's ack is bound to connection #1's packet IDs.
//  2. Connection #1 drops and #2 comes up (beginGrace, epoch 2). A
//     clean_start=false broker REDELIVERS every un-acked QoS 1/2 from #1 with
//     FRESH packet IDs. The old entries' acks are now dead (paho
//     ErrPacketNotFound) and their twins are arriving fresh.
//  3. Without the purge, the stale twins still occupy the buffer up to the
//     count cap (== receive_maximum); each fresh redelivery then finds the
//     buffer full with no QoS 0 to evict and is ACK-DROPPED as a bogus
//     MetricMQTTRouterOverflowDropped — a live, still-desired message is lost
//     while the metric blames the broker.
//
// With the purge, beginGrace discards the stale epoch-1 entries (WITHOUT
// invoking their dead acks), the fresh redeliveries buffer cleanly, and once
// the handler registers the whole backlog drains exactly once.
//
// Mutation killed: delete the `r.purgeStalePendingLocked()` call in beginGrace
// → the redelivered copies hit the full buffer, OverflowDroppedCount becomes 2,
// the redelivered payloads are never delivered, and (a)/(d)/(e) fail.
// ═══════════════════════════════════════════════════════════════════════════
func TestBug_PendingEpochPurge_RedeliveredNotAckDroppedAsOverflow(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	const sessionID = "sess-A"
	r := newRouter(nil, rec,
		withRouterClock(clk),
		withUnmatchedGrace(testGrace),
		withSessionTag(sessionID),
	)
	defer r.shutdown()

	// receive_maximum == 2: the pending count cap admits exactly two un-acked
	// QoS 1/2 at a time, mirroring the broker's in-flight window.
	r.setPendingLimit(2)

	// Connection #1 comes up. First beginGrace: epoch 0 -> 1, grace started,
	// no purge (nothing was buffered under a prior connection).
	r.beginGrace()

	// The broker delivers a QoS 1 backlog before the handler registers, so the
	// router buffers both. Their acks belong to connection #1; the purge must
	// NEVER invoke them (paho would return ErrPacketNotFound on the new
	// connection, and acking a dead packet ID could ack the wrong fresh one).
	const n = 2
	stalePayloads := []string{"alpha", "beta"}
	var staleAcked atomic.Int32
	for i := 0; i < n; i++ {
		r.dispatch(&pahov5.Publish{Topic: "v/1", QoS: 1, Payload: []byte(stalePayloads[i])},
			func() error { staleAcked.Add(1); return nil })
	}
	require.Equal(t, n, r.PendingCount(), "connection #1 backlog is buffered pre-registration")
	require.Equal(t, int64(0), r.OverflowDroppedCount(), "no overflow before reconnect")

	// Connection #1 drops, connection #2 comes up. Second beginGrace: epoch
	// 1 -> 2, RECONNECT branch purges the epoch-1 twins.
	r.beginGrace()

	// (a) the stale twins are gone — the buffer is empty for the fresh window.
	require.Equal(t, 0, r.PendingCount(), "stale epoch-1 entries are purged on reconnect")
	// (b) exactly two entries were purged, surfaced on the counter and metric.
	require.Equal(t, int64(n), r.StalePurgedCount(), "both stale entries are counted as purged")
	purgeEntries := rec.FindEntries(MetricMQTTRouterStalePurged)
	require.Len(t, purgeEntries, 1, "the purge emits a single StalePurged metric for the batch")
	require.Equal(t, int64(n), purgeEntries[0].IValue, "the StalePurged metric carries the purged count")
	// (b'): the loss metric carries the session_id tag for attribution.
	require.Contains(t, purgeEntries[0].Tags, shared.Tag{Key: shared.TagKeySessionID, Value: sessionID},
		"the stale-purge loss metric is tagged with the session id")
	// (c) the purge did NOT invoke the dead connection-#1 acks.
	require.Equal(t, int32(0), staleAcked.Load(), "purge must not ack stale entries — their packet IDs are dead")

	// The broker redelivers the same messages fresh on connection #2, with NEW
	// ack callbacks bound to #2's packet IDs.
	freshPayloads := []string{"alpha", "beta"}
	freshAcked := make([]atomic.Int32, n)
	for i := 0; i < n; i++ {
		idx := i
		r.dispatch(&pahov5.Publish{Topic: "v/1", QoS: 1, Payload: []byte(freshPayloads[idx])},
			func() error { freshAcked[idx].Add(1); return nil })
	}

	// (d) the fresh redeliveries buffer cleanly — none is ack-dropped as a bogus
	// overflow, because the purge freed the cap.
	require.Equal(t, n, r.PendingCount(), "the fresh redeliveries fit — the purge freed the count cap")
	require.Equal(t, int64(0), r.OverflowDroppedCount(),
		"a redelivered live message must NOT be ack-dropped as a bogus overflow")
	require.Empty(t, rec.FindEntries(MetricMQTTRouterOverflowDropped), "no protocol-violation overflow occurred")
	for i := 0; i < n; i++ {
		require.Equal(t, int32(0), freshAcked[i].Load(), "a buffered redelivery stays un-acked until delivered")
	}

	// (e) the receiver finally registers: the fresh backlog drains exactly once,
	// in arrival order, each acked — the stale acks are STILL untouched.
	var mu sync.Mutex
	var delivered []string
	r.RegisterFiltered("rx", []string{"v/1"}, func(pub *pahov5.Publish, ack func() error) {
		mu.Lock()
		delivered = append(delivered, string(pub.Payload))
		mu.Unlock()
		if ack != nil {
			_ = ack()
		}
	})
	r.Wait()

	mu.Lock()
	got := append([]string(nil), delivered...)
	mu.Unlock()
	require.Equal(t, freshPayloads, got, "every redelivered message is delivered exactly once, in arrival order")
	for i := 0; i < n; i++ {
		require.Equal(t, int32(1), freshAcked[i].Load(), "every delivered redelivery is acked — the ack stream drains")
	}
	require.Equal(t, int32(0), staleAcked.Load(), "the dead connection-#1 acks are never invoked")
	require.Equal(t, 0, r.PendingCount(), "the pending buffer is fully drained after flush")
}
