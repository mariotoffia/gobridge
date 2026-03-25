package runtime

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain"
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
}

var _ ports.Sender = (*InstrumentedSender)(nil)

// NewInstrumentedSender decorates inner with send-latency metrics.
// metricName is the timer metric emitted on each Send (e.g.
// domain.MetricMQTTPublishLatency or domain.MetricSQSDeleteLatency).
// tagKey and tagValue are the dimension added to each emission
// (e.g. "session_id"/"my-session" or "queue_url"/"...").
func NewInstrumentedSender(
	inner ports.Sender,
	metrics ports.MetricsExporter,
	metricName, tagKey, tagValue string,
) *InstrumentedSender {
	return &InstrumentedSender{
		inner:      inner,
		metrics:    metrics,
		metricName: metricName,
		tagKey:     tagKey,
		tagValue:   tagValue,
	}
}

func (s *InstrumentedSender) Send(ctx context.Context, env *domain.Envelope) error {
	start := time.Now()
	err := s.inner.Send(ctx, env)
	s.metrics.Timer(s.metricName, time.Since(start),
		domain.Tag{Key: s.tagKey, Value: s.tagValue})
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
}

var _ ports.Receiver = (*InstrumentedReceiver)(nil)

// NewInstrumentedReceiver decorates inner with receive-latency metrics.
func NewInstrumentedReceiver(
	inner ports.Receiver,
	metrics ports.MetricsExporter,
	metricName, tagKey, tagValue string,
) *InstrumentedReceiver {
	return &InstrumentedReceiver{
		inner:      inner,
		metrics:    metrics,
		metricName: metricName,
		tagKey:     tagKey,
		tagValue:   tagValue,
	}
}

func (r *InstrumentedReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	return r.inner.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		start := time.Now()
		err := emit(ctx, &instrumentedDelivery{
			Delivery: del,
			metrics:  r.metrics,
			tagKey:   r.tagKey,
			tagValue: r.tagValue,
		})
		r.metrics.Timer(r.metricName, time.Since(start),
			domain.Tag{Key: r.tagKey, Value: r.tagValue})
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
}

func (d *instrumentedDelivery) Ack(ctx context.Context) error {
	start := time.Now()
	err := d.Delivery.Ack(ctx)
	d.metrics.Timer(domain.MetricSQSDeleteLatency, time.Since(start),
		domain.Tag{Key: d.tagKey, Value: d.tagValue})
	return err
}

func (d *instrumentedDelivery) Extend(ctx context.Context, until time.Time) error {
	err := d.Delivery.Extend(ctx, until)
	if err == nil {
		d.metrics.Counter(domain.MetricSQSVisibilityExtensions, 1,
			domain.Tag{Key: d.tagKey, Value: d.tagValue})
	}
	return err
}
