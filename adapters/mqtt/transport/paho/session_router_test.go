package paho

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/packets"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain"
)

func newTestPacketPublish(topic string, payload []byte) *packets.Publish {
	return &packets.Publish{
		Topic:      topic,
		Payload:    payload,
		Properties: &packets.Properties{},
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// T3 Regression: Router goroutine tracking via WaitGroup
//
// These tests verify that router.Route() tracks spawned handler goroutines
// so that Wait() (and thus Session.Close) blocks until all are complete.
//
//   ┌──────────┐  Route()   ┌───────────────┐
//   │  router  │───────────▶│ goroutine x N │──▶ wg.Done()
//   └──────────┘            └───────────────┘
//        │
//        ▼
//   Wait() blocks until all goroutines finish
// ═══════════════════════════════════════════════════════════════════════════

// TestRouter_Wait_BlocksUntilHandlersComplete validates that Wait() blocks
// until all in-flight handler goroutines spawned by Route() have returned.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	Register 3 slow handlers (sleep 100ms each)
//	Call Route() → spawns 3 goroutines
//	Call Wait() → must block until all 3 complete
//
// ───────────────────────────────────────────────
//
// Assertions:
//   - All 3 handlers were invoked
//   - Wait() does not return before all handlers complete
func TestRouter_Wait_BlocksUntilHandlersComplete(t *testing.T) {
	r := newRouter(nil, nil)
	var completed atomic.Int32

	for i := 0; i < 3; i++ {
		id := string(rune('a' + i))
		r.Register(id, func(_ *pahov5.Publish) {
			time.Sleep(100 * time.Millisecond) // OTHER: simulated slow handler work
			completed.Add(1)
		})
	}

	pb := newTestPacketPublish("test/topic", []byte("hello"))
	r.Route(pb)

	// Wait must block until all 3 handlers finish.
	r.Wait()

	if c := completed.Load(); c != 3 {
		t.Fatalf("expected 3 handlers completed, got %d", c)
	}
}

// TestRouter_ShallowCopy_DistinctPointers validates that each handler
// goroutine receives a distinct *Publish pointer (shallow copy) so that
// concurrent handlers do not share the same struct instance.
//
// Assertions:
//   - Each handler receives a non-nil *Publish
//   - No two handlers receive the same pointer
func TestRouter_ShallowCopy_DistinctPointers(t *testing.T) {
	r := newRouter(nil, nil)
	var mu sync.Mutex
	pointers := make(map[*pahov5.Publish]bool)

	for i := 0; i < 3; i++ {
		id := string(rune('a' + i))
		r.Register(id, func(pub *pahov5.Publish) {
			mu.Lock()
			pointers[pub] = true
			mu.Unlock()
		})
	}

	pb := newTestPacketPublish("test/topic", []byte("hello"))
	r.Route(pb)
	r.Wait()

	if len(pointers) != 3 {
		t.Fatalf("expected 3 distinct pointers, got %d (sharing detected)", len(pointers))
	}
}

// TestRouter_Close_WaitsForInflightHandlers validates that Session.Close
// blocks until all in-flight router handler goroutines have completed
// before closing the events channel.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	Register a slow handler that takes 200ms
//	Dispatch a message via Route()
//	Immediately call Close()
//	Close must not return until the handler finishes
//
// ───────────────────────────────────────────────
//
// Assertions:
//   - Handler was invoked and completed
//   - Close completes without panic
//   - Events channel is closed after Close returns
func TestRouter_Close_WaitsForInflightHandlers(t *testing.T) {
	s := NewSession(
		SessionOptions{BrokerURLs: []string{"tcp://localhost:1883"}, ClientID: "test"},
		domain.SessionEphemeral,
		nil,
	)

	var handlerDone atomic.Bool
	s.router.Register("slow", func(_ *pahov5.Publish) {
		// OTHER: simulated slow handler — tests that Close waits for in-flight work.
		time.Sleep(200 * time.Millisecond)
		handlerDone.Store(true)
	})

	pb := newTestPacketPublish("test/topic", []byte("data"))
	s.router.Route(pb)

	// Close should block until the slow handler finishes.
	_ = s.Close(context.Background())

	if !handlerDone.Load() {
		t.Fatal("handler should have completed before Close returned")
	}

	// Events channel should be closed.
	select {
	case _, ok := <-s.Events():
		if ok {
			t.Fatal("expected events channel to be closed after Close")
		}
	default:
		t.Fatal("expected closed channel to be readable (zero value)")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// M2: Close() respects ctx deadline when handlers block
// ═══════════════════════════════════════════════════════════════════════════

// TestRouter_Close_RespectsCtxDeadline validates that Session.Close returns
// within the ctx deadline even when a handler blocks indefinitely.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	Register a handler that blocks for 5s
//	Call Close with 100ms context deadline
//	Close must return within ~200ms (ctx expired)
//
// ───────────────────────────────────────────────
//
// Assertions:
//   - Close returns within 300ms
//   - Events channel is still closed
func TestRouter_Close_RespectsCtxDeadline(t *testing.T) {
	s := NewSession(
		SessionOptions{BrokerURLs: []string{"tcp://localhost:1883"}, ClientID: "test-ctx"},
		domain.SessionEphemeral,
		nil,
	)

	s.router.Register("blocker", func(_ *pahov5.Publish) {
		time.Sleep(5 * time.Second) // OTHER: simulated blocking handler for Close deadline test
	})

	pb := newTestPacketPublish("test/topic", []byte("data"))
	s.router.Route(pb)

	closeCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_ = s.Close(closeCtx)
	elapsed := time.Since(start)

	if elapsed > 300*time.Millisecond {
		t.Fatalf("Close should have returned within ctx deadline, took %v", elapsed)
	}

	// Events channel should still be closed even on ctx timeout.
	select {
	case _, ok := <-s.Events():
		if ok {
			t.Fatal("expected events channel to be closed after Close")
		}
	default:
		t.Fatal("expected closed channel to be readable (zero value)")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// L5: Router handler panic recovery
// ═══════════════════════════════════════════════════════════════════════════

// TestRouter_HandlerPanic_DoesNotCrash validates that a panicking handler
// does not crash the process and that Wait() still returns.
//
// Assertions:
//   - No process crash
//   - Wait() returns (WaitGroup is still balanced)
//   - Other handlers complete normally
func TestRouter_HandlerPanic_DoesNotCrash(t *testing.T) {
	r := newRouter(nil, nil)
	var normalDone atomic.Bool

	r.Register("panicker", func(_ *pahov5.Publish) {
		panic("handler panic")
	})
	r.Register("normal", func(_ *pahov5.Publish) {
		time.Sleep(50 * time.Millisecond) // OTHER: simulated slow handler work alongside panicking sibling
		normalDone.Store(true)
	})

	pb := newTestPacketPublish("test/topic", []byte("data"))
	r.Route(pb)
	r.Wait()

	if !normalDone.Load() {
		t.Fatal("normal handler should complete despite sibling panic")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// T23: Payload deep-copy isolation
//
// Each handler goroutine must receive an independent copy of the Payload
// byte slice so that mutations in one handler cannot affect another.
//
//   pub.Payload ──copy──▶ handler A payload (independent)
//                ──copy──▶ handler B payload (independent)
// ═══════════════════════════════════════════════════════════════════════════

// TestRouter_Route_PayloadDeepCopy validates that mutating Payload in
// one handler does not affect the Payload seen by another handler.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	Handler A: sets Payload[0] = 'X'
//	Handler B: reads Payload[0]
//	Expected: Handler B sees original byte, not 'X'
//
// ───────────────────────────────────────────────
func TestRouter_Route_PayloadDeepCopy(t *testing.T) {
	r := newRouter(nil, nil)
	original := []byte("hello")

	handlerADone := make(chan struct{})
	var handlerBPayload []byte
	var handlerBDone atomic.Bool

	r.Register("a", func(pub *pahov5.Publish) {
		pub.Payload[0] = 'X'
		close(handlerADone)
	})
	r.Register("b", func(pub *pahov5.Publish) {
		<-handlerADone
		handlerBPayload = make([]byte, len(pub.Payload))
		copy(handlerBPayload, pub.Payload)
		handlerBDone.Store(true)
	})

	pb := newTestPacketPublish("test/deep-copy", original)
	r.Route(pb)
	r.Wait()

	if !handlerBDone.Load() {
		t.Fatal("handler B should have completed")
	}
	if string(handlerBPayload) != "hello" {
		t.Fatalf("handler B saw mutated payload %q, expected %q", handlerBPayload, "hello")
	}
}

// TestRouter_Route_NilPayload validates that routing a message with
// nil Payload does not panic and handlers receive nil.
func TestRouter_Route_NilPayload(t *testing.T) {
	r := newRouter(nil, nil)
	var received atomic.Bool
	var receivedNil atomic.Bool

	r.Register("nil-handler", func(pub *pahov5.Publish) {
		received.Store(true)
		if pub.Payload == nil {
			receivedNil.Store(true)
		}
	})

	pb := newTestPacketPublish("test/nil-payload", nil)
	r.Route(pb)
	r.Wait()

	if !received.Load() {
		t.Fatal("handler should have been called")
	}
	if !receivedNil.Load() {
		t.Fatal("handler should have received nil Payload")
	}
}

// TestRouter_Route_EmptyPayload validates that an empty (zero-length)
// Payload is deep-copied correctly.
func TestRouter_Route_EmptyPayload(t *testing.T) {
	r := newRouter(nil, nil)
	var received atomic.Bool

	r.Register("empty-handler", func(pub *pahov5.Publish) {
		received.Store(true)
		if pub.Payload == nil {
			t.Error("expected non-nil empty payload after deep copy")
		}
		if len(pub.Payload) != 0 {
			t.Errorf("expected empty payload, got len=%d", len(pub.Payload))
		}
	})

	pb := newTestPacketPublish("test/empty-payload", []byte{})
	r.Route(pb)
	r.Wait()

	if !received.Load() {
		t.Fatal("handler should have been called")
	}
}

// TestRouter_Route_OriginalPayloadUnmutated validates that the original
// Publish.Payload bytes are not affected by handler mutations.
func TestRouter_Route_OriginalPayloadUnmutated(t *testing.T) {
	r := newRouter(nil, nil)
	original := []byte("immutable")
	originalCopy := make([]byte, len(original))
	copy(originalCopy, original)

	r.Register("mutator", func(pub *pahov5.Publish) {
		for i := range pub.Payload {
			pub.Payload[i] = 'Z'
		}
	})

	pb := newTestPacketPublish("test/orig-safe", original)
	r.Route(pb)
	r.Wait()

	if string(original) != string(originalCopy) {
		t.Fatalf("original payload was mutated: got %q, want %q", original, originalCopy)
	}
}

// TestRouter_Route_ConcurrentHandlers_IndependentPayloads validates
// that under concurrent dispatch with multiple handlers, each handler
// receives an independent payload copy (safe under -race).
func TestRouter_Route_ConcurrentHandlers_IndependentPayloads(t *testing.T) {
	r := newRouter(nil, nil)
	const numHandlers = 10
	payload := []byte("concurrent-test-payload")
	var completedCount atomic.Int32

	for i := 0; i < numHandlers; i++ {
		id := "h-" + strconv.Itoa(i)
		r.Register(id, func(pub *pahov5.Publish) {
			for j := range pub.Payload {
				pub.Payload[j] = byte(j % 256)
			}
			completedCount.Add(1)
		})
	}

	pb := newTestPacketPublish("test/concurrent", payload)
	r.Route(pb)
	r.Wait()

	if got := completedCount.Load(); got != numHandlers {
		t.Fatalf("expected %d handlers completed, got %d", numHandlers, got)
	}
}

// TestRouter_Route_ConcurrentPropertiesRead validates that concurrent
// handlers reading shared Properties fields do not race (safe under -race).
func TestRouter_Route_ConcurrentPropertiesRead(t *testing.T) {
	r := newRouter(nil, nil)
	const numHandlers = 5
	var completedCount atomic.Int32

	for i := 0; i < numHandlers; i++ {
		id := "p-" + strconv.Itoa(i)
		r.Register(id, func(pub *pahov5.Publish) {
			_ = pub.Properties
			if pub.Properties != nil {
				_ = pub.Properties.ContentType
				_ = pub.Properties.ResponseTopic
			}
			completedCount.Add(1)
		})
	}

	pb := &packets.Publish{
		Topic:   "test/props",
		Payload: []byte("props-test"),
		Properties: &packets.Properties{
			ContentType:   "application/json",
			ResponseTopic: "reply/topic",
		},
	}
	r.Route(pb)
	r.Wait()

	if got := completedCount.Load(); got != int32(numHandlers) {
		t.Fatalf("expected %d handlers completed, got %d", numHandlers, got)
	}
}
