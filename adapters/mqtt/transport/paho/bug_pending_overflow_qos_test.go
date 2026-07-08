package paho

import (
	"sync/atomic"
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// F-2 (HIGH): un-acked QoS 1/2 drop on pending-buffer overflow poisons paho's
// ack tracker → permanent ingress deadlock.
//
// During the unmatched-grace window bufferLocked returns false when the
// pending buffer is over its byte cap and holds only un-evictable QoS 1/2
// entries. For a QoS 1/2 publish the OLD code dropped it WITHOUT acking. paho's
// acksTracker flushes acks strictly in RECEIVE ORDER, so a single permanently
// un-acked packet head-of-line-blocks every later ack, pins the broker's
// Receive-Maximum window, and halts ingress on a LIVE connection.
//
// Fix: ack-then-drop for the QoS 1/2 overflow case (mirrors dropUnmatched).
// Redelivery is already forfeited by dropping, so holding the ack hostage only
// adds the deadlock. QoS 0 stays drop-without-ack (no delivery contract).
// ═══════════════════════════════════════════════════════════════════════════

// TestBug_PendingByteCap_QoS1Overflow_AcksBeforeDrop forces the byte-cap
// overflow of a QoS 1 publish while the buffer holds only an un-evictable
// QoS 1 entry, and asserts the overflowed publish is ACKED before being
// dropped.
//
// Counterfactual (proven by reverting the ack() call in the dispatch
// !buffered QoS>0 branch): pre-fix overflowAcked stays 0 — the un-acked slot
// that head-of-line-blocks paho's ack stream and deadlocks ingress.
//
// Metric counterfactual (FIX 2 / M-1, proven by pointing the QoS>0 overflow
// Counter back at MetricMQTTRouterDropped): the OverflowDropped assertions
// below fail — real QoS 1/2 loss would be masked on the generic best-effort
// drop metric shared with QoS 0.
//
// This closes the gap the audit flagged: the existing byte-cap test
// (TestBug_PendingByteCap_DropsQoS0OverByteCeiling) covers only QoS 0
// eviction, never the QoS 1/2 overflow drop.
func TestBug_PendingByteCap_QoS1Overflow_AcksBeforeDrop(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk), withUnmatchedGrace(testGrace))
	defer r.shutdown()

	// Tiny byte ceiling: room for exactly one small QoS 1 entry. No handler
	// is registered and the clock is within grace, so unmatched publishes
	// take the buffer path.
	r.mu.Lock()
	r.pendingBytesLimit = 20
	r.mu.Unlock()

	// First QoS 1 publish FITS (11 bytes) and is buffered un-acked — the
	// legitimate startup-backlog behaviour (broker redelivers on crash).
	var firstAcked atomic.Int32
	r.dispatch(&pahov5.Publish{Topic: "q", QoS: 1, Payload: []byte("0123456789")},
		func() error { firstAcked.Add(1); return nil })
	require.Equal(t, 1, r.PendingCount(), "first QoS 1 publish is buffered within grace")
	require.Equal(t, int32(0), firstAcked.Load(), "buffered publish stays un-acked")

	// Second QoS 1 publish OVERFLOWS the byte cap. The buffer holds only an
	// un-evictable QoS 1 entry, so bufferLocked drops it. F-2: it MUST be
	// acked before dropping.
	var overflowAcked atomic.Int32
	r.dispatch(&pahov5.Publish{Topic: "q", QoS: 1, Payload: []byte("0123456789")},
		func() error { overflowAcked.Add(1); return nil })

	require.Equal(t, 1, r.PendingCount(), "the overflow publish is dropped, not buffered")
	require.Equal(t, int32(1), overflowAcked.Load(),
		"F-2: the overflow-dropped QoS 1 publish MUST be acked so paho's in-order ack "+
			"stream keeps draining (prevents ingress deadlock)")
	require.Equal(t, int32(0), firstAcked.Load(),
		"only the dropped publish is acked; the still-buffered first publish stays un-acked")
	require.NotEmpty(t, rec.FindEntries(MetricMQTTRouterOverflowDropped),
		"FIX 2 / M-1: a QoS 1/2 overflow drop is REAL loss — counted on the DEDICATED overflow metric")
	require.Empty(t, rec.FindEntries(MetricMQTTRouterDropped),
		"a QoS 1/2 overflow drop must NOT land on the generic (QoS 0 best-effort) drop metric")
	require.Equal(t, int64(1), r.OverflowDroppedCount(),
		"the QoS 1/2 overflow drop is surfaced on OverflowDroppedCount (parity with CoveredDroppedCount)")
	if _, dropped := r.Stats(); dropped != 1 {
		t.Fatalf("overflow AGGREGATE (Stats' dropped) = %d, want 1 (every overflow drop counts, QoS 0 and 1/2 alike)", dropped)
	}
}

// TestBug_PendingByteCap_QoS0Overflow_NoAck asserts the QoS 0 overflow path is
// unchanged: a QoS 0 publish carries no delivery contract (ack is nil), so it
// is dropped WITHOUT any ack — the F-2 fix must not spuriously ack QoS 0.
func TestBug_PendingByteCap_QoS0Overflow_NoAck(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk), withUnmatchedGrace(testGrace))
	defer r.shutdown()

	r.mu.Lock()
	r.pendingBytesLimit = 20
	r.mu.Unlock()

	// Buffer one QoS 1 entry (un-evictable) to fill the cap.
	r.dispatch(&pahov5.Publish{Topic: "q", QoS: 1, Payload: []byte("0123456789")},
		func() error { return nil })
	require.Equal(t, 1, r.PendingCount())

	// A QoS 0 publish overflows. Production wires ack == nil for QoS 0
	// (onPublishReceived), so there is nothing to ack; the drop must be
	// clean without touching a (non-existent) ack.
	r.dispatch(&pahov5.Publish{Topic: "q", QoS: 0, Payload: []byte("0123456789")}, nil)

	require.Equal(t, 1, r.PendingCount(), "the QoS 0 overflow publish is dropped, not buffered")
	require.NotEmpty(t, rec.FindEntries(MetricMQTTRouterDropped),
		"a QoS 0 overflow drop stays on the generic best-effort drop metric")
	require.Empty(t, rec.FindEntries(MetricMQTTRouterOverflowDropped),
		"FIX 2 / M-1: a QoS 0 overflow drop is best-effort (no delivery contract) — it must NOT touch the real-loss overflow metric")
	require.Equal(t, int64(0), r.OverflowDroppedCount(),
		"a QoS 0 overflow drop is not real loss and is not surfaced on OverflowDroppedCount")
}
