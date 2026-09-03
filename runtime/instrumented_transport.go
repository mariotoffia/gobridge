package runtime

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// InstrumentedSender wraps a Sender and records send latency via
// MetricsExporter. The metric name and dimension tag are configurable
// so the same wrapper serves both MQTT and SQS senders.
type InstrumentedSender struct {
	inner      ports.Sender
	metrics    ports.MetricsExporter
	metricName string
	tagKey     string
	tagValue   string
	clk        clock.Clock
}

var _ ports.Sender = (*InstrumentedSender)(nil)

// NewInstrumentedSender decorates inner with send-latency metrics.
// metricName is the timer metric emitted on each Send (e.g. the transport
// adapter's own "MQTTPublishLatency" or "SQSDeleteLatency" constant).
// tagKey and tagValue are the dimension added to each emission
// (e.g. "session_id"/"my-session" or "queue_url"/"...").
func NewInstrumentedSender(
	inner ports.Sender,
	metrics ports.MetricsExporter,
	metricName, tagKey, tagValue string,
	clk clock.Clock,
) *InstrumentedSender {
	return &InstrumentedSender{
		inner:      inner,
		metrics:    metrics,
		metricName: metricName,
		tagKey:     tagKey,
		tagValue:   tagValue,
		clk:        instrumentedClock(clk),
	}
}

func (s *InstrumentedSender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	start := s.clk.Now()
	err := s.inner.Send(ctx, msg)
	s.metrics.Timer(s.metricName, s.clk.Since(start),
		shared.Tag{Key: s.tagKey, Value: s.tagValue})
	return err
}

// SetRouteID forwards the optional ports.RouteIDSetter capability — an
// instrumentation wrapper must not strip an optional interface. When
// inner does not implement it, the call is a no-op — behaviourally identical
// to the runtime's capability probe failing on the unwrapped sender.
func (s *InstrumentedSender) SetRouteID(routeID string) {
	if setter, ok := s.inner.(ports.RouteIDSetter); ok {
		setter.SetRouteID(routeID)
	}
}

// Close forwards the optional ports.ContextCloser capability. A no-op when
// inner does not implement it (closing nothing is indistinguishable from not
// being closable).
func (s *InstrumentedSender) Close(ctx context.Context) error {
	if closer, ok := s.inner.(ports.ContextCloser); ok {
		return closer.Close(ctx)
	}
	return nil
}

// InstrumentedReceiver wraps a Receiver and records per-delivery
// receive latency (the time between delivery emission and the emit
// callback returning).
type InstrumentedReceiver struct {
	inner      ports.Receiver
	metrics    ports.MetricsExporter
	metricName string
	tagKey     string
	tagValue   string
	clk        clock.Clock
}

var _ ports.Receiver = (*InstrumentedReceiver)(nil)

// NewInstrumentedReceiver decorates inner with receive-latency metrics.
// Library consumers wiring their own composition root should prefer
// NewInstrumentedReceiverCapabilityPreserving: this constructor's concrete
// return type never satisfies ports.ReceiverStartedSignaler, so a wrapped
// receiver's started signal is invisible to readiness probing.
func NewInstrumentedReceiver(
	inner ports.Receiver,
	metrics ports.MetricsExporter,
	metricName, tagKey, tagValue string,
	clk clock.Clock,
) *InstrumentedReceiver {
	return &InstrumentedReceiver{
		inner:      inner,
		metrics:    metrics,
		metricName: metricName,
		tagKey:     tagKey,
		tagValue:   tagValue,
		clk:        instrumentedClock(clk),
	}
}

func (r *InstrumentedReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	return r.inner.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		start := r.clk.Now()
		err := emit(ctx, &instrumentedDelivery{
			Delivery: del,
			metrics:  r.metrics,
			tagKey:   r.tagKey,
			tagValue: r.tagValue,
			clk:      r.clk,
		})
		r.metrics.Timer(r.metricName, r.clk.Since(start),
			shared.Tag{Key: r.tagKey, Value: r.tagValue})
		return err
	})
}

// Close forwards the optional ports.ContextCloser capability: the route
// runner probes `Close(context.Context) error` on shutdown,
// and a bare wrapper would strip it, leaking the inner receiver's resources.
// A no-op when inner does not implement it.
func (r *InstrumentedReceiver) Close(ctx context.Context) error {
	if closer, ok := r.inner.(ports.ContextCloser); ok {
		return closer.Close(ctx)
	}
	return nil
}

// SetRouteID forwards the optional ports.RouteIDSetter capability so the inner
// receiver still learns its route ID for metric/log labelling when wrapped.
// A no-op when inner does not implement it.
func (r *InstrumentedReceiver) SetRouteID(routeID string) {
	if setter, ok := r.inner.(ports.RouteIDSetter); ok {
		setter.SetRouteID(routeID)
	}
}

// NewInstrumentedReceiverCapabilityPreserving decorates inner with
// receive-latency metrics while preserving its optional
// ports.ReceiverStartedSignaler capability. This is the
// constructor library consumers should prefer; the bridge's own composition
// root never wraps receivers (adapters self-instrument), so nothing in-repo
// exercises either constructor in production.
//
// Started cannot be forwarded unconditionally on InstrumentedReceiver: the
// health prober treats a successful `receiver.(ports.ReceiverStartedSignaler)`
// assertion as "a start signal WILL arrive" and waits on the channel, so a
// wrapper faking the capability over an inner receiver that never closes any
// channel would wedge readiness. The capability must therefore be re-exported
// only when inner actually has it — the same construction-time variant
// selection as NewInstrumentedOutboxStoreCapabilityPreserving. Close and
// SetRouteID forwarding are safe unconditionally (no-op when absent) and live
// on the base wrapper.
func NewInstrumentedReceiverCapabilityPreserving(
	inner ports.Receiver,
	metrics ports.MetricsExporter,
	metricName, tagKey, tagValue string,
	clk clock.Clock,
) ports.Receiver {
	base := NewInstrumentedReceiver(inner, metrics, metricName, tagKey, tagValue, clk)
	if signaler, ok := inner.(ports.ReceiverStartedSignaler); ok {
		return &instrumentedReceiverStartedSignaler{InstrumentedReceiver: base, signaler: signaler}
	}
	return base
}

// instrumentedReceiverStartedSignaler re-exports the optional
// ports.ReceiverStartedSignaler capability of a wrapped receiver that has it.
type instrumentedReceiverStartedSignaler struct {
	*InstrumentedReceiver
	signaler ports.ReceiverStartedSignaler
}

var (
	_ ports.Receiver                = (*instrumentedReceiverStartedSignaler)(nil)
	_ ports.ReceiverStartedSignaler = (*instrumentedReceiverStartedSignaler)(nil)
)

func (r *instrumentedReceiverStartedSignaler) Started() <-chan struct{} {
	return r.signaler.Started()
}

// instrumentedDelivery wraps a Delivery to count visibility extensions
// and record ack/delete latency.
//
// HAZARD: embedding ports.Delivery promotes every current and FUTURE
// method of the wrapped Delivery, but a wrapper is opaque to interface probing —
// a caller doing `d.(SomeOptionalCap)` sees THIS concrete type, not the inner
// delivery, so any optional capability the underlying Delivery grows (e.g. a
// future DeadLetter() or Lease() sub-interface) is silently masked. If such an
// optional Delivery capability is ever added, it MUST be explicitly forwarded
// here (add the method and delegate to d.Delivery, or expose the inner value via
// an Unwrap) so this wrapper does not hide it.
type instrumentedDelivery struct {
	ports.Delivery
	metrics  ports.MetricsExporter
	tagKey   string
	tagValue string
	clk      clock.Clock
}

func (d *instrumentedDelivery) Ack(ctx context.Context) error {
	start := d.clk.Now()
	err := d.Delivery.Ack(ctx)
	d.metrics.Timer(shared.MetricAckLatency, d.clk.Since(start),
		shared.Tag{Key: d.tagKey, Value: d.tagValue})
	return err
}

func (d *instrumentedDelivery) Extend(ctx context.Context, until time.Time) error {
	err := d.Delivery.Extend(ctx, until)
	if err == nil {
		d.metrics.Counter(shared.MetricVisibilityExtensions, 1,
			shared.Tag{Key: d.tagKey, Value: d.tagValue})
	}
	return err
}
