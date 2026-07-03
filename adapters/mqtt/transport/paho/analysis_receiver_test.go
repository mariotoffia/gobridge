package paho

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ═══════════════════════════════════════════════════════════════════════════
// Receiver behaviour analysis (no real broker required).
//
// The Receiver registers a handler with the Session router, converts
// incoming MQTT publishes to a Delivery, and propagates emit errors
// via a buffered errCh while cancelling the runCtx so the handler
// unblocks. These tests drive the handler directly via the router to
// exercise behaviour without needing autopaho.
// ═══════════════════════════════════════════════════════════════════════════

// TestAnaRecv_RunReturnsCtxErrOnParentCancel verifies that Run returns
// ctx.Err() when the parent context is cancelled (even with no
// messages flowing).
func TestAnaRecv_RunReturnsCtxErrOnParentCancel(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recv-cancel",
	}, connectivity.SessionEphemeral, nil)

	r := NewReceiver("rx-cancel", sess)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
			return nil
		})
	}()

	select {
	case <-r.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not start")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after parent cancel")
	}
}

// TestAnaRecv_EmitError_PropagatedAndCancelsRun verifies that returning
// an error from emit causes Run to return that error (with priority
// over the parent ctx).
func TestAnaRecv_EmitError_PropagatedAndCancelsRun(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recv-emit-err",
	}, connectivity.SessionEphemeral, nil)

	r := NewReceiver("rx-emit-err", sess)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	emitErr := errors.New("emit boom")

	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
			return emitErr
		})
	}()

	select {
	case <-r.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not start")
	}
	sess.Router().Route(newTestPacketPublish("test/emit-err", []byte("p")))

	select {
	case err := <-done:
		if !errors.Is(err, emitErr) {
			t.Fatalf("Run returned %v, want %v", err, emitErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after emit error")
	}
}

// TestAnaRecv_EmitError_DeliveryNotSettled is the MQTT conformance test for
// the ports.Receiver emit-error contract. MQTT settlement is a documented
// no-op (Delivery.Ack returns nil; Retry/Extend return ErrNotSupported —
// see delivery.go), because PUBACK/PUBREC are sent by the paho client before
// the inbound handler runs and there is no application-layer settlement
// handle. So the conformance guarantee for MQTT is: the emit error is
// propagated out of Run (cancelling the run), and the delivery handed to
// emit exposes only the documented no-op settlement (there is no broker
// settle/ack/abandon operation to suppress).
func TestAnaRecv_EmitError_DeliveryNotSettled(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recv-emit-err-nosettle",
	}, connectivity.SessionEphemeral, nil)

	r := NewReceiver("rx-emit-err-nosettle", sess)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	emitErr := errors.New("pipeline rejected delivery")
	var (
		seen ports.Delivery
		mu   sync.Mutex
	)

	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(_ context.Context, del ports.Delivery) error {
			mu.Lock()
			seen = del
			mu.Unlock()
			return emitErr
		})
	}()

	select {
	case <-r.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not start")
	}
	sess.Router().Route(newTestPacketPublish("test/emit-err-nosettle", []byte("p")))

	select {
	case err := <-done:
		if !errors.Is(err, emitErr) {
			t.Fatalf("Run returned %v, want %v (emit error must propagate and cancel Run)", err, emitErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after emit error")
	}

	mu.Lock()
	del := seen
	mu.Unlock()
	if del == nil {
		t.Fatal("emit was never handed a delivery")
	}

	// MQTT settlement is a documented no-op: there is no broker-side
	// settle/ack/abandon to (wrongly) invoke on emit error. Assert the
	// no-op contract holds for the delivery the receiver built.
	if err := del.Ack(context.Background()); err != nil {
		t.Fatalf("MQTT Delivery.Ack must be a no-op returning nil, got %v", err)
	}
	if err := del.Retry(context.Background(), time.Second, emitErr); !errors.Is(err, shared.ErrNotSupported) {
		t.Fatalf("MQTT Delivery.Retry must return ErrNotSupported (no broker redelivery primitive), got %v", err)
	}
}

// TestAnaRecv_HandlerUnregisteredAfterRunReturns verifies the deferred
// Unregister actually removes the handler so it cannot fire after Run
// returns.
func TestAnaRecv_HandlerUnregisteredAfterRunReturns(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recv-unreg",
	}, connectivity.SessionEphemeral, nil)

	r := NewReceiver("rx-unreg", sess)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		_ = r.Run(ctx, func(_ context.Context, _ ports.Delivery) error { return nil })
	}()
	select {
	case <-r.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not start")
	}
	if sess.Router().HandlerCount() != 1 {
		t.Fatalf("HandlerCount = %d, want 1 while Run is alive", sess.Router().HandlerCount())
	}

	cancel()
	wait.Until(t, 5*time.Second, "handler deregistered after cancel",
		func() bool { return sess.Router().HandlerCount() == 0 })

	if sess.Router().HandlerCount() != 0 {
		t.Fatalf("HandlerCount = %d, want 0 after Run returns", sess.Router().HandlerCount())
	}
}

// TestAnaRecv_MessagesArriveAsDeliveries verifies the basic happy path:
// a message routed by the router becomes a Delivery passed to emit.
func TestAnaRecv_MessagesArriveAsDeliveries(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recv-happy",
	}, connectivity.SessionEphemeral, nil)

	r := NewReceiver("rx-happy", sess)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		got     atomic.Int32
		envSeen *messaging.Envelope
		mu      sync.Mutex
	)

	go func() {
		_ = r.Run(ctx, func(_ context.Context, del ports.Delivery) error {
			mu.Lock()
			envSeen = del.Envelope()
			mu.Unlock()
			got.Add(1)
			return nil
		})
	}()

	select {
	case <-r.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not start")
	}
	sess.Router().Route(newTestPacketPublish("test/happy", []byte("payload")))

	deadline := time.After(2 * time.Second)
	for got.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("emit was not invoked")
		case <-time.After(20 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if envSeen == nil {
		t.Fatal("envSeen nil")
	}
	if envSeen.Subject() != "" {
		t.Errorf("subject = %q, want empty (no gobridge.subject user property)", envSeen.Subject())
	}
	if v, _ := messaging.GetHeaderString(envSeen.Headers(), HeaderMQTTTopic); v != "test/happy" {
		t.Errorf("headers[%q] = %q, want test/happy", HeaderMQTTTopic, v)
	}
	if string(envSeen.Payload()) != "payload" {
		t.Errorf("payload = %q, want payload", envSeen.Payload())
	}
}

// TestAnaRecv_DeliveryAck_IsNoop verifies that Delivery.Ack returns nil
// (MQTT acks are handled by paho internally, no caller action needed).
func TestAnaRecv_DeliveryAck_IsNoop(t *testing.T) {
	d := NewDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "x"}))
	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("Ack returned %v, want nil", err)
	}
}

// TestAnaRecv_DeliveryRetry_ReturnsErrNotSupported verifies the
// non-supported semantics for Retry.
func TestAnaRecv_DeliveryRetry_ReturnsErrNotSupported(t *testing.T) {
	d := NewDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "x"}))
	err := d.Retry(context.Background(), time.Second, errors.New("x"))
	if err == nil || !errors.Is(err, shared.ErrNotSupported) {
		t.Fatalf("Retry → %v, want ErrNotSupported", err)
	}
}

// TestAnaRecv_DeliveryExtend_ReturnsErrNotSupported verifies the
// non-supported semantics for Extend.
func TestAnaRecv_DeliveryExtend_ReturnsErrNotSupported(t *testing.T) {
	d := NewDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "x"}))
	err := d.Extend(context.Background(), time.Now())
	if err == nil || !errors.Is(err, shared.ErrNotSupported) {
		t.Fatalf("Extend → %v, want ErrNotSupported", err)
	}
}

// TestAnaRecv_EmitErrorWithMultipleDeliveries_OnlyFirstErrorReturned
// verifies that when multiple messages are flying and emit errors on
// the first, Run returns that error (not subsequent ones).
func TestAnaRecv_EmitErrorWithMultipleDeliveries_OnlyFirstErrorReturned(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recv-multi-err",
	}, connectivity.SessionEphemeral, nil)

	r := NewReceiver("rx-multi-err", sess)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := errors.New("first")
	var calls atomic.Int32

	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
			n := calls.Add(1)
			if n == 1 {
				return first
			}
			return errors.New("subsequent")
		})
	}()

	select {
	case <-r.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not start")
	}
	for i := 0; i < 5; i++ {
		sess.Router().Route(newTestPacketPublish("test/multi-err", []byte("p")))
	}

	select {
	case err := <-done:
		if !errors.Is(err, first) {
			t.Fatalf("Run returned %v, want %v", err, first)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after emit error")
	}
}

// TestAnaRecv_RouterRoutingWithoutAnyReceiver_BuffersUntilRegistration
// verifies that messages routed before any Receiver is running are NOT
// dropped: they are held in the router's bounded pending buffer and
// flushed — in order — once a matching handler registers. This pins the
// startup-window fix: a persistent session's CONNACK backlog arriving
// before Receiver.Run must survive.
func TestAnaRecv_RouterRoutingWithoutAnyReceiver_BuffersUntilRegistration(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recv-no-handler",
	}, connectivity.SessionEphemeral, nil)

	const n = 5
	for i := 0; i < n; i++ {
		sess.Router().Route(newTestPacketPublish("t/x", []byte{byte(i)}))
	}
	got, dropped := sess.Router().Stats()
	if got != n || dropped != 0 {
		t.Fatalf("Stats received=%d dropped=%d, want %d received and 0 dropped", got, dropped, n)
	}
	if pc := sess.Router().PendingCount(); pc != n {
		t.Fatalf("PendingCount = %d, want %d buffered", pc, n)
	}

	// Registering a matching handler flushes the backlog in order.
	var mu sync.Mutex
	var seen []byte
	done := make(chan struct{})
	sess.Router().RegisterFiltered("late", []string{"t/#"}, func(pub *pahov5.Publish, _ func() error) {
		mu.Lock()
		seen = append(seen, pub.Payload[0])
		if len(seen) == n {
			close(done)
		}
		mu.Unlock()
	})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		mu.Lock()
		got := len(seen)
		mu.Unlock()
		t.Fatalf("only %d/%d buffered messages flushed to the late handler", got, n)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, b := range seen {
		if int(b) != i {
			t.Fatalf("flush out of order: position %d got payload %d", i, b)
		}
	}
	if pc := sess.Router().PendingCount(); pc != 0 {
		t.Fatalf("PendingCount after flush = %d, want 0", pc)
	}
}

// TestAnaRecv_HandlerSeesIndependentEnvelope verifies the EnvelopeFromPublish
// path produces an independent Envelope per delivery: distinct
// publishes get distinct IDs, while a redelivered (byte-identical)
// publish derives the SAME fallback ID so downstream dedup can catch
// broker redeliveries (deterministic topic+payload hash).
func TestAnaRecv_HandlerSeesIndependentEnvelope(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recv-unique-env",
	}, connectivity.SessionEphemeral, nil)

	r := NewReceiver("rx-unique", sess)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var ids []string
	var mu sync.Mutex

	go func() {
		_ = r.Run(ctx, func(_ context.Context, del ports.Delivery) error {
			mu.Lock()
			ids = append(ids, del.Envelope().ID())
			mu.Unlock()
			return nil
		})
	}()
	select {
	case <-r.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not start")
	}

	const n = 10
	for i := 0; i < n; i++ {
		// Distinct payloads → distinct application messages.
		sess.Router().Route(newTestPacketPublish("t/x", []byte{byte(i)}))
	}
	// One byte-identical redelivery of the first publish: must NOT get a
	// fresh ID (QoS 1 redelivery dedup contract).
	sess.Router().Route(newTestPacketPublish("t/x", []byte{0}))

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		l := len(ids)
		mu.Unlock()
		if l >= n+1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only got %d/%d deliveries", l, n+1)
		case <-time.After(20 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	seen := map[string]int{}
	for _, id := range ids {
		seen[id]++
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct envelope IDs for %d distinct messages (+1 redelivery), want %d", len(seen), n, n)
	}
	dups := 0
	for _, c := range seen {
		if c == 2 {
			dups++
		}
	}
	if dups != 1 {
		t.Fatalf("redelivered publish should share its original ID exactly once, got %d duplicated IDs", dups)
	}
}

// staticHandler is a tiny reusable handler for tests that just count.
func staticHandler(c *atomic.Int32) func(*pahov5.Publish) {
	return func(*pahov5.Publish) { c.Add(1) }
}

// TestAnaRecv_HandlerCount_TracksRunLifecycle verifies HandlerCount
// reflects the alive set throughout a Run lifecycle.
func TestAnaRecv_HandlerCount_TracksRunLifecycle(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recv-lifecycle",
	}, connectivity.SessionEphemeral, nil)

	r := NewReceiver("rx-life", sess)

	if sess.Router().HandlerCount() != 0 {
		t.Fatalf("expected 0 handlers initially, got %d", sess.Router().HandlerCount())
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = r.Run(ctx, func(_ context.Context, _ ports.Delivery) error { return nil }) }()

	// Wait for registration.
	deadline := time.After(time.Second)
	for sess.Router().HandlerCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("Run never registered handler")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	for sess.Router().HandlerCount() != 0 {
		select {
		case <-deadline:
			t.Fatal("handler not unregistered after Run returned")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Suppress unused warning of helper.
	var c atomic.Int32
	_ = staticHandler(&c)
}
