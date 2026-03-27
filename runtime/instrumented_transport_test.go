package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

func TestInstrumentedDelivery_Ack_EmitsGenericMetric(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeDelivery(&domain.Envelope{ID: "test"})

	wrapped := goruntime.NewInstrumentedReceiver(
		&singleDeliveryReceiver{del: inner},
		rec,
		"TestReceiveLatency",
		"session_id", "mqtt-1",
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = wrapped.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		return del.Ack(ctx)
	})

	entries := rec.Entries()

	for _, e := range entries {
		if e.Name == domain.MetricSQSDeleteLatency {
			t.Fatalf("instrumented delivery emitted SQS-specific metric %q for non-SQS transport; "+
				"expected a generic metric name like MetricAckLatency", domain.MetricSQSDeleteLatency)
		}
	}

	found := false
	for _, e := range entries {
		if e.Name == domain.MetricAckLatency {
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
	inner := NewFakeDelivery(&domain.Envelope{ID: "test"})

	wrapped := goruntime.NewInstrumentedReceiver(
		&singleDeliveryReceiver{del: inner},
		rec,
		"TestReceiveLatency",
		"session_id", "mqtt-1",
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = wrapped.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		_ = del.Extend(ctx, time.Now().Add(time.Minute))
		return del.Ack(ctx)
	})

	for _, e := range rec.Entries() {
		if e.Name == domain.MetricSQSVisibilityExtensions {
			t.Fatalf("instrumented delivery emitted SQS-specific metric %q for non-SQS transport; "+
				"expected a generic metric name like MetricVisibilityExtensions",
				domain.MetricSQSVisibilityExtensions)
		}
	}
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
