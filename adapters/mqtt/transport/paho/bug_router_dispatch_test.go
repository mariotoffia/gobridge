package paho

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// Finding 2 (HIGH): QoS 1/2 acked-and-dropped in the unregister→re-register
// gap. beginGrace re-arms the grace window only on connection-up, never on
// Unregister. A supervisor-restarted receiver (Run exits → Unregister → new
// Run → RegisterFiltered) leaves a gap; a publish landing there matches no
// handler and — past the long-expired grace deadline — is ACKED and DROPPED
// (real at-least-once loss). Fix: Unregister re-arms the grace window so the
// gap publish is BUFFERED for the replacement handler.
// ═══════════════════════════════════════════════════════════════════════════

func TestBug_UnregisterRearmsGrace_BuffersCoveredTopicInGap(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk), withUnmatchedGrace(testGrace))
	defer r.shutdown()

	// Simulate a long-lived connected session whose startup grace window has
	// already expired (steady state). graceStarted is set directly — without
	// spawning the async grace-sweep worker — so the assertions below cannot
	// race a background sweep; rearmGrace's guard (graceStarted) is what the
	// Unregister path exercises.
	r.mu.Lock()
	r.graceStarted = true
	r.graceDeadline = clk.Now().Add(-time.Second) // grace already elapsed
	r.mu.Unlock()

	r.RegisterFiltered("rx", []string{"t/#"}, func(_ *pahov5.Publish, ack func() error) {
		if ack != nil {
			_ = ack()
		}
	})

	// Receiver restart: Unregister must re-arm the grace window so the gap is
	// covered again.
	r.Unregister("rx")

	// A covered-topic QoS 1 publish arrives in the unregister→re-register
	// gap. It MUST be buffered, never acked-and-dropped.
	var acked atomic.Int32
	r.dispatch(&pahov5.Publish{Topic: "t/x", QoS: 1, Payload: []byte("p")},
		func() error { acked.Add(1); return nil })

	require.Equal(t, 1, r.PendingCount(),
		"covered-topic publish in the gap must be buffered after Unregister re-arms grace")
	require.Equal(t, int32(0), acked.Load(),
		"covered-topic QoS1/2 publish must NOT be acked-and-dropped in the gap")
	require.Equal(t, int64(0), r.UnmatchedDroppedCount())

	// Re-register the replacement receiver: the buffered publish flushes.
	flushed := make(chan struct{})
	r.RegisterFiltered("rx", []string{"t/#"}, func(_ *pahov5.Publish, ack func() error) {
		if ack != nil {
			_ = ack()
		}
		close(flushed)
	})
	select {
	case <-flushed:
	case <-time.After(3 * time.Second):
		t.Fatal("buffered gap publish was not flushed to the re-registered handler")
	}
	r.Wait()
	require.Equal(t, int32(1), acked.Load(), "gap publish settled exactly once after re-register")
	require.Equal(t, 0, r.PendingCount())
}

// ═══════════════════════════════════════════════════════════════════════════
// Finding 4 (MEDIUM): pending-flush + live dispatch must funnel through ONE
// serialized, in-order path (ports.Receiver emit is SEQUENTIAL, in-order).
// The buffered (older) publishes must fully emit before any live (newer)
// publish, and never concurrently, for a given handler.
// ═══════════════════════════════════════════════════════════════════════════

func TestBug_FlushAndLiveDispatch_SerializedInOrder(t *testing.T) {
	r := newRouter(nil, nil)
	defer r.shutdown()

	// Buffer one pre-registration publish (no handler yet → pending).
	r.dispatch(&pahov5.Publish{Topic: "t/1", QoS: 0, Payload: []byte("1")}, nil)
	require.Equal(t, 1, r.PendingCount())

	var mu sync.Mutex
	var order []string
	rec := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	// active counts concurrently-executing emits; overlap latches true if a
	// second emit ever runs while another is in flight (a ports.Receiver
	// sequential-emit violation).
	var active atomic.Int32
	var overlap atomic.Bool

	flushEntered := make(chan struct{})
	liveEntered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, liveOnce sync.Once

	r.RegisterFiltered("rx", []string{"t/#"}, func(pub *pahov5.Publish, _ func() error) {
		if active.Add(1) > 1 {
			overlap.Store(true)
		}
		defer active.Add(-1)
		switch pub.Topic {
		case "t/1":
			// The buffered flush emit: signal, then hold emitMu until released.
			enteredOnce.Do(func() { close(flushEntered) })
			<-release
		case "t/2":
			liveOnce.Do(func() { close(liveEntered) })
		}
		rec(pub.Topic)
	})

	// The flush is now emitting t/1 and holding emitMu.
	<-flushEntered

	// A live (newer) publish arrives WHILE the flush holds emitMu. With
	// serialization it MUST block on emitMu (liveEntered stays closed) until
	// the flush completes; without it, t/2 would emit concurrently now.
	liveDone := make(chan struct{})
	go func() {
		r.dispatch(&pahov5.Publish{Topic: "t/2", QoS: 0, Payload: []byte("2")}, nil)
		close(liveDone)
	}()

	// Serialized emit means the live publish cannot enter its handler while
	// the flush holds emitMu.
	select {
	case <-liveEntered:
		t.Fatal("live dispatch emitted concurrently with the in-flight pending flush (not serialized)")
	case <-time.After(50 * time.Millisecond):
	}

	// Release the flush; it finishes t/1, frees emitMu, then the live t/2 emits.
	close(release)
	<-liveDone
	r.Wait()

	require.False(t, overlap.Load(), "no two emits to a handler may overlap (sequential-emit contract)")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"t/1", "t/2"}, order,
		"buffered (older) publish must emit before the live (newer) publish")
}

// ═══════════════════════════════════════════════════════════════════════════
// Finding 5 (MEDIUM): Unregister must await in-flight dispatch so emit is
// never invoked after Unregister returns (and therefore never after the
// owning Receiver.Run has returned — ports.Receiver emit lifetime).
// ═══════════════════════════════════════════════════════════════════════════

func TestBug_Unregister_AwaitsInFlightDispatch(t *testing.T) {
	r := newRouter(nil, nil)
	defer r.shutdown()

	entered := make(chan struct{})
	release := make(chan struct{})

	var mu sync.Mutex
	var order []string
	rec := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	r.RegisterFiltered("rx", nil, func(_ *pahov5.Publish, _ func() error) {
		close(entered)
		<-release
		rec("emit-done")
	})

	// Dispatch in the background; the handler blocks in-flight on release.
	go r.dispatch(&pahov5.Publish{Topic: "t", QoS: 0, Payload: []byte("p")}, nil)

	// Wait until the emit has started (inflight already incremented under
	// r.mu before the handler ran).
	<-entered

	// Unregister must block until the in-flight emit returns.
	unregDone := make(chan struct{})
	go func() {
		r.Unregister("rx")
		rec("unregister-done")
		close(unregDone)
	}()

	// Let the emit finish; Unregister's inflight.Wait then unblocks.
	close(release)
	<-unregDone
	r.Wait()

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"emit-done", "unregister-done"}, order,
		"Unregister must return only after the in-flight emit has returned")
}

// ═══════════════════════════════════════════════════════════════════════════
// Finding 6 (MEDIUM): a QoS 0 flood must not stall the connection. On a full
// serialized dispatch queue a QoS 0 publish is dropped (no delivery
// contract) so the paho read loop keeps moving (PINGRESP/PUBACK); QoS 1/2 is
// bounded by the broker's Receive-Maximum window and blocks instead.
// ═══════════════════════════════════════════════════════════════════════════

func TestBug_DispatchQueue_QoS0DroppedWhenFull(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk))
	r.dispatchSize = 1 // tiny queue so it fills deterministically
	defer r.shutdown()

	enter := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	r.RegisterFiltered("rx", nil, func(_ *pahov5.Publish, _ func() error) {
		once.Do(func() { close(enter) })
		<-release
	})

	// Start the serialized dispatch worker (dispatchCh, cap 1).
	r.beginGrace()

	// First publish: the worker dequeues it and blocks inside emit.
	r.enqueueDispatch(&pahov5.Publish{Topic: "t", QoS: 0, Payload: []byte("a")}, nil)
	<-enter

	// Worker is busy. Fill the single queue slot, then the next QoS 0 publish
	// has nowhere to go and must be dropped (not block the callback goroutine).
	r.enqueueDispatch(&pahov5.Publish{Topic: "t", QoS: 0, Payload: []byte("b")}, nil)
	r.enqueueDispatch(&pahov5.Publish{Topic: "t", QoS: 0, Payload: []byte("c")}, nil)

	require.GreaterOrEqual(t, r.dropCount.Load(), int64(1),
		"QoS 0 publish must be dropped when the dispatch queue is full")
	require.NotEmpty(t, rec.FindEntries(MetricMQTTRouterDropped),
		"dropped-metric emitted for the QoS 0 flood drop")

	close(release)
}

// TestBug_PendingByteCap_DropsOverByteCeiling verifies the pending buffer is
// bounded in BYTES (not just entry count) so a flood of large publishes
// during a grace window cannot buffer gigabytes (finding 6).
func TestBug_PendingByteCap_DropsQoS0OverByteCeiling(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk), withUnmatchedGrace(testGrace))
	defer r.shutdown()

	r.mu.Lock()
	r.pendingBytesLimit = 10 // tiny byte ceiling
	r.mu.Unlock()

	// A QoS 0 publish larger than the byte ceiling: entry count has room but
	// the byte cap is exceeded, so with no QoS 0 already buffered to evict it
	// is dropped rather than buffered.
	r.dispatch(&pahov5.Publish{Topic: "t", QoS: 0, Payload: []byte("way-over-ten-bytes")}, nil)

	require.Equal(t, 0, r.PendingCount(),
		"QoS 0 publish exceeding the pending byte ceiling must be dropped, not buffered")
	require.GreaterOrEqual(t, r.dropCount.Load(), int64(1))
	require.NotEmpty(t, rec.FindEntries(MetricMQTTRouterDropped))
}
