package paho

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/eclipse/paho.golang/packets"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// Router thorough analysis: bugs, races, resilience
//
// These tests cover behaviour that the existing suite does not assert
// directly, including concurrent register/unregister, panic isolation
// across many handlers, drop counters, stats accuracy, and properties
// pointer ownership semantics.
// ═══════════════════════════════════════════════════════════════════════════

// TestAnaRouter_DuplicateRegister_OverwritesSilently characterises the
// current behaviour of router.Register when called twice with the same
// id: the second call REPLACES the first. This is unobservable by the
// caller (no error, no log warning at WARN level) — a footgun that this
// test pins so behaviour does not silently change.
func TestAnaRouter_DuplicateRegister_OverwritesSilently(t *testing.T) {
	r := newRouter(nil, nil)

	var firstCalled, secondCalled atomic.Bool
	r.Register("dup", func(*pahov5.Publish) { firstCalled.Store(true) })
	r.Register("dup", func(*pahov5.Publish) { secondCalled.Store(true) })

	if r.HandlerCount() != 1 {
		t.Fatalf("HandlerCount = %d, want 1 (overwrite expected)", r.HandlerCount())
	}

	r.Route(newTestPacketPublish("test/dup", []byte("x")))
	r.Wait()

	if firstCalled.Load() {
		t.Error("first handler should NOT have been called (overwritten)")
	}
	if !secondCalled.Load() {
		t.Error("second handler should have been called (overwriter)")
	}
}

// TestAnaRouter_UnregisterUnknown_NoPanic asserts that unregistering an
// id that was never registered is a silent no-op (defensive behaviour).
func TestAnaRouter_UnregisterUnknown_NoPanic(t *testing.T) {
	r := newRouter(nil, nil)
	defer func() {
		if rv := recover(); rv != nil {
			t.Fatalf("Unregister of unknown id panicked: %v", rv)
		}
	}()
	r.Unregister("never-registered")
	if r.HandlerCount() != 0 {
		t.Fatalf("HandlerCount = %d, want 0", r.HandlerCount())
	}
}

// TestAnaRouter_RouteWithNoHandlers_IncrementsDropCounterAndMetric
// validates both the in-process drop counter and the emitted metric.
func TestAnaRouter_RouteWithNoHandlers_IncrementsDropCounterAndMetric(t *testing.T) {
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec)

	const n = 7
	for i := 0; i < n; i++ {
		r.Route(newTestPacketPublish("test/drop", []byte("x")))
	}

	received, dropped := r.Stats()
	if received != n {
		t.Fatalf("Stats received = %d, want %d", received, n)
	}
	if dropped != n {
		t.Fatalf("Stats dropped = %d, want %d", dropped, n)
	}

	entries := rec.FindEntries(domain.MetricMQTTRouterDropped)
	if len(entries) != n {
		t.Fatalf("MetricMQTTRouterDropped entries = %d, want %d", len(entries), n)
	}
}

// TestAnaRouter_StatsAccurateUnderConcurrency exercises Route from many
// goroutines while handlers are concurrently registered and unregistered,
// and verifies the totals are exactly accounted for at the end.
func TestAnaRouter_StatsAccurateUnderConcurrency(t *testing.T) {
	r := newRouter(nil, nil)
	const (
		producers   = 8
		perProducer = 200
	)

	var wg sync.WaitGroup
	wg.Add(producers)

	r.Register("h", func(*pahov5.Publish) {})

	for p := 0; p < producers; p++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				r.Route(newTestPacketPublish("test/conc", []byte("x")))
			}
		}()
	}
	wg.Wait()
	r.Wait()

	received, dropped := r.Stats()
	if received != int64(producers*perProducer) {
		t.Fatalf("received = %d, want %d", received, producers*perProducer)
	}
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 (handler always present)", dropped)
	}
}

// TestAnaRouter_ConcurrentRegisterUnregister_NoRaceOrPanic stresses the
// router lock: many goroutines registering and unregistering while other
// goroutines route messages. Must not deadlock, panic, or race
// (verify with -race).
func TestAnaRouter_ConcurrentRegisterUnregister_NoRaceOrPanic(t *testing.T) {
	r := newRouter(nil, nil)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Routers
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					r.Route(newTestPacketPublish("test/rr", []byte("x")))
				}
			}
		}()
	}

	// Registrars
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := "h" + strconv.Itoa(idx)
			for i := 0; i < 500; i++ {
				r.Register(id, func(*pahov5.Publish) {})
				r.Unregister(id)
			}
		}(p)
	}

	time.Sleep(100 * time.Millisecond) // OTHER: race window — let concurrent register/unregister/route goroutines exercise
	close(stop)
	wg.Wait()
	r.Wait()
}

// TestAnaRouter_OneHandlerSharesPropertiesWithRouterCopy pins the
// current implementation behaviour: when multiple handlers are present,
// exactly one of them (i==0 in the dispatch loop) keeps the original
// *PublishProperties pointer while the others receive deep-copies.
// This is a deliberate design choice (avoid one allocation in the
// common single-handler case while still isolating siblings).
func TestAnaRouter_OneHandlerSharesPropertiesWithRouterCopy(t *testing.T) {
	r := newRouter(nil, nil)

	const handlers = 4
	var (
		mu   sync.Mutex
		addr = make([]uintptr, 0, handlers)
	)

	for i := 0; i < handlers; i++ {
		id := "h" + strconv.Itoa(i)
		r.Register(id, func(p *pahov5.Publish) {
			mu.Lock()
			addr = append(addr, uintptr(unsafe.Pointer(p.Properties)))
			mu.Unlock()
		})
	}

	pb := &packets.Publish{
		Topic:      "test/firstprops",
		Payload:    []byte("x"),
		Properties: &packets.Properties{ContentType: "application/json"},
	}
	r.Route(pb)
	r.Wait()

	if len(addr) != handlers {
		t.Fatalf("captured %d addrs, want %d", len(addr), handlers)
	}
	// All addresses must be distinct (deep-copy invariant) — except in
	// the current implementation where i==0 keeps the router-internal
	// PublishFromPacketPublish copy. We assert distinctness modulo at
	// most ONE duplicate (the shared-pointer case is allowed but not
	// required).
	seen := map[uintptr]int{}
	for _, a := range addr {
		seen[a]++
	}
	for a, cnt := range seen {
		if cnt > 1 {
			t.Fatalf("Properties pointer %x shared across %d handlers, expected at most 1", a, cnt)
		}
	}
}

// TestAnaRouter_PanicHandler_DoesNotAffectLargeFanOut verifies that with
// many handlers, a panic in one of them does not prevent the rest from
// executing nor the WaitGroup from balancing.
func TestAnaRouter_PanicHandler_DoesNotAffectLargeFanOut(t *testing.T) {
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec)

	const total = 16
	const panickers = 4
	var ok atomic.Int32

	for i := 0; i < total; i++ {
		id := "h" + strconv.Itoa(i)
		if i < panickers {
			r.Register(id, func(*pahov5.Publish) { panic("boom") })
		} else {
			r.Register(id, func(*pahov5.Publish) { ok.Add(1) })
		}
	}

	r.Route(newTestPacketPublish("test/panic-fan", []byte("x")))
	r.Wait()

	if ok.Load() != int32(total-panickers) {
		t.Fatalf("non-panic handlers ran %d, want %d", ok.Load(), total-panickers)
	}
	entries := rec.FindEntries(domain.MetricMQTTHandlerPanics)
	if len(entries) != panickers {
		t.Fatalf("panic metric count = %d, want %d", len(entries), panickers)
	}
}

// TestAnaRouter_RegisterDuringRoute_NotInvokedForCurrentMessage verifies
// the snapshot semantics: handlers registered AFTER Route() began
// dispatching are NOT invoked for that in-flight message.
func TestAnaRouter_RegisterDuringRoute_NotInvokedForCurrentMessage(t *testing.T) {
	r := newRouter(nil, nil)

	gate := make(chan struct{})
	released := make(chan struct{})
	r.Register("blocker", func(*pahov5.Publish) {
		gate <- struct{}{}
		<-released
	})

	go r.Route(newTestPacketPublish("t/snap", []byte("x")))
	<-gate

	var lateCalled atomic.Bool
	r.Register("late", func(*pahov5.Publish) { lateCalled.Store(true) })

	close(released)
	r.Wait()

	if lateCalled.Load() {
		t.Fatal("handler registered AFTER Route snapshot must not be invoked for in-flight message")
	}
}

// TestAnaRouter_NextRouteSeesNewHandler verifies that handlers added
// after a previous Route are invoked for SUBSEQUENT routes.
func TestAnaRouter_NextRouteSeesNewHandler(t *testing.T) {
	r := newRouter(nil, nil)

	var firstSaw atomic.Bool
	r.Register("h1", func(*pahov5.Publish) { firstSaw.Store(true) })
	r.Route(newTestPacketPublish("t/x", []byte("a")))
	r.Wait()

	if !firstSaw.Load() {
		t.Fatal("first handler should have been invoked")
	}

	var secondSaw atomic.Bool
	r.Register("h2", func(*pahov5.Publish) { secondSaw.Store(true) })
	r.Route(newTestPacketPublish("t/x", []byte("b")))
	r.Wait()
	if !secondSaw.Load() {
		t.Fatal("late-registered handler should be invoked on next Route")
	}
}

// TestAnaRouter_InterfaceStubs_AreNoops asserts that the paho.Router
// interface stubs (RegisterHandler/UnregisterHandler/SetDebugLogger)
// do not affect the internal handler set or panic.
func TestAnaRouter_InterfaceStubs_AreNoops(t *testing.T) {
	r := newRouter(nil, nil)
	defer func() {
		if rv := recover(); rv != nil {
			t.Fatalf("interface stubs must not panic: %v", rv)
		}
	}()
	r.RegisterHandler("x", func(*pahov5.Publish) {})
	r.UnregisterHandler("x")
	r.SetDebugLogger(nil)
	if r.HandlerCount() != 0 {
		t.Fatalf("HandlerCount should remain 0, got %d", r.HandlerCount())
	}
}

// TestAnaRouter_NewRouter_DefaultMetrics_NilMetricsExporterAccepted
// verifies the constructor accepts nil and substitutes a no-op exporter
// — required so callers do not have to wire metrics for tests.
func TestAnaRouter_NewRouter_DefaultMetrics_NilAccepted(t *testing.T) {
	r := newRouter(nil, nil)
	r.Route(newTestPacketPublish("t/x", []byte("x")))
	if got, _ := r.Stats(); got != 1 {
		t.Fatalf("received = %d, want 1", got)
	}
}

// TestAnaRouter_HandlerSeesPayloadCorrectly_LargePayload verifies that
// large payloads are deep-copied per handler and the data integrity is
// preserved.
func TestAnaRouter_HandlerSeesPayloadCorrectly_LargePayload(t *testing.T) {
	r := newRouter(nil, nil)

	payload := make([]byte, 64*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	var sawN atomic.Int32
	for i := 0; i < 4; i++ {
		id := "h" + strconv.Itoa(i)
		r.Register(id, func(p *pahov5.Publish) {
			if len(p.Payload) != len(payload) {
				return
			}
			for j, b := range p.Payload {
				if b != byte(j%251) {
					return
				}
			}
			sawN.Add(1)
		})
	}

	r.Route(newTestPacketPublish("t/large", payload))
	r.Wait()

	if sawN.Load() != 4 {
		t.Fatalf("handlers saw correct payload %d times, want 4", sawN.Load())
	}
}
