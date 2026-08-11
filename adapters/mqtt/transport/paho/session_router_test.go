package paho

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/eclipse/paho.golang/packets"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

func newTestPacketPublish(topic string, payload []byte) *packets.Publish {
	return &packets.Publish{
		Topic:      topic,
		Payload:    payload,
		Properties: &packets.Properties{},
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Finding #2: Synchronous dispatch (backpressure + deferred ACK)
//
// Route must block until every handler for a publish has returned. The Paho
// client only sends the PUBACK/PUBCOMP after Route unwinds, so blocking here
// applies broker backpressure and defers the protocol ACK until the handler
// (emit) has taken ownership.
// ═══════════════════════════════════════════════════════════════════════════

// TestRouter_Route_IsSynchronous_BlocksUntilHandlersComplete validates that
// Route does not return until all handler goroutines it spawned have
// completed. Neuter-and-verify: against the previous goroutine-per-publish
// implementation Route returned immediately and the negative select below
// would fire.
func TestRouter_Route_IsSynchronous_BlocksUntilHandlersComplete(t *testing.T) {
	r := newRouter(nil, nil)

	const handlers = 3
	started := make(chan struct{}, handlers)
	release := make(chan struct{})
	var completed atomic.Int32

	for i := 0; i < handlers; i++ {
		id := string(rune('a' + i))
		r.Register(id, func(_ *pahov5.Publish) {
			started <- struct{}{}
			<-release
			completed.Add(1)
		})
	}

	pb := newTestPacketPublish("test/topic", []byte("hello"))
	routeReturned := make(chan struct{})
	go func() {
		r.Route(pb)
		close(routeReturned)
	}()

	// Every handler must be dispatched (concurrently) before Route returns.
	for i := 0; i < handlers; i++ {
		<-started
	}
	// Route is still blocked: it dispatches synchronously and waits for
	// every handler. This select is deterministically the default branch
	// on correct code because routeReturned cannot close until we release.
	select {
	case <-routeReturned:
		t.Fatal("Route returned before handlers completed; dispatch must be synchronous")
	default:
	}

	close(release)
	<-routeReturned // Route returns once handlers finish.

	if c := completed.Load(); c != handlers {
		t.Fatalf("expected %d handlers completed, got %d", handlers, c)
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
// Deterministic design: a handler signals it is in-flight (started) and
// then parks on a release channel. Close is started in a goroutine; it
// must not return while the handler is in-flight. Releasing the handler
// lets Close complete. handlerDone is guaranteed observed once Close
// returns because the handler's wg.Done runs only after it sets the flag.
//
// Assertions:
//   - Close does not return while a handler is in-flight
//   - Handler completed before Close returned
//   - Events channel is closed after Close returns
func TestRouter_Close_WaitsForInflightHandlers(t *testing.T) {
	s := NewSession(
		SessionOptions{BrokerURLs: []string{"tcp://localhost:1883"}, ClientID: "test"},
		connectivity.SessionEphemeral,
		nil,
	)

	started := make(chan struct{})
	release := make(chan struct{})
	var handlerDone atomic.Bool
	s.router.Register("slow", func(_ *pahov5.Publish) {
		close(started)
		<-release
		handlerDone.Store(true)
	})

	pb := newTestPacketPublish("test/topic", []byte("data"))
	go s.router.Route(pb)
	<-started // handler is in-flight (counted in the shared WaitGroup)

	closeReturned := make(chan struct{})
	go func() {
		_ = s.Close(context.Background())
		close(closeReturned)
	}()

	// Close must block on the in-flight handler — deterministically the
	// default branch because closeReturned cannot close before release.
	select {
	case <-closeReturned:
		t.Fatal("Close returned while a handler was still in-flight")
	default:
	}

	close(release)
	<-closeReturned

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
// Close() respects ctx deadline when handlers block
// ═══════════════════════════════════════════════════════════════════════════

// TestRouter_Close_RespectsCtxDeadline validates that Session.Close returns
// when its context is cancelled even though a handler is still in-flight,
// rather than blocking on the handler indefinitely.
//
// Deterministic design: the handler parks on a release channel that is only
// closed at test cleanup, so it never finishes on its own. Close is invoked
// with a cancellable context; cancelling it must unblock Close. Against
// broken code (Close ignoring ctx) this test blocks and the harness times
// out.
//
// Assertions:
//   - Close returns after ctx cancel without the handler completing
//   - Events channel is still closed even on ctx cancellation
func TestRouter_Close_RespectsCtxDeadline(t *testing.T) {
	s := NewSession(
		SessionOptions{BrokerURLs: []string{"tcp://localhost:1883"}, ClientID: "test-ctx"},
		connectivity.SessionEphemeral,
		nil,
	)

	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // unblock the parked handler at test end
	s.router.Register("blocker", func(_ *pahov5.Publish) {
		close(started)
		<-release
	})

	pb := newTestPacketPublish("test/topic", []byte("data"))
	go s.router.Route(pb)
	<-started // handler is in-flight and will not finish on its own

	ctx, cancel := context.WithCancel(context.Background())
	closeReturned := make(chan error, 1)
	go func() { closeReturned <- s.Close(ctx) }()

	// Close is blocking on the in-flight handler; cancelling ctx must make
	// it return WITHOUT waiting for the handler to finish.
	cancel()
	<-closeReturned

	// Events channel should still be closed even on ctx cancellation.
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
// Finding #3: Close stops consumption (lease step-down)
// ═══════════════════════════════════════════════════════════════════════════

// TestRouter_Close_WaitsForInflightHandlers (see above) covers the
// at-least-once boundary on Close: in-flight handlers are awaited so a
// publish the broker is still delivering during the disconnect window is
// processed-then-acked rather than dropped. The router has no stop gate —
// dropping-then-acking in-flight publishes would silently lose them
// because the Paho Router seam acks after Route returns (see
// session_lifecycle.go for the rationale).

// ═══════════════════════════════════════════════════════════════════════════
// Router handler panic recovery
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
// Payload deep-copy isolation
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

// TestRouter_Fanout_TransfersOwnedPublishOnce verifies fan-out transfers the
// router-owned publish to exactly one handler and clones only the remaining
// handlers. Every handler must still receive isolated mutable payload and
// properties.
func TestRouter_Fanout_TransfersOwnedPublishOnce(t *testing.T) {
	r := newRouter(nil, nil)
	owned := &pahov5.Publish{
		Topic:   "test/owned-fanout",
		Payload: []byte("payload"),
		Properties: &pahov5.PublishProperties{
			User: pahov5.UserProperties{{Key: "key", Value: "value"}},
		},
	}
	var (
		mu            sync.Mutex
		payloadStarts []*byte
		properties    []*pahov5.PublishProperties
	)
	handlers := make([]routerHandler, 3)
	for i := range handlers {
		handlers[i].fn = func(pub *pahov5.Publish, _ func() error) {
			mu.Lock()
			payloadStarts = append(payloadStarts, &pub.Payload[0])
			properties = append(properties, pub.Properties)
			mu.Unlock()
		}
	}

	r.fanout(owned, nil, handlers)

	mu.Lock()
	defer mu.Unlock()
	ownedPayloadUses := 0
	ownedPropertyUses := 0
	for i := range payloadStarts {
		if payloadStarts[i] == &owned.Payload[0] {
			ownedPayloadUses++
		}
		if properties[i] == owned.Properties {
			ownedPropertyUses++
		}
		for j := i + 1; j < len(payloadStarts); j++ {
			if payloadStarts[i] == payloadStarts[j] || properties[i] == properties[j] {
				t.Fatalf("handlers %d and %d share mutable publish state", i, j)
			}
		}
	}
	if ownedPayloadUses != 1 || ownedPropertyUses != 1 {
		t.Fatalf("router-owned publish uses = payload:%d properties:%d, want exactly one each", ownedPayloadUses, ownedPropertyUses)
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
// Publish.Payload() bytes are not affected by handler mutations.
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
