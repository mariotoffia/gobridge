package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

func TestInstrumentedDelivery_Ack_EmitsGenericMetric(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeDelivery(&messaging.Envelope{ID: "test"})

	wrapped := goruntime.NewInstrumentedReceiver(
		&singleDeliveryReceiver{del: inner},
		rec,
		"TestReceiveLatency",
		"session_id", "mqtt-1",
		clock.System,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = wrapped.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		return del.Ack(ctx)
	})

	entries := rec.Entries()

	for _, e := range entries {
		if e.Name == shared.MetricSQSDeleteLatency {
			t.Fatalf("instrumented delivery emitted SQS-specific metric %q for non-SQS transport; "+
				"expected a generic metric name like MetricAckLatency", shared.MetricSQSDeleteLatency)
		}
	}

	found := false
	for _, e := range entries {
		if e.Name == shared.MetricAckLatency {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected MetricAckLatency emission; got entries: %v", metricNames(entries))
	}
}

func TestInstrumentedDelivery_Extend_EmitsGenericMetric(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeDelivery(&messaging.Envelope{ID: "test"})

	wrapped := goruntime.NewInstrumentedReceiver(
		&singleDeliveryReceiver{del: inner},
		rec,
		"TestReceiveLatency",
		"session_id", "mqtt-1",
		clock.System,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = wrapped.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		_ = del.Extend(ctx, time.Now().Add(time.Minute))
		return del.Ack(ctx)
	})

	for _, e := range rec.Entries() {
		if e.Name == shared.MetricSQSVisibilityExtensions {
			t.Fatalf("instrumented delivery emitted SQS-specific metric %q for non-SQS transport; "+
				"expected a generic metric name like MetricVisibilityExtensions",
				shared.MetricSQSVisibilityExtensions)
		}
	}
}

func TestInstrumentedDelivery_AckLatencyUsesInjectedClock(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(100, 0))
	rec := &ports.RecordingExporter{}
	inner := &advancingDelivery{Delivery: NewFakeDelivery(&messaging.Envelope{ID: "test"}), clk: clk, advance: 40 * time.Millisecond}

	wrapped := goruntime.NewInstrumentedReceiver(
		&singleDeliveryReceiver{del: inner},
		rec,
		"TestReceiveLatency",
		"session_id", "mqtt-1",
		clk,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = wrapped.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		return del.Ack(ctx)
	})

	timers := rec.FindEntries(shared.MetricAckLatency)
	if len(timers) != 1 {
		t.Fatalf("expected 1 ack timer, got %d", len(timers))
	}
	if timers[0].Duration != 40*time.Millisecond {
		t.Fatalf("duration = %s, want 40ms", timers[0].Duration)
	}
}

type advancingDelivery struct {
	ports.Delivery
	clk     *clocktest.Fake
	advance time.Duration
}

func (d *advancingDelivery) Ack(ctx context.Context) error {
	d.clk.Advance(d.advance)
	return d.Delivery.Ack(ctx)
}

type singleDeliveryReceiver struct {
	del ports.Delivery
}

func (r *singleDeliveryReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	_ = emit(ctx, r.del)
	<-ctx.Done()
	return ctx.Err()
}

func metricNames(entries []ports.MetricEntry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	return names
}
