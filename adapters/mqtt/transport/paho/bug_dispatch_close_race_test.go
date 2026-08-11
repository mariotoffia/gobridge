package paho

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// (BLOCKER regression): the serialized dispatch worker (finding 6) is a
// SECOND producer into r.wg. Close does shutdown() (closes r.stop) then
// r.wg.Wait(); if dispatchCh still holds buffered items at Close (handler
// latency → backpressure), dispatchLoop's select can still pick a buffered
// item AFTER r.stop is closed and call r.wg.Add via fanout CONCURRENTLY with
// the in-flight Wait → "panic: sync: WaitGroup is reused before previous Wait
// has returned" + data race.
//
// Fix: Close joins the dispatch worker (awaitDispatchLoop → dispatchDone)
// BEFORE r.wg.Wait(), so the worker's final Add completes before the Wait.
//
// This test drives real backpressure: it blocks the worker inside the first
// emit while hundreds of items queue, then runs the Close quiesce sequence
// (shutdown → awaitDispatchLoop → Wait) while the worker drains. Without the
// join the drain Adds race the Wait (panic / race under -race). With the join
// it is quiescent.
// ═══════════════════════════════════════════════════════════════════════════

func TestBug_DispatchLoop_CloseJoin_NoWaitGroupRace(t *testing.T) {
	r := newRouter(nil, nil)

	firstEntered := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	var processed atomic.Int64

	// A match-all handler. The FIRST emit blocks on gate so the worker
	// stalls with a full backlog queued behind it; every later emit is fast,
	// so once released the worker churns r.wg 0→1→0 rapidly — maximising the
	// Add-during-Wait window a missing join would hit.
	r.RegisterFiltered("rx", nil, func(_ *pahov5.Publish, _ func() error) {
		once.Do(func() {
			close(firstEntered)
			<-gate
		})
		processed.Add(1)
	})

	// Start the serialized dispatch worker (dispatchCh + dispatchLoop).
	r.beginGrace()

	// Queue a deep backlog (well under the default dispatch buffer so nothing
	// is dropped): item 1 is being processed (blocked on gate); the rest sit
	// in dispatchCh waiting to be drained.
	const backlog = 500
	for i := 0; i < backlog; i++ {
		r.enqueueDispatch(&pahov5.Publish{Topic: "t", QoS: 0, Payload: []byte("x")}, nil)
	}
	<-firstEntered // worker is stalled on item 1 with ~backlog-1 items buffered

	// Run the Close quiesce sequence exactly as Session.Close does.
	r.shutdown() // closes r.stop and marks closing
	close(gate)  // release the worker so it drains the buffered backlog

	quiesced := make(chan struct{})
	go func() {
		r.awaitDispatchLoop() // JOIN the worker before Wait (the fix)
		r.Wait()
		close(quiesced)
	}()

	select {
	case <-quiesced:
	case <-time.After(5 * time.Second):
		t.Fatal("Close quiesce (awaitDispatchLoop + Wait) did not return")
	}

	// Reaching here without a panic/race is the assertion. Every drained
	// item ran exactly once through the single handler.
	require.GreaterOrEqual(t, processed.Load(), int64(1))
}
