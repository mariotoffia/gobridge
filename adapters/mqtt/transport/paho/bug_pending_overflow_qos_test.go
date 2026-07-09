package paho

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// c4-qos12-overflow (HIGH): a QoS 1/2 publish is NEVER dropped for the pending
// buffer's BYTE ceiling.
//
// The byte ceiling (pendingBytesLimit) governs QoS 0 memory only. A QoS 1/2
// publish is ALWAYS buffered: dropping it is never safe — ack+drop loses it,
// and un-ack+drop head-of-line-blocks paho's CONTIGUOUS-PREFIX manual-ack
// stream (acksTracker.flush sends the acknowledged prefix and stops at the
// first un-acked entry), stranding acks for messages that WERE delivered and,
// once receive_maximum un-acked slots accumulate, wedging ingress on a stable
// connection. QoS 1/2 pending memory is bounded WITHOUT a byte cap by the
// entry-count cap (== receive_maximum), which Receive-Maximum flow control
// enforces. QoS 0 stays best-effort drop-without-ack.
// ═══════════════════════════════════════════════════════════════════════════

// TestBug_PendingByteCap_QoS12_NeverDroppedForByteCap_DrainsAndAcksInOrder
// publishes a QoS 1 backlog whose aggregate size blows PAST the pending byte
// ceiling while the count cap has ample room, then asserts (a) NO QoS 1/2 is
// dropped or lost for the byte ceiling, and (b) once a handler registers the
// whole backlog flushes — every message delivered exactly once in ARRIVAL
// ORDER and every one acked, i.e. the ack stream drains and ingress is NOT
// wedged.
//
// Mutation killed (the reviewer's Critical): restore the pre-fix bufferLocked
// that refuses a QoS 1/2 over the BYTE cap — either the un-ack+drop variant
// (my previous, wedging) or the ack+drop variant (the original F-2) — and
// PendingCount drops below n and `delivered` loses the tail messages, failing
// (a); an un-ack+drop victim would also strand later acks, but (a) already
// catches the regression before any wedge can manifest.
func TestBug_PendingByteCap_QoS12_NeverDroppedForByteCap_DrainsAndAcksInOrder(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk), withUnmatchedGrace(testGrace))
	defer r.shutdown()

	// Byte ceiling far smaller than the backlog; the COUNT cap stays at the
	// default (65535), so ONLY the byte ceiling is exercised — and it must
	// never drop a QoS 1/2. No handler is registered and the clock is within
	// grace, so each unmatched publish takes the buffer path.
	r.mu.Lock()
	r.pendingBytesLimit = 20
	r.mu.Unlock()

	const n = 5
	payloads := make([]string, n)
	acked := make([]atomic.Int32, n)
	for i := 0; i < n; i++ {
		payloads[i] = fmt.Sprintf("msg-%02d", i) // 6B payload + 3B topic = 9B each; n×9 ≫ 20B
		idx := i
		r.dispatch(&pahov5.Publish{Topic: "q/1", QoS: 1, Payload: []byte(payloads[idx])},
			func() error { acked[idx].Add(1); return nil })
	}

	// (a) NOTHING dropped for the byte ceiling: every QoS 1 is buffered.
	require.Equal(t, n, r.PendingCount(),
		"c4-qos12-overflow: QoS 1/2 publishes must NEVER be dropped for the byte ceiling")
	require.Equal(t, int64(0), r.OverflowDroppedCount(),
		"the byte ceiling must not trigger a QoS 1/2 overflow drop")
	require.Empty(t, rec.FindEntries(MetricMQTTRouterOverflowDropped),
		"no protocol-violation overflow drop occurred")
	if _, dropped := r.Stats(); dropped != 0 {
		t.Fatalf("no drops expected (no QoS 0 to evict, nothing refused), got dropped=%d", dropped)
	}
	for i := 0; i < n; i++ {
		require.Equal(t, int32(0), acked[i].Load(),
			"a buffered QoS 1 stays un-acked until it is actually delivered")
	}

	// (b) Register the matching handler: the flush delivers the whole backlog
	// in ARRIVAL ORDER and acks each — the ack stream drains, ingress is not
	// wedged. RegisterFiltered adds the flush to r.wg before returning, so
	// r.Wait() is a deterministic barrier (no sleep).
	var mu sync.Mutex
	var delivered []string
	r.RegisterFiltered("rx", []string{"q/1"}, func(pub *pahov5.Publish, ack func() error) {
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
	require.Equal(t, payloads, got,
		"every buffered QoS 1 must be delivered exactly once, in arrival order")
	for i := 0; i < n; i++ {
		require.Equal(t, int32(1), acked[i].Load(),
			"every delivered QoS 1 must be acked — the ack stream drains, ingress is not wedged")
	}
	require.Equal(t, 0, r.PendingCount(), "the pending buffer is fully drained after flush")
	require.Equal(t, int64(0), r.OverflowDroppedCount(), "still no overflow drop after drain")
}

// TestBug_PendingByteCap_QoS0Overflow_NoAck asserts the QoS 0 byte-ceiling
// overflow path is correct and unchanged: a QoS 0 publish carries no delivery
// contract (ack is nil), so when it exceeds the byte ceiling it is dropped
// WITHOUT any ack and counted only on the generic best-effort drop metric —
// never on the QoS 1/2 protocol-violation overflow metric.
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

// TestBug_CountCapValve_ProtocolViolation_AcksVictimAndDrainsPrefix pins the
// QoS 1/2 count-cap valve (acl_router.go dispatch overflow branch + bufferLocked):
// the pending buffer's entry-count cap equals receive_maximum, so a compliant
// broker's Receive-Maximum flow control makes this branch UNREACHABLE. A broker
// that exceeds the granted receive_maximum (protocol violation) can still push
// one QoS 1/2 past a full buffer that holds NO evictable QoS 0. That single
// victim MUST be ACKED-and-dropped, never un-acked+dropped: paho's manual-ack
// acksTracker.flush sends a strictly-contiguous acknowledged PREFIX and breaks at
// the first un-acked entry, so a permanently un-acked victim would head-of-line-
// block every later ack and (once receive_maximum slots fill) wedge ingress on a
// stable connection while reporting healthy. Acking the victim keeps the ack
// stream draining; the buffered prefix still settles on handler registration.
//
// This is the EXACT branch round-1 flagged as the wedge risk and had NO positive
// coverage (every other OverflowDroppedCount assertion is == 0).
//
// Mutation killed (reproduces the round-1 wedge): drop the victim ack in
// dispatch's QoS>0 overflow branch (un-ack+drop) → assertion (b) "victim acked"
// fails. Dropping the overflowDropped counter/metric fails assertion (a).
func TestBug_CountCapValve_ProtocolViolation_AcksVictimAndDrainsPrefix(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk), withUnmatchedGrace(testGrace))
	defer r.shutdown()

	// Minimal reachable count cap: receive_maximum == 2. The byte ceiling stays
	// at its 64 MiB default so ONLY the count cap is exercised. No handler is
	// registered and the clock is within grace, so each unmatched publish takes
	// the buffer path.
	r.setPendingLimit(2)

	const n = 2
	payloads := []string{"first", "second"}
	prefixAcked := make([]atomic.Int32, n)
	for i := 0; i < n; i++ {
		idx := i
		r.dispatch(&pahov5.Publish{Topic: "v/1", QoS: 1, Payload: []byte(payloads[idx])},
			func() error { prefixAcked[idx].Add(1); return nil })
	}
	require.Equal(t, n, r.PendingCount(),
		"the count cap admits exactly receive_maximum (2) QoS 1 entries")

	// A protocol-violating broker pushes a 3rd QoS 1 past the full buffer with no
	// QoS 0 to evict → the count-cap valve fires (bufferLocked returns false).
	var victimAcked atomic.Int32
	r.dispatch(&pahov5.Publish{Topic: "v/1", QoS: 1, Payload: []byte("victim")},
		func() error { victimAcked.Add(1); return nil })

	// (a) exactly one protocol-violation overflow drop is surfaced (not masked by
	// the generic best-effort QoS 0 drop metric).
	require.Equal(t, int64(1), r.OverflowDroppedCount(),
		"the count-cap valve surfaces exactly one QoS 1/2 protocol-violation drop")
	require.Len(t, rec.FindEntries(MetricMQTTRouterOverflowDropped), 1,
		"the dedicated protocol-violation metric fires once")
	require.Equal(t, n, r.PendingCount(),
		"the victim is dropped, not buffered — the buffered prefix is untouched")

	// (b) the victim was ACKED — its slot is not a permanent un-acked entry, so
	// paho's contiguous-prefix ack stream is not head-of-line-blocked.
	require.Equal(t, int32(1), victimAcked.Load(),
		"the dropped QoS 1/2 victim MUST be acked — an un-acked victim wedges the ack stream (round-1)")
	for i := 0; i < n; i++ {
		require.Equal(t, int32(0), prefixAcked[i].Load(),
			"the buffered prefix stays un-acked until it is actually delivered")
	}

	// (c) registering the handler flushes the buffered prefix in ARRIVAL order,
	// each acked — the ack stream drains, ingress is not wedged. RegisterFiltered
	// enrolls the flush in r.wg before returning, so r.Wait() is a deterministic
	// barrier (no sleep).
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
	require.Equal(t, payloads, got,
		"only the buffered prefix (#1,#2) drains, in arrival order — the dropped victim is not delivered")
	for i := 0; i < n; i++ {
		require.Equal(t, int32(1), prefixAcked[i].Load(),
			"every delivered prefix message is acked — the ack stream drains, ingress is not wedged")
	}
	require.Equal(t, 0, r.PendingCount(), "the pending buffer is fully drained after flush")
}
