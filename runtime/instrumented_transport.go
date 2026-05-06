package runtime

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain"
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
// metricName is the timer metric emitted on each Send (e.g.
// shared.MetricMQTTPublishLatency or shared.MetricSQSDeleteLatency).
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

func (s *InstrumentedSender) Send(ctx context.Context, env *domain.Envelope) error {
	start := s.clk.Now()
	err := s.inner.Send(ctx, env)
	s.metrics.Timer(s.metricName, s.clk.Since(start),
		shared.Tag{Key: s.tagKey, Value: s.tagValue})
	return err
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

// instrumentedDelivery wraps a Delivery to count visibility extensions
// and record ack/delete latency.
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
