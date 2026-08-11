package sqs

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// captureHandler is a slog.Handler that records every emitted record so a
// test can assert on the structured attributes. Enabled always returns true
// so LevelTrace/Debug/Error records are all captured (logging.TraceEnabled
// therefore reports true). Safe for concurrent Handle calls from background
// goroutines (auto-extend, poll loop) while a test reads records().
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// records returns a snapshot copy of the captured records.
func (h *captureHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, len(h.records))
	copy(out, h.records)
	return out
}

// findAttr returns the first record attribute with the given key across all
// captured records.
func findAttr(records []slog.Record, key string) (slog.Value, bool) {
	for _, r := range records {
		var (
			val   slog.Value
			found bool
		)
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key {
				val, found = a.Value, true
				return false
			}
			return true
		})
		if found {
			return val, true
		}
	}
	return slog.Value{}, false
}

// TestSettlementLog_MessageIDIsStringValue_NotMethodValue is the regression
// for Finding 1. Several settlement/auto-extend log sites logged d.env.ID —
// the bound METHOD value (a func) — instead of calling d.env.ID(). slog then
// rendered message_id as an opaque function pointer, identical for every
// message, destroying the diagnostic value of the field. Assert the logged
// message_id is the actual string ID.
func TestSettlementLog_MessageIDIsStringValue_NotMethodValue(t *testing.T) {
	t.Parallel()

	h := &captureHandler{}
	logger := slog.New(h)

	ctx := context.Background()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-42", Payload: []byte("x"), CreatedAt: time.Now()})
	fake := clocktest.New()
	// vis=0, autoExtend=false → no background goroutine; Ack logs at trace.
	d := newDelivery(ctx, env, &mockSQSClient{}, "https://test-queue", "receipt", 0, false, nil, logger, nil, fake)

	if err := d.Ack(ctx); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	val, ok := findAttr(h.snapshot(), "message_id")
	if !ok {
		t.Fatal("no log record carried a message_id attribute")
	}
	if val.Kind() != slog.KindString {
		t.Fatalf("message_id attr kind = %v, want String "+
			"(Finding 1: logged the bound ID method value, not the ID)", val.Kind())
	}
	if got := val.String(); got != "msg-42" {
		t.Fatalf("message_id = %q, want %q", got, "msg-42")
	}
}

// TestAutoExtend_UserExtendRefreshesDeadline_NoPrematureCancel is the
// regression for Finding 2. A user Extend to now+120s stores the new
// visibility atomically, but the auto-extend loop must ALSO see the refreshed
// window deadline: otherwise a transient auto-extend CMV failure at the old
// (2s) boundary is mistaken for a lapsed window and cancels still-locked work.
//
// CMV call #1 is the user Extend (succeeds → deadline pushed to t0+120s);
// every subsequent auto-extend tick fails. Advancing to the OLD 2s deadline
// fires one failing tick; with the fix the refreshed deadline is not lapsed,
// so processing is NOT cancelled.
func TestAutoExtend_UserExtendRefreshesDeadline_NoPrematureCancel(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	mock := &mockSQSClient{}
	mock.ChangeMessageVisibilityFn = func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
		if callCount.Add(1) == 1 {
			return &awssqs.ChangeMessageVisibilityOutput{}, nil // the user Extend
		}
		return nil, errors.New("transient") // every auto-extend tick fails
	}

	ctx := context.Background()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e-extend", Payload: []byte("x"), CreatedAt: time.Now()})
	fake := clocktest.New()
	var cancelled atomic.Bool
	// vis=2 → interval floored to 1s, initial deadline at t0+2s.
	d := newDelivery(ctx, env, mock, "https://test-queue", "receipt-extend", 2, true,
		func() { cancelled.Store(true) }, nil, nil, fake)
	defer func() { d.stopAutoExtend(); d.cleanupContext() }()

	wait.Until(t, time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	// User extends the lock far past the initial 2s window: CMV #1 succeeds,
	// pushing both visibilityTimeout and the window deadline to t0+120s.
	if err := d.Extend(ctx, fake.Now().Add(120*time.Second)); err != nil {
		t.Fatalf("Extend: %v", err)
	}
	wait.Until(t, time.Second, "extend CMV issued", func() bool {
		return callCount.Load() >= 1
	})

	// Advance to the OLD deadline (t=2s) and fire one auto-extend tick that
	// fails transiently. With the stale-deadline bug this failure sees the
	// window as lapsed (now t0+2s is not before the stale t0+2s) and cancels;
	// with the fix the refreshed t0+120s deadline is not lapsed, so the loop
	// retries and processing survives.
	fake.Advance(2 * time.Second)
	wait.Until(t, time.Second, "failing auto-extend tick observed", func() bool {
		return callCount.Load() >= 2
	})

	if got := wait.StableFor(t, cancelled.Load, 50*time.Millisecond, 500*time.Millisecond); got {
		t.Fatal("processing was cancelled by a transient auto-extend failure inside the " +
			"user-extended window (Finding 2: stale window deadline)")
	}
}

// TestAutoExtend_DeadReceiptHandle_CancelsImmediately is the regression for
// Finding 8 (and covers the Finding 5 SQSAutoExtendFailures counter). When a
// ChangeMessageVisibility returns MessageNotInflight / ReceiptHandleIsInvalid
// the lock is already lost; retrying only widens the duplicate window, so the
// loop must cancel processing immediately instead of treating it as transient.
//
// vis=30 → interval 10s, deadline t0+30s, so on the FIRST failure neither the
// window-lapse nor the 3-consecutive-failure ceiling can fire — isolating the
// dead-handle fast-cancel.
func TestAutoExtend_DeadReceiptHandle_CancelsImmediately(t *testing.T) {
	t.Parallel()

	mock := &mockSQSClient{}
	mock.ChangeMessageVisibilityFn = func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
		return nil, &sqstypes.ReceiptHandleIsInvalid{Message: aws.String("handle gone")}
	}

	ctx := context.Background()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e-dead", Payload: []byte("x"), CreatedAt: time.Now()})
	fake := clocktest.New()
	rec := &ports.RecordingExporter{}
	var cancelled atomic.Bool
	d := newDelivery(ctx, env, mock, "https://test-queue", "receipt-dead", 30, true,
		func() { cancelled.Store(true) }, nil, rec, fake)
	defer func() { d.stopAutoExtend(); d.cleanupContext() }()

	wait.Until(t, time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	// One tick at t=10s: CMV returns ReceiptHandleIsInvalid → immediate cancel.
	fake.Advance(10 * time.Second)
	wait.Until(t, time.Second, "processing cancelled on dead handle", func() bool {
		return cancelled.Load()
	})

	mock.mu.Lock()
	n := len(mock.ChangeVisibilityCalls)
	mock.mu.Unlock()
	if n != 1 {
		t.Fatalf("ChangeMessageVisibility calls: want exactly 1 (no retry on a dead handle), got %d", n)
	}
	if got := len(rec.FindEntries(MetricSQSAutoExtendFailures)); got != 1 {
		t.Fatalf("%s entries: want 1, got %d", MetricSQSAutoExtendFailures, got)
	}
}

// TestAck_EmitsSettlementErrorCounter covers Finding 5: a failed settlement
// (DeleteMessage) must increment the SQSSettlementErrors counter, not just
// log a warning.
func TestAck_EmitsSettlementErrorCounter(t *testing.T) {
	t.Parallel()

	rec := &ports.RecordingExporter{}
	mock := &mockSQSClient{}
	mock.DeleteMessageFn = func(_ context.Context, _ *awssqs.DeleteMessageInput, _ ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
		return nil, errors.New("delete boom")
	}

	ctx := context.Background()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e-set", Payload: []byte("x"), CreatedAt: time.Now()})
	fake := clocktest.New()
	d := newDelivery(ctx, env, mock, "https://test-queue", "receipt", 0, false, nil, nil, rec, fake)

	if err := d.Ack(ctx); err == nil {
		t.Fatal("Ack: want error from failing DeleteMessage, got nil")
	}
	if got := len(rec.FindEntries(MetricSQSSettlementErrors)); got != 1 {
		t.Fatalf("%s entries: want 1, got %d", MetricSQSSettlementErrors, got)
	}
}

// TestRetry_EmitsSettlementErrorCounter covers Finding 5 for the Retry
// settlement path (ChangeMessageVisibility failure).
func TestRetry_EmitsSettlementErrorCounter(t *testing.T) {
	t.Parallel()

	rec := &ports.RecordingExporter{}
	mock := &mockSQSClient{}
	mock.ChangeMessageVisibilityFn = func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
		return nil, errors.New("cmv boom")
	}

	ctx := context.Background()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e-retry", Payload: []byte("x"), CreatedAt: time.Now()})
	fake := clocktest.New()
	d := newDelivery(ctx, env, mock, "https://test-queue", "receipt", 0, false, nil, nil, rec, fake)

	if err := d.Retry(ctx, 5*time.Second, errors.New("boom")); err == nil {
		t.Fatal("Retry: want error from failing ChangeMessageVisibility, got nil")
	}
	if got := len(rec.FindEntries(MetricSQSSettlementErrors)); got != 1 {
		t.Fatalf("%s entries: want 1, got %d", MetricSQSSettlementErrors, got)
	}
}
