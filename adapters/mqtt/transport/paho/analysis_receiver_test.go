package paho

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain"
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
	}, domain.SessionEphemeral, nil)

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
	}, domain.SessionEphemeral, nil)

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

// TestAnaRecv_HandlerUnregisteredAfterRunReturns verifies the deferred
// Unregister actually removes the handler so it cannot fire after Run
// returns.
func TestAnaRecv_HandlerUnregisteredAfterRunReturns(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recv-unreg",
	}, domain.SessionEphemeral, nil)

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
	}, domain.SessionEphemeral, nil)

	r := NewReceiver("rx-happy", sess)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		got     atomic.Int32
		envSeen *domain.Envelope
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
	if envSeen.Subject != "test/happy" {
		t.Errorf("subject = %q, want test/happy", envSeen.Subject)
	}
	if string(envSeen.Payload) != "payload" {
		t.Errorf("payload = %q, want payload", envSeen.Payload)
	}
}

// TestAnaRecv_DeliveryAck_IsNoop verifies that Delivery.Ack returns nil
// (MQTT acks are handled by paho internally, no caller action needed).
func TestAnaRecv_DeliveryAck_IsNoop(t *testing.T) {
	d := NewDelivery(&domain.Envelope{ID: "x"})
	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("Ack returned %v, want nil", err)
	}
}

// TestAnaRecv_DeliveryRetry_ReturnsErrNotSupported verifies the
// non-supported semantics for Retry.
func TestAnaRecv_DeliveryRetry_ReturnsErrNotSupported(t *testing.T) {
	d := NewDelivery(&domain.Envelope{ID: "x"})
	err := d.Retry(context.Background(), time.Second, errors.New("x"))
	if err == nil || !errors.Is(err, domain.ErrNotSupported) {
		t.Fatalf("Retry → %v, want ErrNotSupported", err)
	}
}

// TestAnaRecv_DeliveryExtend_ReturnsErrNotSupported verifies the
// non-supported semantics for Extend.
func TestAnaRecv_DeliveryExtend_ReturnsErrNotSupported(t *testing.T) {
	d := NewDelivery(&domain.Envelope{ID: "x"})
	err := d.Extend(context.Background(), time.Now())
	if err == nil || !errors.Is(err, domain.ErrNotSupported) {
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
	}, domain.SessionEphemeral, nil)

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

// TestAnaRecv_RouterRoutingWithoutAnyReceiver_DropsAndCounts verifies
// that messages routed before any Receiver is running are dropped (no
// panic, drop counter incremented).
func TestAnaRecv_RouterRoutingWithoutAnyReceiver_DropsAndCounts(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recv-no-handler",
	}, domain.SessionEphemeral, nil)

	const n = 5
	for i := 0; i < n; i++ {
		sess.Router().Route(newTestPacketPublish("t/x", []byte("p")))
	}
	got, dropped := sess.Router().Stats()
	if got != n || dropped != n {
		t.Fatalf("Stats received=%d dropped=%d, want %d/%d", got, dropped, n, n)
	}
}

// TestAnaRecv_HandlerSeesIndependentEnvelope verifies the EnvelopeFromPublish
// path produces a unique Envelope per delivery (different IDs).
func TestAnaRecv_HandlerSeesIndependentEnvelope(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recv-unique-env",
	}, domain.SessionEphemeral, nil)

	r := NewReceiver("rx-unique", sess)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var ids []string
	var mu sync.Mutex

	go func() {
		_ = r.Run(ctx, func(_ context.Context, del ports.Delivery) error {
			mu.Lock()
			ids = append(ids, del.Envelope().ID)
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
		sess.Router().Route(newTestPacketPublish("t/x", []byte("p")))
	}

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		l := len(ids)
		mu.Unlock()
		if l >= n {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only got %d/%d deliveries", l, n)
		case <-time.After(20 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate envelope ID %s", id)
		}
		seen[id] = true
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
	}, domain.SessionEphemeral, nil)

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
