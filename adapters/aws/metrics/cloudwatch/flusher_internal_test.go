package cloudwatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/smithy-go"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
)

// waitTimeout bounds event waits in this file. It is a deadline on a
// channel receive (not a sleep); tests pass as soon as the event fires.
const waitTimeout = 5 * time.Second

// MF-1 regression: reaching the flush-trigger threshold must wake the
// single flusher goroutine — no per-emission goroutine spawning, no
// blocking of the emitting caller. The mock signals when the flush
// lands, so the test synchronises on the event itself.
func TestExporter_BufferFullTriggersBackgroundFlush(t *testing.T) {
	flushed := make(chan int, 10)
	mock := &mockCloudWatch{
		PutMetricDataFn: func(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
			flushed <- len(params.MetricData)
			return &cloudwatch.PutMetricDataOutput{}, nil
		},
	}
	fake := clocktest.NewAt(time.Unix(1700000000, 0))
	ctx := context.Background()

	e, err := New(ctx, "Test/NS",
		WithClient(mock),
		WithClock(fake),
		WithBufferSize(3),
		WithLogger(nil),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close(ctx) }()

	// Three distinct gauge samples reach the threshold; the third
	// emission signals the flusher.
	e.Gauge("g1", 1)
	e.Gauge("g2", 2)
	e.Gauge("g3", 3)

	select {
	case n := <-flushed:
		if n != 3 {
			t.Errorf("flushed %d datums, want 3", n)
		}
	case <-time.After(waitTimeout):
		t.Fatal("background flusher did not flush after threshold trigger")
	}
}

// MF-1: after a retryable flush failure, buffer-full triggers are
// suppressed until the backoff window has elapsed on the injected
// clock — a stalled endpoint must not be hammered by threshold
// triggers. The governor is exercised synchronously (it is owned by
// the single flusher goroutine in production), so the test is fully
// deterministic.
func TestFlushGovernor_SuppressesTriggersDuringBackoff(t *testing.T) {
	fake := clocktest.NewAt(time.Unix(1700000000, 0))
	gov := newFlushGovernor(fake, time.Minute)

	if !gov.allowTrigger() {
		t.Fatal("fresh governor must allow triggers")
	}

	// Retryable failure arms the 1s base backoff.
	if !gov.observe(errors.New("connection reset")) {
		t.Fatal("non-APIError must be classified retryable")
	}
	if gov.allowTrigger() {
		t.Error("trigger inside backoff window must be suppressed")
	}

	// Half a window: still suppressed.
	fake.Advance(500 * time.Millisecond)
	if gov.allowTrigger() {
		t.Error("trigger halfway through backoff must be suppressed")
	}

	// Past the window: allowed again.
	fake.Advance(600 * time.Millisecond)
	if !gov.allowTrigger() {
		t.Error("trigger after backoff elapsed must be allowed")
	}

	// Success resets suppression and backoff.
	gov.observe(nil)
	if !gov.allowTrigger() {
		t.Error("success must lift suppression")
	}
	if gov.backoff != flushRetryBaseBackoff {
		t.Errorf("success must reset backoff, got %v", gov.backoff)
	}
}

// MF-1: consecutive retryable failures double the backoff up to the
// FlushInterval cap; permanent failures neither suppress nor reset
// (the batcher already dropped the offending batch, MF-3).
func TestFlushGovernor_BackoffDoublesAndPermanentSkips(t *testing.T) {
	fake := clocktest.NewAt(time.Unix(1700000000, 0))
	gov := newFlushGovernor(fake, 3*time.Second)

	retryable := errors.New("connection reset")
	gov.observe(retryable)
	if gov.backoff != 2*time.Second {
		t.Errorf("backoff after 1st failure = %v, want 2s", gov.backoff)
	}
	gov.observe(retryable)
	if gov.backoff != 3*time.Second {
		t.Errorf("backoff must cap at FlushInterval, got %v", gov.backoff)
	}

	// Permanent error: batch already dropped — no suppression.
	gov2 := newFlushGovernor(fake, time.Minute)
	if gov2.observe(&smithyErr{code: "InvalidParameterValue", fault: smithy.FaultClient}) {
		t.Fatal("client-fault APIError must be classified permanent")
	}
	if !gov2.allowTrigger() {
		t.Error("permanent failure must not suppress triggers")
	}
}

// MF-1: the periodic ticker flushes on the injected clock cadence.
func TestExporter_PeriodicFlushOnTicker(t *testing.T) {
	flushed := make(chan struct{}, 10)
	mock := &mockCloudWatch{
		PutMetricDataFn: func(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
			flushed <- struct{}{}
			return &cloudwatch.PutMetricDataOutput{}, nil
		},
	}
	fake := clocktest.NewAt(time.Unix(1700000000, 0))
	ctx := context.Background()

	e, err := New(ctx, "Test/NS",
		WithClient(mock),
		WithClock(fake),
		WithFlushInterval(time.Minute),
		WithLogger(nil),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close(ctx) }()

	e.Counter("c", 1)

	// Wait for the flusher goroutine to have registered its ticker on
	// the fake clock before advancing (clocktest exposes TickerCount).
	waitForTicker(t, fake)
	fake.Advance(time.Minute)

	select {
	case <-flushed:
	case <-time.After(waitTimeout):
		t.Fatal("ticker advance did not cause a periodic flush")
	}
}

// waitForTicker blocks until the exporter's flush loop has created its
// ticker on the fake clock — an event wait on the fake's own state, so
// no sleep-based synchronisation is involved.
func waitForTicker(t *testing.T, fake *clocktest.Fake) {
	t.Helper()
	deadline := time.After(waitTimeout)
	for fake.TickerCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("flush loop never registered its ticker")
		default:
		}
	}
}
