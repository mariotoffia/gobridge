package runtime_test

import (
	"context"
	"errors"
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

// TestInstrumentedDelivery_Extend_Success verifies that on a successful
// Extend the wrapper:
//   - forwards the absolute `until` argument unchanged to the inner Delivery,
//   - emits exactly one MetricVisibilityExtensions counter,
//   - tags the counter with the configured (tagKey, tagValue),
//   - propagates a nil error.
//
// Closes the verification gap noted in CLOCK_FINDINGS.md: the previous
// Extend test only asserted absence of the legacy SQS-specific metric;
// it did not validate the positive emission contract.
func TestInstrumentedDelivery_Extend_Success(t *testing.T) {
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

	until := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	var extendErr error
	_ = wrapped.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		extendErr = del.Extend(ctx, until)
		cancel() // unblock singleDeliveryReceiver.Run; 2s timeout is the safety net
		return nil
	})

	if extendErr != nil {
		t.Fatalf("Extend returned error: %v", extendErr)
	}
	if !inner.Extended {
		t.Fatal("inner Delivery.Extend was not called")
	}
	if !inner.ExtendTo.Equal(until) {
		t.Fatalf("inner.ExtendTo = %s, want %s (until must be forwarded unchanged)", inner.ExtendTo, until)
	}

	counters := rec.FindEntries(shared.MetricVisibilityExtensions)
	if len(counters) != 1 {
		t.Fatalf("expected 1 MetricVisibilityExtensions counter, got %d (%v)",
			len(counters), metricNames(rec.Entries()))
	}
	c := counters[0]
	if c.Kind != "counter" {
		t.Fatalf("entry.Kind = %q, want counter", c.Kind)
	}
	if c.IValue != 1 {
		t.Fatalf("counter value = %d, want 1", c.IValue)
	}
	if len(c.Tags) != 1 || c.Tags[0].Key != "session_id" || c.Tags[0].Value != "mqtt-1" {
		t.Fatalf("counter tags = %+v, want [session_id=mqtt-1]", c.Tags)
	}
}

// TestInstrumentedDelivery_Extend_ErrorSuppressesCounter verifies that
// when the inner Delivery.Extend returns an error the wrapper:
//   - propagates the error unchanged,
//   - does NOT emit a MetricVisibilityExtensions counter.
//
// Counting failed extension attempts would corrupt SLO dashboards;
// the contract is "extensions" = "successful extensions".
func TestInstrumentedDelivery_Extend_ErrorSuppressesCounter(t *testing.T) {
	rec := &ports.RecordingExporter{}
	wantErr := errors.New("broker rejected extend")
	inner := NewFakeDelivery(&messaging.Envelope{ID: "test"})
	inner.ExtendErr = wantErr

	wrapped := goruntime.NewInstrumentedReceiver(
		&singleDeliveryReceiver{del: inner},
		rec,
		"TestReceiveLatency",
		"session_id", "mqtt-1",
		clock.System,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var gotErr error
	_ = wrapped.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		gotErr = del.Extend(ctx, time.Now().Add(time.Minute))
		cancel() // unblock singleDeliveryReceiver.Run; 2s timeout is the safety net
		return nil
	})

	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("Extend error = %v, want %v", gotErr, wantErr)
	}
	if counters := rec.FindEntries(shared.MetricVisibilityExtensions); len(counters) != 0 {
		t.Fatalf("expected 0 MetricVisibilityExtensions counters on error, got %d", len(counters))
	}
}
