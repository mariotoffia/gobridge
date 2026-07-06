// ═══════════════════════════════════════════════
// Production-readiness remediation tests: consumer channel ownership.
//
// Covers the HIGH finding — the consume loop used to `defer ch.Close()`,
// tearing the channel down the moment Run returned on ctx cancel. In-flight
// deliveries, settled by the route runner in detached goroutines AFTER Run
// returns, then failed their Ack on the closed channel and the broker
// requeued settled work as duplicates on every shutdown/rollout.
//
// The fix hands channel ownership to the Receiver: on a graceful stop the
// consume loop leaves the channel open and Receiver.Close (invoked by the
// runner only after draining) tears it down. On an error/reconnect return
// the loop still closes the channel so the next attempt opens a fresh one.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// fakeReceiverChannel is a test double for receiverChannel that records
// Close and lets the test drive the delivery and channel-close streams.
type fakeReceiverChannel struct {
	mu          sync.Mutex
	closed      bool
	closeCalls  int
	deliveries  chan *Delivery
	notifyClose chan error
	// closeGate, when non-nil, blocks Close until the test releases it,
	// modelling amqp091 Channel.Close blocking on close-ok over a
	// half-dead connection.
	closeGate chan struct{}
}

func newFakeReceiverChannel() *fakeReceiverChannel {
	return &fakeReceiverChannel{
		deliveries:  make(chan *Delivery, 1),
		notifyClose: make(chan error, 1),
	}
}

func (f *fakeReceiverChannel) Qos(int, int) error { return nil }

func (f *fakeReceiverChannel) Consume(
	_ context.Context,
	_, _ string,
	_, _ bool,
	_ *slog.Logger,
	_ ports.MetricsExporter,
	_ clock.Clock,
) (<-chan *Delivery, error) {
	return f.deliveries, nil
}

func (f *fakeReceiverChannel) NotifyClose() <-chan error { return f.notifyClose }

func (f *fakeReceiverChannel) Close() error {
	f.mu.Lock()
	gate := f.closeGate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	f.closed = true
	f.closeCalls++
	f.mu.Unlock()
	return nil
}

func (f *fakeReceiverChannel) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeReceiverChannel) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCalls
}

// TestReceiver_GracefulStop_HandsChannelToClose is the core drain-then-close
// proof: after runChannel returns on ctx cancel the channel is still open, so
// an in-flight delivery accepted before the stop can be Acked; Close then
// tears the channel down. With the old `defer ch.Close()` the Ack would fail
// on a closed channel and the message would be redelivered as a duplicate.
func TestReceiver_GracefulStop_HandsChannelToClose(t *testing.T) {
	fc := newFakeReceiverChannel()

	ack := newMockAcknowledger()
	ack.AckFn = func(uint64, bool) error {
		// Model the SDK: Acking on a closed channel fails.
		if fc.isClosed() {
			return errors.New("amqp091: channel/connection is not open")
		}
		return nil
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Payload: []byte("x")})
	fc.deliveries <- NewDelivery(
		env,
		amqp.Delivery{Acknowledger: ack, DeliveryTag: 1, RoutingKey: "rk"},
		slog.Default(), &ports.NoopExporter{}, nil,
	)

	r := &Receiver{
		cfg:     ReceiverConfig{QueueName: "q"},
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
		started: make(chan struct{}),
	}

	got := make(chan ports.Delivery, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- r.runChannel(ctx, fc, func(_ context.Context, d ports.Delivery) error {
			// Simulate the runner accepting the delivery into a detached
			// settlement goroutine WITHOUT settling it inline.
			got <- d
			return nil
		})
	}()

	inflight := wait.RequireReceive(t, got, 2*time.Second)

	// Graceful stop.
	cancel()
	if err := wait.RequireReceive(t, done, 2*time.Second); err != nil {
		t.Fatalf("runChannel returned %v, want nil on graceful stop", err)
	}

	// The channel must NOT have been closed by the consume loop.
	if fc.isClosed() {
		t.Fatal("graceful stop closed the channel; in-flight Acks fail and duplicate on next start")
	}
	// The drained delivery settles successfully on the still-open channel.
	if err := inflight.Ack(context.Background()); err != nil {
		t.Fatalf("in-flight Ack after Run-return failed: %v", err)
	}

	// Close, as the runner does post-drain, tears the channel down exactly once.
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !fc.isClosed() {
		t.Fatal("Close did not close the handed-off channel")
	}
	if n := fc.closeCount(); n != 1 {
		t.Fatalf("channel Close called %d times, want exactly 1", n)
	}
}

// TestReceiver_ErrorReturn_ClosesChannel proves the non-graceful path still
// closes the channel so the next reconnect attempt opens a fresh one (no
// channel leak across a broker-initiated close). Here the broker closes the
// channel (NotifyClose emits) while ctx stays live.
func TestReceiver_ErrorReturn_ClosesChannel(t *testing.T) {
	fc := newFakeReceiverChannel()

	r := &Receiver{
		cfg:     ReceiverConfig{QueueName: "q"},
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
		started: make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- r.runChannel(ctx, fc, func(context.Context, ports.Delivery) error { return nil })
	}()

	// Broker closes the channel.
	fc.notifyClose <- &amqp.Error{Code: 320, Reason: "CONNECTION_FORCED"}

	err := wait.RequireReceive(t, done, 2*time.Second)
	if err == nil {
		t.Fatal("runChannel should return a transport error on broker channel close")
	}
	if !fc.isClosed() {
		t.Fatal("error return must close the channel so the next attempt opens fresh")
	}
	// After an error return there is nothing to hand off; Close is a no-op.
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close after error return: %v", err)
	}
	if n := fc.closeCount(); n != 1 {
		t.Fatalf("channel Close called %d times, want exactly 1 (error path only)", n)
	}
}

// TestReceiver_Close_NoChannel_NoOp proves Close is safe when no consume
// attempt handed off a channel (e.g. the receiver never started).
func TestReceiver_Close_NoChannel_NoOp(t *testing.T) {
	r := &Receiver{started: make(chan struct{})}
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close with no active channel = %v, want nil", err)
	}
}

// TestReceiver_Close_HonoursContext covers SHOULD-FIX 1: Close must honour
// ctx (ports.ContextCloser). amqp091 Channel.Close blocks on close-ok, which
// on a half-dead connection resolves only via missed-heartbeat detection
// (~2× heartbeat). Close detaches the channel teardown and races it against
// ctx.Done, so an expired ctx returns promptly instead of wedging the route
// shutdown; the detached goroutine still completes the channel close.
func TestReceiver_Close_HonoursContext(t *testing.T) {
	fc := newFakeReceiverChannel()
	fc.closeGate = make(chan struct{}) // Close blocks until released
	r := &Receiver{started: make(chan struct{})}
	r.setActiveChannel(fc)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // ctx already expired: Close must not wait on the blocked channel

	if err := r.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close(cancelled ctx) = %v, want context.Canceled", err)
	}

	// The detached goroutine still completes the channel close once unblocked.
	close(fc.closeGate)
	wait.Until(t, 2*time.Second, "detached channel close completes", func() bool {
		return fc.isClosed()
	})
}

// TestReceiver_Close_ReturnsNilWhenCloseCompletesFirst proves the happy path:
// when the channel closes before ctx expires, Close returns the close result
// (nil here), not a spurious ctx error.
func TestReceiver_Close_ReturnsNilWhenCloseCompletesFirst(t *testing.T) {
	fc := newFakeReceiverChannel() // no gate: Close returns immediately
	r := &Receiver{started: make(chan struct{})}
	r.setActiveChannel(fc)

	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	if !fc.isClosed() {
		t.Fatal("Close did not close the channel")
	}
}

// TestReceiver_Run_SessionClosedWhileActive_ReturnsError covers the
// MINOR/SUSPECT finding: when the session is closed while the route ctx is
// still live, waitForReconnect returns false (its event stream is closed).
// Run previously returned ctx.Err() == nil there — a silent clean stop that
// left the route dead while the runtime believed it healthy. Run must now
// return a non-nil transient error so the stop is attributable.
func TestReceiver_Run_SessionClosedWhileActive_ReturnsError(t *testing.T) {
	sess := newResilienceSession(nil)
	// The embedder closes the session out from under a live route.
	_ = sess.Close(context.Background())

	r := &Receiver{
		cfg:     ReceiverConfig{QueueName: "q"},
		session: sess,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
		started: make(chan struct{}),
	}

	// ctx stays LIVE for the whole test — the point is that Run must NOT
	// rely on ctx cancellation to report the stop.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, func(context.Context, ports.Delivery) error { return nil }) }()

	err := wait.RequireReceive(t, done, 2*time.Second)
	if err == nil {
		t.Fatal("Run must return a non-nil error when the session closes under a live route")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) || be.Code != shared.ErrCodeConnectionLost {
		t.Fatalf("Run returned %v, want ErrConnectionLost", err)
	}
}
