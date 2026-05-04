package runtime_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// ═══════════════════════════════════════════════════════════════════
// RouteRunner Concurrent Delivery & Global Semaphore
//
// Tests verifying that MaxInFlight now effectively limits concurrent
// processing (P5) and the global semaphore throttles cross-route
// concurrency (P6).
//
// Summary:
// ┌──────┬──────────────────────────────────────────────┬──────────┐
// │ ID   │ Description                                  │ Type     │
// ├──────┼──────────────────────────────────────────────┼──────────┤
// │ T010 │ Concurrent delivery up to MaxInFlight        │ unit     │
// │ T011 │ Backpressure when semaphore full              │ unit     │
// │ T012 │ Graceful shutdown waits in-flight             │ unit     │
// │ T013 │ Global semaphore limits cross-route           │ unit     │
// │ T014 │ No global limit when unconfigured             │ unit     │
// └──────┴──────────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════

func makePeakTracker() (sender *ConcurrentSender, peak *int64) {
	var concurrentPeak int64
	var currentConcurrency int64
	s := NewConcurrentSender(func(_ *domain.Envelope) error {
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
		time.Sleep(50 * time.Millisecond) // OTHER: simulated processing duration
		return nil
	})
	return s, &concurrentPeak
}

// TestRouteRunner_ConcurrentDelivery validates that multiple messages
// are processed concurrently up to MaxInFlight.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────
//
//	MaxInFlight=5, emit 10 messages with 50ms processing each.
//	With concurrency, peak > 1 goroutine active simultaneously.
//
// ───────────────────────────────────────────────────────────────────
func TestRouteRunner_ConcurrentDelivery(t *testing.T) {
	sender, peak := makePeakTracker()

	receiver := NewFakeReceiver()
	runner := goruntime.NewRouteRunnerFromConfig(goruntime.RouteRunnerConfig{
		RouteID:  "concurrent-route",
		Policy:   domain.RoutePolicy{MaxInFlight: 5, DeliveryMode: domain.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runWg sync.WaitGroup
	runWg.Add(1)
	go func() {
		defer runWg.Done()
		_ = runner.Run(ctx)
	}()

	<-receiver.Ready()

	var emitWg sync.WaitGroup
	for i := 0; i < 10; i++ {
		emitWg.Add(1)
		go func(n int) {
			defer emitWg.Done()
			env := &domain.Envelope{ID: "conc-msg-" + string(rune('a'+n)), Payload: []byte("x")}
			del := NewFakeDelivery(env)
			_ = receiver.Emit(ctx, del)
		}(i)
	}

	emitWg.Wait()
	cancel()
	runWg.Wait()

	p := atomic.LoadInt64(peak)
	if p < 2 {
		t.Errorf("expected concurrent delivery (peak=%d), messages processed sequentially", p)
	}
	if p > 5 {
		t.Errorf("MaxInFlight=5 violated: peak concurrent=%d", p)
	}
}

// TestRouteRunner_BackpressureOnSemFull validates that emit blocks when
// MaxInFlight goroutines are all busy, providing backpressure.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────
//
//	MaxInFlight=2, processing takes 100ms per message.
//	Emit 4 messages: first 2 start immediately, 3rd blocks until
//	a slot frees up. Peak never exceeds 2.
//
// ───────────────────────────────────────────────────────────────────
func TestRouteRunner_BackpressureOnSemFull(t *testing.T) {
	var concurrentPeak int64
	var currentConcurrency int64

	sender := NewConcurrentSender(func(_ *domain.Envelope) error {
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
		time.Sleep(100 * time.Millisecond) // OTHER: simulated processing duration
		return nil
	})

	receiver := NewFakeReceiver()
	runner := goruntime.NewRouteRunnerFromConfig(goruntime.RouteRunnerConfig{
		RouteID:  "backpressure-route",
		Policy:   domain.RoutePolicy{MaxInFlight: 2, DeliveryMode: domain.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runWg sync.WaitGroup
	runWg.Add(1)
	go func() {
		defer runWg.Done()
		_ = runner.Run(ctx)
	}()

	<-receiver.Ready()

	var emitWg sync.WaitGroup
	for i := 0; i < 4; i++ {
		emitWg.Add(1)
		go func(n int) {
			defer emitWg.Done()
			env := &domain.Envelope{ID: "bp-" + string(rune('a'+n)), Payload: []byte("x")}
			del := NewFakeDelivery(env)
			_ = receiver.Emit(ctx, del)
		}(i)
	}

	emitWg.Wait()
	waitFor(t, 2*time.Second, "all 4 sends complete", func() bool { return sender.SentCount() >= 4 })

	cancel()
	runWg.Wait()

	p := atomic.LoadInt64(&concurrentPeak)
	if p > 2 {
		t.Errorf("backpressure violated: peak=%d, MaxInFlight=2", p)
	}
}

// TestRouteRunner_GracefulShutdownWaitsInFlight validates that Run waits
// for all in-flight goroutines before returning.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────
//
//	Emit a message that takes 200ms to process, then cancel context.
//	Verify Run does not return until the in-flight message finishes.
//
// ───────────────────────────────────────────────────────────────────
func TestRouteRunner_GracefulShutdownWaitsInFlight(t *testing.T) {
	var sendCompleted int64

	sender := NewConcurrentSender(func(_ *domain.Envelope) error {
		time.Sleep(200 * time.Millisecond) // OTHER: simulated processing duration
		atomic.StoreInt64(&sendCompleted, 1)
		return nil
	})

	receiver := NewFakeReceiver()
	runner := goruntime.NewRouteRunnerFromConfig(goruntime.RouteRunnerConfig{
		RouteID:  "shutdown-route",
		Policy:   domain.RoutePolicy{MaxInFlight: 5, DeliveryMode: domain.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
	})

	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan struct{})
	go func() {
		_ = runner.Run(ctx)
		close(runDone)
	}()

	<-receiver.Ready()

	env := &domain.Envelope{ID: "shutdown-msg", Payload: []byte("data")}
	del := NewFakeDelivery(env)

	go func() {
		_ = receiver.Emit(ctx, del)
	}()

	time.Sleep(50 * time.Millisecond) // STARTUP: let emit reach receiver before cancel
	cancel()

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s after cancel")
	}

	if atomic.LoadInt64(&sendCompleted) != 1 {
		t.Error("Run returned before in-flight message completed")
	}
}

// TestGlobalSemaphore_LimitsCrossRoute validates a global semaphore
// limits total concurrent operations across multiple routes.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────
//
//	2 routes, each MaxInFlight=5, globalMaxInFlight=3.
//	Each route emits 3 messages (6 total). Peak concurrency ≤ 3.
//
// ───────────────────────────────────────────────────────────────────
func TestGlobalSemaphore_LimitsCrossRoute(t *testing.T) {
	var concurrentPeak int64
	var currentConcurrency int64

	makeSender := func() *ConcurrentSender {
		return NewConcurrentSender(func(_ *domain.Envelope) error {
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
			time.Sleep(50 * time.Millisecond) // OTHER: simulated processing duration
			return nil
		})
	}

	globalSem := make(chan struct{}, 3)

	receiver1 := NewFakeReceiver()
	receiver2 := NewFakeReceiver()

	runner1 := goruntime.NewRouteRunnerFromConfig(goruntime.RouteRunnerConfig{
		RouteID:   "global-route-1",
		Policy:    domain.RoutePolicy{MaxInFlight: 5, DeliveryMode: domain.DeliveryDirectHold},
		Receiver:  receiver1,
		Sender:    makeSender(),
		GlobalSem: globalSem,
	})
	runner2 := goruntime.NewRouteRunnerFromConfig(goruntime.RouteRunnerConfig{
		RouteID:   "global-route-2",
		Policy:    domain.RoutePolicy{MaxInFlight: 5, DeliveryMode: domain.DeliveryDirectHold},
		Receiver:  receiver2,
		Sender:    makeSender(),
		GlobalSem: globalSem,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runWg sync.WaitGroup
	runWg.Add(2)
	go func() { defer runWg.Done(); _ = runner1.Run(ctx) }()
	go func() { defer runWg.Done(); _ = runner2.Run(ctx) }()

	<-receiver1.Ready()
	<-receiver2.Ready()

	var emitWg sync.WaitGroup
	for i := 0; i < 3; i++ {
		emitWg.Add(2)
		go func(n int) {
			defer emitWg.Done()
			env := &domain.Envelope{ID: "g1-" + string(rune('a'+n)), Payload: []byte("x")}
			_ = receiver1.Emit(ctx, NewFakeDelivery(env))
		}(i)
		go func(n int) {
			defer emitWg.Done()
			env := &domain.Envelope{ID: "g2-" + string(rune('a'+n)), Payload: []byte("x")}
			_ = receiver2.Emit(ctx, NewFakeDelivery(env))
		}(i)
	}

	emitWg.Wait()
	time.Sleep(200 * time.Millisecond) // SYNC: let in-flight sends complete for peak measurement
	cancel()
	runWg.Wait()

	p := atomic.LoadInt64(&concurrentPeak)
	if p > 3 {
		t.Errorf("global semaphore violated: peak=%d, limit=3", p)
	}
}

// TestGlobalSemaphore_ZeroDisablesGlobal validates that when no global
// semaphore is configured, there is no cross-route throttling.
func TestGlobalSemaphore_ZeroDisablesGlobal(t *testing.T) {
	sender, peak := makePeakTracker()

	receiver := NewFakeReceiver()
	runner := goruntime.NewRouteRunnerFromConfig(goruntime.RouteRunnerConfig{
		RouteID:  "no-global-route",
		Policy:   domain.RoutePolicy{MaxInFlight: 10, DeliveryMode: domain.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runWg sync.WaitGroup
	runWg.Add(1)
	go func() { defer runWg.Done(); _ = runner.Run(ctx) }()

	<-receiver.Ready()

	var emitWg sync.WaitGroup
	for i := 0; i < 8; i++ {
		emitWg.Add(1)
		go func(n int) {
			defer emitWg.Done()
			env := &domain.Envelope{ID: "ng-" + string(rune('a'+n)), Payload: []byte("x")}
			_ = receiver.Emit(ctx, NewFakeDelivery(env))
		}(i)
	}

	emitWg.Wait()
	waitFor(t, 2*time.Second, "all 8 sends complete", func() bool { return sender.SentCount() >= 8 })
	cancel()
	runWg.Wait()

	p := atomic.LoadInt64(peak)
	if p < 2 {
		t.Errorf("expected concurrent processing without global limit, got peak=%d", p)
	}
}
