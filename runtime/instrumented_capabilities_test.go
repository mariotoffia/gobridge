package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// Coverage for the finding-14 residual (audit D8): the instrumentation
// wrappers for Sender and Receiver must not strip the optional capabilities
// the runtime probes by type assertion — ports.ContextCloser (route-runner
// shutdown), ports.RouteIDSetter (route start) and
// ports.ReceiverStartedSignaler (health/readiness). A bare wrapper silently
// degraded all three: receivers were never closed, adapters never learned
// their route ID, and readiness lost the started signal.

// capabilitySender implements ports.Sender plus the optional capabilities.
type capabilitySender struct {
	routeID  string
	closed   bool
	closeErr error
}

func (s *capabilitySender) Send(context.Context, ports.OutboundMessage) error { return nil }
func (s *capabilitySender) SetRouteID(routeID string)                         { s.routeID = routeID }
func (s *capabilitySender) Close(context.Context) error {
	s.closed = true
	return s.closeErr
}

// bareSender implements ONLY ports.Sender.
type bareSender struct{}

func (bareSender) Send(context.Context, ports.OutboundMessage) error { return nil }

// capabilityReceiver implements ports.Receiver plus all optional capabilities.
type capabilityReceiver struct {
	routeID string
	closed  bool
	started chan struct{}
}

func (r *capabilityReceiver) Run(ctx context.Context, _ func(context.Context, ports.Delivery) error) error {
	<-ctx.Done()
	return ctx.Err()
}
func (r *capabilityReceiver) SetRouteID(routeID string) { r.routeID = routeID }
func (r *capabilityReceiver) Close(context.Context) error {
	r.closed = true
	return nil
}
func (r *capabilityReceiver) Started() <-chan struct{} { return r.started }

// bareReceiver implements ONLY ports.Receiver.
type bareReceiver struct{}

func (bareReceiver) Run(ctx context.Context, _ func(context.Context, ports.Delivery) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestInstrumentedSender_ForwardsOptionalCapabilities(t *testing.T) {
	inner := &capabilitySender{closeErr: errors.New("close failed")}
	s := goruntime.NewInstrumentedSender(inner, &ports.RecordingExporter{}, "m", "k", "v", nil)

	var asSetter ports.RouteIDSetter = s
	asSetter.SetRouteID("route-1")
	if inner.routeID != "route-1" {
		t.Fatalf("SetRouteID not forwarded: inner got %q", inner.routeID)
	}

	var asCloser ports.ContextCloser = s
	if err := asCloser.Close(context.Background()); !errors.Is(err, inner.closeErr) {
		t.Fatalf("Close not forwarded: err %v", err)
	}
	if !inner.closed {
		t.Fatal("Close not forwarded to inner sender")
	}
}

func TestInstrumentedSender_NoOpsWhenInnerLacksCapabilities(t *testing.T) {
	s := goruntime.NewInstrumentedSender(bareSender{}, &ports.RecordingExporter{}, "m", "k", "v", nil)
	s.SetRouteID("route-1") // must not panic
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("no-op Close must return nil, got %v", err)
	}
}

func TestInstrumentedReceiver_ForwardsCloseAndRouteID(t *testing.T) {
	inner := &capabilityReceiver{started: make(chan struct{})}
	r := goruntime.NewInstrumentedReceiver(inner, &ports.RecordingExporter{}, "m", "k", "v", nil)

	// The route runner probes exactly this anonymous interface on shutdown.
	closer, ok := any(r).(interface{ Close(context.Context) error })
	if !ok {
		t.Fatal("wrapper must expose Close(context.Context) error")
	}
	if err := closer.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !inner.closed {
		t.Fatal("D8: Close not forwarded to inner receiver — resources leak on shutdown")
	}

	var asSetter ports.RouteIDSetter = r
	asSetter.SetRouteID("route-2")
	if inner.routeID != "route-2" {
		t.Fatalf("SetRouteID not forwarded: inner got %q", inner.routeID)
	}
}

func TestInstrumentedReceiverCapabilityPreserving_ForwardsStartedSignal(t *testing.T) {
	inner := &capabilityReceiver{started: make(chan struct{})}
	r := goruntime.NewInstrumentedReceiverCapabilityPreserving(inner, &ports.RecordingExporter{}, "m", "k", "v", nil)

	signaler, ok := r.(ports.ReceiverStartedSignaler)
	if !ok {
		t.Fatal("D8: wrapper strips ReceiverStartedSignaler — readiness loses the started signal")
	}

	select {
	case <-signaler.Started():
		t.Fatal("signal must not be closed yet")
	default:
	}
	close(inner.started)
	select {
	case <-signaler.Started():
	default:
		t.Fatal("wrapper Started() is not inner's channel")
	}
}

func TestInstrumentedReceiverCapabilityPreserving_DoesNotFakeStartedSignal(t *testing.T) {
	r := goruntime.NewInstrumentedReceiverCapabilityPreserving(bareReceiver{}, &ports.RecordingExporter{}, "m", "k", "v", nil)
	if _, ok := r.(ports.ReceiverStartedSignaler); ok {
		t.Fatal("wrapper must NOT advertise ReceiverStartedSignaler when inner lacks it: " +
			"health would wait forever on a signal that never comes")
	}
}
