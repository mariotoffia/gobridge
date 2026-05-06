package runtime_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// ═══════════════════════════════════════════════════════════════════════
// RouteRunner Pull-Pause Backpressure (Scenario #2 deepening)
//
// Existing tests (route_runner_concurrent_test.go) assert that the
// per-route semaphore caps concurrent delivery goroutines (peak ≤
// MaxInFlight). Those tests do NOT assert the stronger pull-pause
// contract: that the receiver's emit callback itself blocks on
// acquireSlots when the semaphore is saturated, proving the receiver
// stops pulling new messages from the transport (the backpressure
// signal that propagates into e.g. MQTT manual-ack pre-fetch windows
// or SQS long-polling concurrency caps).
//
//     receiver.Emit(n+1) ──▶ acquireSlots ──▶ BLOCK
//                                │
//                 slot frees     │
//                    ◀───────────┘
//
//  ┌──────┬──────────────────────────────────────────────┬──────────┐
//  │ ID   │ Description                                  │ Type     │
//  ├──────┼──────────────────────────────────────────────┼──────────┤
//  │ PP1  │ Emit blocks synchronously when sem is full   │ unit     │
//  │ PP2  │ Throughput resumes after sender unblocks     │ unit     │
//  └──────┴──────────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════

// TestRouteRunner_PullPause_EmitBlocksWhenSaturated validates that when
// MaxInFlight slots are all held by an inflight sender, a further call
// to receiver.Emit blocks (inside acquireSlots) and does not return
// until a slot is released. This is the contract that drives
// receiver-side backpressure in real transports — the receiver's run
// loop is stalled synchronously on its own Emit callback, which stops
// it from pulling more messages from the broker.
//
// Scenario:
//
//	MaxInFlight=2.
//	Two senders are parked (blocked on a gate).
//	A 3rd Emit call is launched in its own goroutine.
//	Verify: the 3rd Emit has NOT returned for at least 100ms
//	        (confirming synchronous block on acquireSlots).
//	Release the gate.
//	Verify: the 3rd Emit returns and the 3rd message is sent.
func TestRouteRunner_PullPause_EmitBlocksWhenSaturated(t *testing.T) {
	t.Parallel()

	const maxInFlight = 2

	gate := make(chan struct{})
	var sendsInProgress atomic.Int32

	sender := NewConcurrentSender(func(_ *messaging.Envelope) error {
		sendsInProgress.Add(1)
		defer sendsInProgress.Add(-1)
		<-gate
		return nil
	})

	receiver := NewFakeReceiver()
	runner := goruntime.NewRouteRunnerFromConfig(goruntime.RouteRunnerConfig{
		RouteID: "pullpause-route",
		Policy: routing.RoutePolicy{
			MaxInFlight:  maxInFlight,
			DeliveryMode: routing.DeliveryDirectHold,
		},
		Receiver: receiver,
		Sender:   sender,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = runner.Run(ctx)
	}()

	<-receiver.Ready()

	// Fill the two slots. These Emits return as soon as the delivery
	// goroutine is spawned (acquireSlots succeeds and returns). Wait
	// until both senders are actually parked on the gate — this
	// guarantees the 2 semaphore slots are held by in-flight work.
	for i := range maxInFlight {
		del := NewFakeDelivery(&messaging.Envelope{
			ID:      fmt.Sprintf("pp-fill-%d", i),
			Payload: []byte("fill"),
		})
		require.NoError(t, receiver.Emit(ctx, del), "fill Emit should not error")
	}
	waitFor(t, 2*time.Second, "both senders parked on gate", func() bool {
		return sendsInProgress.Load() == int32(maxInFlight)
	})

	// Now launch the (N+1)th emit. It MUST block synchronously inside
	// acquireSlots because the per-route semaphore is saturated. The
	// delivery goroutine for msg #3 will not be spawned until a slot
	// is released.
	blocker := NewFakeDelivery(&messaging.Envelope{
		ID:      "pp-blocker",
		Payload: []byte("blocks"),
	})
	emitReturned := make(chan error, 1)
	emitStart := time.Now()
	go func() {
		emitReturned <- receiver.Emit(ctx, blocker)
	}()

	// Assert that Emit has NOT returned within a bounded window.
	// 100ms is well above scheduling jitter but still bounded.
	select {
	case err := <-emitReturned:
		t.Fatalf("Emit returned too early (%v) with err=%v -- "+
			"backpressure contract violated: receiver did not "+
			"pull-pause when MaxInFlight was saturated",
			time.Since(emitStart), err)
	case <-time.After(100 * time.Millisecond):
		// Expected: Emit is blocked inside acquireSlots.
	}

	// Verify the blocking Emit has NOT spawned a 3rd sender goroutine.
	// If it had, sendsInProgress would be 3 (or we'd see a peak > 2
	// from the concurrent tracker). Here we check directly: the third
	// send cannot start until a slot is released.
	assert.Equal(t, int32(maxInFlight), sendsInProgress.Load(),
		"no additional sender should have started while emit is blocked")

	// Release the gate so one (actually all) parked senders finish.
	close(gate)

	// The 3rd Emit must now return.
	select {
	case err := <-emitReturned:
		require.NoError(t, err, "blocker Emit should succeed after slot is released")
	case <-time.After(2 * time.Second):
		t.Fatal("blocker Emit did not return after gate was released -- " +
			"slot release did not propagate")
	}

	// Throughput resumes: all 3 messages get sent.
	waitFor(t, 2*time.Second, "all 3 messages sent", func() bool {
		return sender.SentCount() >= maxInFlight+1
	})

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestRouteRunner_PullPause_ThroughputResumesAfterSlowSender validates
// that after a slow sender completes and releases its slot, the
// receiver resumes pulling and the backlog drains at full concurrency.
//
// Scenario:
//
//	MaxInFlight=2.
//	Prime 2 slow sends (blocked on a gate), then enqueue 4 fast
//	sends via concurrent Emits. Each fast Emit will block on
//	acquireSlots behind the slow ones.
//	Release the gate.
//	Verify: all 6 messages sent, and the concurrent peak never
//	        exceeded MaxInFlight throughout.
func TestRouteRunner_PullPause_ThroughputResumesAfterSlowSender(t *testing.T) {
	t.Parallel()

	const maxInFlight = 2

	gate := make(chan struct{})
	var releaseSlow atomic.Bool
	var concurrentPeak int64
	var currentConcurrency int64

	sender := NewConcurrentSender(func(env *messaging.Envelope) error {
		cur := atomic.AddInt64(&currentConcurrency, 1)
		defer atomic.AddInt64(&currentConcurrency, -1)
		for {
			p := atomic.LoadInt64(&concurrentPeak)
			if cur <= p {
				break
			}
			if atomic.CompareAndSwapInt64(&concurrentPeak, p, cur) {
				break
			}
		}
		// Only the first two ("slow-*") messages gate — fast ones run freely.
		if !releaseSlow.Load() && len(env.ID) > 5 && env.ID[:5] == "slow-" {
			<-gate
		}
		return nil
	})

	receiver := NewFakeReceiver()
	runner := goruntime.NewRouteRunnerFromConfig(goruntime.RouteRunnerConfig{
		RouteID: "pullpause-throughput-route",
		Policy: routing.RoutePolicy{
			MaxInFlight:  maxInFlight,
			DeliveryMode: routing.DeliveryDirectHold,
		},
		Receiver: receiver,
		Sender:   sender,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = runner.Run(ctx)
	}()

	<-receiver.Ready()

	// Prime 2 slow sends that fill the semaphore.
	for i := range maxInFlight {
		del := NewFakeDelivery(&messaging.Envelope{
			ID:      fmt.Sprintf("slow-%d", i),
			Payload: []byte("slow"),
		})
		require.NoError(t, receiver.Emit(ctx, del))
	}
	// Wait for both slow senders to park on the gate before queuing fast ones.
	waitFor(t, 2*time.Second, "slow senders parked", func() bool {
		return atomic.LoadInt64(&currentConcurrency) == int64(maxInFlight)
	})

	// Enqueue 4 fast sends concurrently. Each Emit will block on
	// acquireSlots until the slow senders release.
	const fastCount = 4
	var fastWg sync.WaitGroup
	fastWg.Add(fastCount)
	for i := range fastCount {
		go func(n int) {
			defer fastWg.Done()
			del := NewFakeDelivery(&messaging.Envelope{
				ID:      fmt.Sprintf("fast-%d", n),
				Payload: []byte("fast"),
			})
			_ = receiver.Emit(ctx, del)
		}(i)
	}

	// Release slow senders; throughput resumes.
	releaseSlow.Store(true)
	close(gate)

	// All 4 fast Emits must return.
	emitsReturned := make(chan struct{})
	go func() {
		fastWg.Wait()
		close(emitsReturned)
	}()
	select {
	case <-emitsReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("fast Emits did not all return after slow senders released")
	}

	// All 6 messages delivered.
	waitFor(t, 3*time.Second, "all 6 messages sent", func() bool {
		return sender.SentCount() >= maxInFlight+fastCount
	})

	// Peak concurrency never exceeded MaxInFlight.
	peak := atomic.LoadInt64(&concurrentPeak)
	assert.LessOrEqual(t, peak, int64(maxInFlight),
		"MaxInFlight=%d must be respected throughout; peak=%d", maxInFlight, peak)

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
