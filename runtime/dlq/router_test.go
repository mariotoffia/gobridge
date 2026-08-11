package dlq_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// Verifies routing with a nil DLQ store is a no-op and returns no error.
func TestRouter_NilStore(t *testing.T) {
	r := dlq.New(nil)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1"})

	err := r.Route(context.Background(), env, "route-1", "", "", "", "", shared.ErrNotFound, 1)
	if err != nil {
		t.Fatalf("nil store should be a no-op, got %v", err)
	}
}

// Verifies Route persists one entry with route, correlation, permanent category, and attempt count.
func TestRouter_WritesEntry(t *testing.T) {
	store := NewFakeStore()
	r := dlq.New(store)

	env := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
		ID:      "msg-1",
		Headers: map[string]any{messaging.HeaderCorrelationID: "corr-abc"},
	})

	err := r.Route(context.Background(), env, "route-1", "bind-1", "", "sess-1", "src-1", shared.ErrNotFound, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", store.Count())
	}

	entry := store.Entries[0]
	if entry.RouteID() != "route-1" {
		t.Fatalf("expected route 'route-1', got %q", entry.RouteID())
	}
	if entry.CorrelationID() != "corr-abc" {
		t.Fatalf("expected correlation ID 'corr-abc', got %q", entry.CorrelationID())
	}
	if entry.Category() != string(shared.ErrorPermanent) {
		t.Fatalf("expected category 'permanent', got %q", entry.Category())
	}
	if entry.Attempts() != 3 {
		t.Fatalf("expected Attempts 3, got %d", entry.Attempts())
	}
}

// Verifies non-domain errors are categorized as unknown in the DLQ entry.
func TestRouter_UnknownError(t *testing.T) {
	store := NewFakeStore()
	r := dlq.New(store)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1"})
	_ = r.Route(context.Background(), env, "r", "", "", "", "", context.DeadlineExceeded, 0)

	if store.Count() != 1 {
		t.Fatalf("expected 1 entry, got %d", store.Count())
	}
	if store.Entries[0].Category() != "unknown" {
		t.Fatalf("expected category 'unknown', got %q", store.Entries[0].Category())
	}
}

// Verifies HasStore returns true when a store is configured.
func TestRouter_HasStore_True(t *testing.T) {
	store := NewFakeStore()
	r := dlq.New(store)

	if !r.HasStore() {
		t.Fatal("HasStore() should return true when store is non-nil")
	}
}

// Verifies HasStore returns false when store is nil.
func TestRouter_HasStore_False(t *testing.T) {
	r := dlq.New(nil)

	if r.HasStore() {
		t.Fatal("HasStore() should return false when store is nil")
	}
}

// Documents RISK: Reason and LastError contain raw err.Error() strings,
// which may include infrastructure details such as hostnames and ports.
// TestRouter_Route_RedactsErrorDetails verifies that DLQ entries use
// sanitized error reasons rather than raw err.Error() to prevent information
// disclosure of internal infrastructure details.
func TestRouter_Route_RedactsErrorDetails(t *testing.T) {
	store := NewFakeStore()
	r := dlq.New(store)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-redact"})

	rawErr := shared.ErrConnectionLost.Wrap(
		fmt.Errorf("connection to db-prod.internal:5432 refused"),
	)

	err := r.Route(context.Background(), env, "route-redact", "bind-1", "", "sess-1", "src-1", rawErr, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", store.Count())
	}

	entry := store.Entries[0]
	const sensitiveSubstring = "db-prod.internal:5432"
	if strings.Contains(entry.Reason(), sensitiveSubstring) {
		t.Fatalf("Reason should NOT contain sensitive details %q, got %q", sensitiveSubstring, entry.Reason())
	}
	if strings.Contains(entry.LastError(), sensitiveSubstring) {
		t.Fatalf("LastError should NOT contain sensitive details %q, got %q", sensitiveSubstring, entry.LastError())
	}
	if entry.Reason() != "connection lost" {
		t.Fatalf("Reason should be the BridgeError message, got %q", entry.Reason())
	}
}

// Verifies that when the DLQ store returns a write error, Route propagates it.
func TestRouter_Route_StoreWriteError_Propagated(t *testing.T) {
	store := NewFakeStore()
	store.WriteErr = fmt.Errorf("disk full")
	r := dlq.NewFromConfig(dlq.Config{Store: store, WriteMaxAttempts: 1})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-fail"})

	err := r.Route(context.Background(), env, "r", "", "", "", "", shared.ErrNotFound, 1)
	if err == nil {
		t.Fatal("expected store write error to be propagated")
	}
	if !errors.Is(err, store.WriteErr) {
		t.Fatalf("expected wrapped error to match store write error, got %q", err.Error())
	}
}

// Verifies all DLQEntry fields are correctly populated from the Route call.
func TestRouter_Route_AllFieldsPopulated(t *testing.T) {
	store := NewFakeStore()
	failedAt := time.Date(2026, 5, 4, 12, 34, 56, 789, time.UTC)
	clk := clocktest.NewAt(failedAt)
	r := dlq.NewFromConfig(dlq.Config{
		Store: store,
		Clock: clk,
	})

	env := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
		ID:      "msg-all",
		Subject: "test/topic",
		Payload: []byte("payload-data"),
		Headers: map[string]any{messaging.HeaderCorrelationID: "corr-xyz"},
	})

	routeErr := shared.ErrThrottled
	err := r.Route(context.Background(), env, "route-all", "bind-all", "", "sess-all", "src-all", routeErr, 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.Count() != 1 {
		t.Fatalf("expected 1 entry, got %d", store.Count())
	}

	entry := store.Entries[0]
	if entry.ID() == "" {
		t.Fatal("entry ID should be generated and non-empty")
	}
	if env := entry.Snapshot(); env.ID() != "msg-all" {
		t.Fatalf("expected envelope ID 'msg-all', got %q", env.ID())
	}
	if entry.RouteID() != "route-all" {
		t.Fatalf("expected RouteID 'route-all', got %q", entry.RouteID())
	}
	if entry.BindingID() != "bind-all" {
		t.Fatalf("expected BindingID 'bind-all', got %q", entry.BindingID())
	}
	if entry.SessionID() != "sess-all" {
		t.Fatalf("expected SessionID 'sess-all', got %q", entry.SessionID())
	}
	if entry.SourceID() != "src-all" {
		t.Fatalf("expected SourceID 'src-all', got %q", entry.SourceID())
	}
	if entry.CorrelationID() != "corr-xyz" {
		t.Fatalf("expected CorrelationID 'corr-xyz', got %q", entry.CorrelationID())
	}
	if entry.Reason() == "" {
		t.Fatal("Reason should not be empty")
	}
	if entry.Category() != string(shared.ErrorTransient) {
		t.Fatalf("expected category %q, got %q", shared.ErrorTransient, entry.Category())
	}
	if entry.ErrorCode() != string(shared.ErrCodeThrottled) {
		t.Fatalf("expected error code %q, got %q", shared.ErrCodeThrottled, entry.ErrorCode())
	}
	if entry.LastError() == "" {
		t.Fatal("LastError should not be empty")
	}
	if !entry.FailedAt().Equal(failedAt) {
		t.Fatalf("expected FailedAt from injected clock %s, got %s", failedAt, entry.FailedAt())
	}
	if entry.Attempts() != 7 {
		t.Fatalf("expected Attempts 7, got %d", entry.Attempts())
	}
}

// Verifies that when the envelope has no correlation ID header, CorrelationID is empty.
func TestRouter_Route_NoCorrelationID(t *testing.T) {
	store := NewFakeStore()
	r := dlq.New(store)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-nocorr"})

	err := r.Route(context.Background(), env, "route-nocorr", "", "", "", "", shared.ErrNotFound, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.Count() != 1 {
		t.Fatalf("expected 1 entry, got %d", store.Count())
	}
	if store.Entries[0].CorrelationID() != "" {
		t.Fatalf("expected empty CorrelationID, got %q", store.Entries[0].CorrelationID())
	}
}

// Verifies the router classifies BridgeError values via the public Route
// surface. This is the package-level analogue of the previous internal
// classifyError helper test.
func TestRouter_ClassifyError_BridgeError(t *testing.T) {
	store := NewFakeStore()
	r := dlq.New(store)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-bridge"})

	testCases := []struct {
		name         string
		err          error
		wantCategory string
		wantCode     string
	}{
		{
			name:         "transient/throttled",
			err:          shared.ErrThrottled,
			wantCategory: string(shared.ErrorTransient),
			wantCode:     string(shared.ErrCodeThrottled),
		},
		{
			name:         "permanent/not_found",
			err:          shared.ErrNotFound,
			wantCategory: string(shared.ErrorPermanent),
			wantCode:     string(shared.ErrCodeNotFound),
		},
		{
			name:         "rejected/invalid_payload",
			err:          shared.ErrInvalidPayload,
			wantCategory: string(shared.ErrorRejected),
			wantCode:     string(shared.ErrCodeInvalidPayload),
		},
		{
			name:         "expired/message_expired",
			err:          shared.ErrMessageExpired,
			wantCategory: string(shared.ErrorExpired),
			wantCode:     string(shared.ErrCodeMessageExpired),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			store.Entries = nil
			err := r.Route(context.Background(), env, "route-classify", "", "", "", "", tc.err, 1)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if store.Count() != 1 {
				t.Fatalf("expected 1 entry, got %d", store.Count())
			}
			entry := store.Entries[0]
			if entry.Category() != tc.wantCategory {
				t.Fatalf("expected category %q, got %q", tc.wantCategory, entry.Category())
			}
			if entry.ErrorCode() != tc.wantCode {
				t.Fatalf("expected error code %q, got %q", tc.wantCode, entry.ErrorCode())
			}
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Production contract: the synchronous DLQ write hold is BOUNDED and OBSERVABLE.
//
// A DLQ-store outage holds the calling route goroutine — and with it a route
// and global concurrency slot — for the whole synchronous write budget. That
// hold is ACCEPTED: the evidence must be durable before the source delivery is
// settled, so the alternative is loss. What the tradeoff requires is that the
// hold is (a) bounded by a value an operator can read, and (b) reported, so a
// DLQ outage is diagnosed from a metric rather than inferred from stalled
// intake.
//
// The bound is derived from the SAME constants the runtime wires
// (runtime/bridge_start.go): RuntimeWriteMaxAttempts attempts, each capped at
// RuntimeWriteTimeout, plus the backoff between them — 2×5s + 500ms = 10.5s.
// ═══════════════════════════════════════════════════════════════════════════

// deadlineRecordingStore fails every write immediately and records the deadline
// the router attached to each attempt, so the per-attempt bound is asserted
// without waiting for it. Signals each attempt on attempts (unbuffered — the
// router blocks until the test reads).
type deadlineRecordingStore struct {
	*FakeStore
	mu         sync.Mutex
	remaining  []time.Duration
	noDeadline int
	attempts   chan struct{}
}

func (s *deadlineRecordingStore) Write(ctx context.Context, _ routing.DLQEntry) error {
	deadline, ok := ctx.Deadline()
	s.mu.Lock()
	if ok {
		s.remaining = append(s.remaining, time.Until(deadline))
	} else {
		s.noDeadline++
	}
	s.mu.Unlock()
	s.attempts <- struct{}{}
	return shared.ErrUnavailable
}

func (s *deadlineRecordingStore) observed() (remaining []time.Duration, noDeadline int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.remaining...), s.noDeadline
}

// TestRouter_BlackHoledStoreHoldIsBoundedAndMetered_ProductionContract pins the
// accepted tradeoff: a black-holed DLQ store holds the caller for at
// most Router.MaxWriteHold(), every attempt carries the per-write deadline that
// makes that bound real, the caller is told the evidence is NOT durable (so it
// leaves the source unsettled), and the hold is emitted as MetricDLQWriteHold.
func TestRouter_BlackHoledStoreHoldIsBoundedAndMetered_ProductionContract(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	metrics := &ports.RecordingExporter{}
	store := &deadlineRecordingStore{FakeStore: NewFakeStore(), attempts: make(chan struct{})}

	router := dlq.NewFromConfig(dlq.Config{
		Store:            store,
		Clock:            fake,
		Metrics:          metrics,
		WriteTimeout:     dlq.RuntimeWriteTimeout,
		WriteMaxAttempts: dlq.RuntimeWriteMaxAttempts,
	})

	require.Equal(t, 10500*time.Millisecond, router.MaxWriteHold(),
		"the documented production DLQ hold ceiling (2 attempts × 5s + 500ms backoff)")

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "hold-bound-1",
		Subject: "test/hold",
		Payload: []byte("payload"),
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- router.Route(context.Background(), env,
			"route-1", "bind-1", "devices/1/state", "sess-1", "src-1",
			shared.ErrUnavailable, 1)
	}()

	<-store.attempts // attempt 1 — immediate
	require.Eventually(t, func() bool { return fake.TimerCount() == 1 },
		time.Second, time.Millisecond, "backoff timer for attempt 2 not registered")
	fake.Advance(500 * time.Millisecond)
	<-store.attempts // attempt 2 — after the injected-clock backoff

	err := <-errCh
	require.Error(t, err, "a failed DLQ write must be reported so the caller leaves the source unsettled")

	remaining, noDeadline := store.observed()
	require.Len(t, remaining, dlq.RuntimeWriteMaxAttempts, "exactly the configured number of attempts")
	assert.Zero(t, noDeadline, "every write attempt must carry a deadline; an undeadlined attempt is an unbounded hold")
	for i, rem := range remaining {
		assert.Greater(t, rem, time.Duration(0), "attempt %d deadline already expired", i+1)
		assert.LessOrEqual(t, rem, dlq.RuntimeWriteTimeout, "attempt %d exceeds the per-write bound", i+1)
	}

	held := metrics.FindEntries(shared.MetricDLQWriteHold)
	require.Len(t, held, 1, "every Route must report the hold it cost")
	assert.LessOrEqual(t, held[0].Duration, router.MaxWriteHold(),
		"the reported hold must never exceed the documented ceiling")
	assert.Equal(t, 500*time.Millisecond, held[0].Duration,
		"the hold is measured on the injected clock (both attempts returned instantly; only the backoff elapsed)")
	require.Len(t, metrics.FindEntries(shared.MetricDLQWriteFailures), 1,
		"a permanently failed write must raise the failure counter operators alarm on")
}

// TestRouter_SuccessfulWriteReportsHold_ProductionContract proves the hold
// signal is not outage-only: a healthy DLQ write also reports its hold, so the
// alarm threshold in the runbook is set against a real baseline rather than
// against silence.
func TestRouter_SuccessfulWriteReportsHold_ProductionContract(t *testing.T) {
	metrics := &ports.RecordingExporter{}
	router := dlq.NewFromConfig(dlq.Config{
		Store:            NewFakeStore(),
		Clock:            clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		Metrics:          metrics,
		WriteTimeout:     dlq.RuntimeWriteTimeout,
		WriteMaxAttempts: dlq.RuntimeWriteMaxAttempts,
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "hold-ok-1", Subject: "test/hold"})
	require.NoError(t, router.Route(context.Background(), env,
		"route-1", "bind-1", "devices/1/state", "sess-1", "src-1", shared.ErrUnavailable, 1))

	held := metrics.FindEntries(shared.MetricDLQWriteHold)
	require.Len(t, held, 1, "a successful write must report its hold too")
	assert.Zero(t, held[0].Duration, "no backoff elapsed on the injected clock for a first-attempt success")
	assert.Empty(t, metrics.FindEntries(shared.MetricDLQWriteFailures))
}
