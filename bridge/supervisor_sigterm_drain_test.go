package bridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// workCtxReceiver dispatches deliveries on the runtime WORK context it was
// handed in Run, exactly as a production receiver's loop does. Only deliveries
// riding that context can observe the difference between "the process cancelled
// its context" and "the runtime was stopped".
type workCtxReceiver struct {
	ready chan struct{}
	once  sync.Once
	mu    sync.Mutex
	emit  func(context.Context, ports.Delivery) error
	ctx   context.Context //nolint:containedctx // captured for the test's own emit
}

func newWorkCtxReceiver() *workCtxReceiver {
	return &workCtxReceiver{ready: make(chan struct{})}
}

func (r *workCtxReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	r.mu.Lock()
	r.emit = emit
	r.ctx = ctx
	r.mu.Unlock()
	r.once.Do(func() { close(r.ready) })
	<-ctx.Done()
	return ctx.Err()
}

func (r *workCtxReceiver) emitWork(del ports.Delivery) error {
	<-r.ready
	r.mu.Lock()
	emit, ctx := r.emit, r.ctx
	r.mu.Unlock()
	return emit(ctx, del)
}

// liveCtxSender blocks in Send until released, then captures ctx.Err() at that
// exact instant: nil when the delivery drained on a live context,
// context.Canceled when it was killed mid-flight.
type liveCtxSender struct {
	entered chan struct{}
	release chan struct{}
	errAt   chan error
	once    sync.Once
}

func newLiveCtxSender() *liveCtxSender {
	return &liveCtxSender{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		errAt:   make(chan error, 1),
	}
}

func (s *liveCtxSender) Send(ctx context.Context, _ ports.OutboundMessage) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	s.errAt <- ctx.Err()
	return nil
}

type sigtermFactory struct {
	fakeTransportFactory
	recv *workCtxReceiver
	send *liveCtxSender
}

func (f *sigtermFactory) NewReceiver(context.Context, ports.ReceiverSpec, ports.Session) (ports.Receiver, error) {
	return f.recv, nil
}

func (f *sigtermFactory) NewSender(context.Context, ports.SenderSpec, ports.Session) (ports.Sender, error) {
	return f.send, nil
}

// TestSupervisorRun_SigtermKeepsInFlightDeliveriesOnALiveContext is the SIGTERM
// contract for the shipped `gobridge` binary, end to end through the wiring it
// actually uses: main cancels the process context and Supervisor.Run then drains
// the runtime.
//
// While the runtime derived its route contexts from that same process context,
// the cancel reached every in-flight send FIRST, so Runtime.Stop's
// settle-before-cancel phase never ran and every rolling restart aborted work
// mid-flight — duplicates on redelivery, and losses under a drop policy. The
// delivery must instead stay on a live context until the drain releases it.
func TestSupervisorRun_SigtermKeepsInFlightDeliveriesOnALiveContext(t *testing.T) {
	recv := newWorkCtxReceiver()
	send := newLiveCtxSender()
	t.Cleanup(func() {
		select {
		case <-send.release:
		default:
			close(send.release)
		}
	})

	s := newTestSupervisorTransport(&sigtermFactory{recv: recv, send: send})

	cfg := supervisorTestConfig("r1")
	// A drain budget with room for the settle phase this test holds open.
	cfg.Bridge.DrainTimeout = "20s"

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := runSupervisorAsync(ctx, s, cfg, nil)
	waitForRuntime(s, 5*time.Second)

	// One delivery in flight on the runtime work context.
	del := newBlockingDelivery("m1")
	go func() { _ = recv.emitWork(del) }()
	<-send.entered

	// SIGTERM: the process cancels the context it handed to Supervisor.Run.
	cancel()

	// The runtime must still be draining, so releasing the send now must observe
	// a LIVE context.
	close(send.release)
	require.NoError(t, <-send.errAt,
		"SIGTERM must not cancel an in-flight delivery: Run's context cancel has to become a bounded drain, not an abort")

	select {
	case err := <-errCh:
		assert.NoError(t, err, "the drained shutdown must report success")
	case <-time.After(20 * time.Second):
		t.Fatal("Supervisor.Run did not return after the in-flight delivery drained")
	}
}

// newBlockingDelivery builds a minimal delivery for the drain test.
func newBlockingDelivery(id string) ports.Delivery {
	return &sigtermDelivery{env: messaging.MustEnvelope(messaging.EnvelopeInput{ID: id, Subject: "t"})}
}

type sigtermDelivery struct {
	env *messaging.Envelope
}

func (d *sigtermDelivery) Envelope() *messaging.Envelope { return d.env }
func (d *sigtermDelivery) Ack(context.Context) error     { return nil }

func (d *sigtermDelivery) Retry(context.Context, time.Duration, error) error { return nil }
func (d *sigtermDelivery) Extend(context.Context, time.Time) error           { return nil }
