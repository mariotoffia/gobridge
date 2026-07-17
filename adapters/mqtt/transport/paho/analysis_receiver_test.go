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
// exercise behaviour without needing autopaho. They therefore exercise
// the LEGACY router seam (Session.Router().Route), which dispatches with
// a nil ack callback — so Delivery.Ack is a no-op in these tests ONLY.
// Production Paho enables manual acknowledgement and the Receiver wires
// WithAckFunc, so a production Delivery.Ack invokes the Paho ack (see
// delivery.go and acl_session.go). Nothing here is a production
// settlement-conformance statement.
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

// TestAnaRecv_EmitError_LegacyRouteSeamAckNoop drives the LEGACY router seam
// (Session.Router().Route), NOT the production Paho ingress path. The seam
// dispatches with a nil ack callback, so the Delivery it hands to emit has no
// wired acknowledgement and Delivery.Ack is a no-op HERE ONLY. Production Paho
// enables manual acknowledgement (acl_session.go) and the Receiver wires
// WithAckFunc, so a production Delivery.Ack invokes the Paho ack — this test is
// not a production settlement-conformance statement. What it verifies is the
// ports.Receiver emit-error contract on the seam: the emit error is propagated
// out of Run (cancelling the run), and an un-wired Retry leaves the delivery
// unsettled so a fallback Ack can still win.
func TestAnaRecv_EmitError_LegacyRouteSeamAckNoop(t *testing.T) {
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

	// This legacy seam wired no ack/retry callback, so Retry is unsupported and
	// must leave the delivery unsettled so a fallback Ack can still win. (In
	// production the Receiver wires both callbacks; Ephemeral/QoS 0 Retry is
	// unsupported there because a reconnect cannot redeliver, not because the
	// callback is absent.)
	if err := del.Retry(context.Background(), time.Second, emitErr); !errors.Is(err, shared.ErrNotSupported) {
		t.Fatalf("legacy-seam Delivery.Retry must return ErrNotSupported, got %v", err)
	}
	// No ack callback is wired on the seam, so Ack is a no-op returning nil here.
	// Production Paho wires WithAckFunc and Delivery.Ack invokes the Paho manual ack.
	if err := del.Ack(context.Background()); err != nil {
		t.Fatalf("legacy-seam Delivery.Ack (no wired callback) must return nil, got %v", err)
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

// TestAnaRecv_DeliveryAck_NoCallbackIsNoop verifies that a Delivery constructed
// WITHOUT an ack callback (the legacy Route path / QoS 0 / tests) acks as a
// no-op returning nil. This is NOT the production contract: production Paho
// enables manual acknowledgement and the Receiver wires WithAckFunc, so a
// production Delivery.Ack invokes the Paho ack. See delivery.go.
func TestAnaRecv_DeliveryAck_NoCallbackIsNoop(t *testing.T) {
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

// TestAnaRecv_HandlerSeesIndependentEnvelope verifies every no-ID publish
// receives an independent fallback identity, including byte-identical broker
// redelivery that MQTT cannot prove is the same application event.
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
	// A byte-identical no-ID redelivery receives a fresh ID. MQTT packet IDs
	// are reusable and cannot safely prove application identity.
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
	if len(seen) != n+1 {
		t.Fatalf("got %d distinct envelope IDs for %d no-ID publishes, want %d", len(seen), n+1, n+1)
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

// TestAnaRecv_FanoutIdentity_StablePerPublish verifies the router stamps one
// fallback identity before fan-out, so every receiver conversion of one
// publish observes the same Envelope ID while the next equal-valued publish
// receives a new ID.
func TestAnaRecv_FanoutIdentity_StablePerPublish(t *testing.T) {
	r := newRouter(nil, nil)
	var (
		mu  sync.Mutex
		ids = map[string][]string{"a": nil, "b": nil}
	)
	for _, name := range []string{"a", "b"} {
		name := name
		r.RegisterEnvelope(name, nil, nil, func(env *messaging.Envelope, _ func() error) {
			mu.Lock()
			ids[name] = append(ids[name], env.ID())
			mu.Unlock()
		})
	}

	for range 2 {
		r.Route(newTestPacketPublish("identity/fanout", []byte("same")))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(ids["a"]) != 2 || len(ids["b"]) != 2 {
		t.Fatalf("fan-out counts = a:%d b:%d, want 2 each", len(ids["a"]), len(ids["b"]))
	}
	if ids["a"][0] != ids["b"][0] || ids["a"][1] != ids["b"][1] {
		t.Fatalf("one publish must have one fan-out identity: a=%v b=%v", ids["a"], ids["b"])
	}
	if ids["a"][0] == ids["a"][1] {
		t.Fatalf("separate equal-valued publishes must differ: %v", ids["a"])
	}
}

// TestAnaRecv_IngressIdentity_DoesNotMutateBrokerPublish verifies identity is
// stamped on the router-owned clone, not on the Paho callback object that may
// be observed by later SDK callbacks.
func TestAnaRecv_IngressIdentity_DoesNotMutateBrokerPublish(t *testing.T) {
	r := newRouter(nil, nil)
	var id string
	r.RegisterEnvelope("identity-owner", nil, nil, func(env *messaging.Envelope, _ func() error) {
		id = env.ID()
	})
	original := &pahov5.Publish{Topic: "identity/ownership", QoS: 1, Payload: []byte("same")}

	handled, err := r.onPublishReceived(pahov5.PublishReceived{Packet: original})
	if err != nil || !handled {
		t.Fatalf("onPublishReceived = handled:%v err:%v", handled, err)
	}
	if id == "" {
		t.Fatal("handler did not receive a generated identity")
	}
	if original.Properties != nil {
		t.Fatalf("broker-owned publish was mutated: %+v", original.Properties)
	}
}
