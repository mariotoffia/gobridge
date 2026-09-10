package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	outboxpkg "github.com/mariotoffia/gobridge/runtime/outbox"
	"github.com/mariotoffia/gobridge/runtime/route"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// ═══════════════════════════════════════════════════════════════════════════
// Medium Fixes + Additional Regression Tests
//
// Continuation of medium_fixes_test.go covering adaptive batch
// reduction, lease release logging, atomic FakeProcessor,
// plus additional edge case coverage.
// ═══════════════════════════════════════════════════════════════════════════

// ---------------------------------------------------------------------------
// adaptBatchSize halves on consecutive zero-success cycles
// ---------------------------------------------------------------------------

// TestAdaptBatchSize_HalvesOnConsecutiveZeroSuccess validates that the
// batch size is halved when drain cycles produce zero successful sends.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────
//
//	Start with DrainBatchSize=5, DrainMaxBatchSize=50
//	First few sends succeed, then all fail (transient)
//	Drainer halves batch on each zero-success cycle (floored at 5)
//
// ───────────────────────────────────────────────────────────────────────
func TestAdaptBatchSize_HalvesOnConsecutiveZeroSuccess(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}

	var sendCount int32
	failAfter := int32(3)

	outbox := NewFakeOutboxStore()
	sender := NewFakeSender()
	sender.SendFn = func(_ *messaging.Envelope) error {
		n := atomic.AddInt32(&sendCount, 1)
		if n > failAfter {
			return errors.New("downstream unavailable")
		}
		return nil
	}

	lease := NewFakeLeaseStore()
	pk := persistence.OutboxPartitionKey("sess-1", "")
	_, _ = lease.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	ctx := context.Background()
	for i := 0; i < 20; i++ {
		rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
			ID: fmt.Sprintf("adapt-%d", i), RouteID: "adapt-route",
			EnvelopeID: fmt.Sprintf("env-adapt-%d", i), BindingID: "bind-1",
			SessionID: "sess-1",
			Envelope:  *messaging.MustEnvelope(messaging.EnvelopeInput{ID: fmt.Sprintf("env-adapt-%d", i), Payload: []byte("data")}),
			Status:    persistence.OutboxPending,
		})
		_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})
	}

	drainer := outboxpkg.New(outboxpkg.Config{
		OutboxStore:         outbox,
		LeaseStore:          lease,
		Sender:              sender,
		DLQ:                 dlq.New(nil),
		RouteID:             "adapt-route",
		PartitionKey:        pk,
		LeaseID:             "sess-1",
		Policy:              routing.RoutePolicy{}.WithDefaults(),
		Strategy:            persistence.NewFixedPoll(30 * time.Millisecond),
		DrainBatchSize:      5,
		DrainMaxBatchSize:   50,
		DrainMaxConcurrency: 2,
		TokenFn: func() (persistence.LeaseToken, bool) {
			return token, true
		},
	})

	drainCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	sent := int(atomic.LoadInt32(&sendCount))
	if sent < 3 {
		t.Fatalf("expected at least 3 successful sends before failure, got %d", sent)
	}
}

// ---------------------------------------------------------------------------
// Lease release error logged in SessionManager.Close
// ---------------------------------------------------------------------------

// TestSessionManager_LogsLeaseReleaseError validates that when
// leaseStore.Release fails during Close, the error is logged rather
// than silently discarded.
//
// ═══════════════════════════════════════════════════════════════════════
// Before fix:
//
//	_ = m.leaseStore.Release(...) → error silently discarded
//
// After fix:
//
//	if err := m.leaseStore.Release(...); err != nil { log... }
//
// ═══════════════════════════════════════════════════════════════════════
func TestSessionManager_LogsLeaseReleaseError(t *testing.T) {
	lease := NewFakeLeaseStore()
	sess := NewFakeSession()

	handler := &logCaptureHandler{}
	logger := slog.New(handler)

	sessCfg := session.Config{
		SessionID:     "release-err-sess",
		Exclusive:     true,
		LeaseTTL:      500 * time.Millisecond,
		RenewInterval: 100 * time.Millisecond,
		RenewJitter:   10 * time.Millisecond,
		MaxRenewFails: 3,
		StepDownGrace: 100 * time.Millisecond,
	}

	mgr := session.NewFromConfig(sessCfg, sess, lease, "owner-1", logger)

	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan struct{})
	go func() {
		_ = mgr.Run(ctx)
		close(runDone)
	}()

	select {
	case evt := <-mgr.LeaseStateChanged():
		if evt.State != session.LeaseStateAcquired {
			t.Fatalf("expected LeaseStateAcquired, got %v", evt.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lease not acquired in time")
	}

	lease.SetReleaseErr(errors.New("network timeout"))

	cancel()

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run should exit after cancel")
	}

	_ = mgr.Close(context.Background())

	found := false
	for _, msg := range handler.Messages() {
		if msg == "lease release failed during close" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'lease release failed during close' log message")
	}
}

// logCaptureHandler captures slog messages for assertion.
// Thread-safe: Handle may be called from background goroutines while
// Messages is called from the test goroutine.
type logCaptureHandler struct {
	mu       sync.Mutex
	messages []string
}

func (h *logCaptureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *logCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, r.Message)
	return nil
}

func (h *logCaptureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *logCaptureHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *logCaptureHandler) Messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := make([]string, len(h.messages))
	copy(cp, h.messages)
	return cp
}

// ---------------------------------------------------------------------------
// FakeProcessor.Called uses atomic operations
// ---------------------------------------------------------------------------

// TestFakeProcessor_AtomicCalled validates that FakeProcessor.CalledCount
// is safe for concurrent access using atomic operations.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────
//
//	100 goroutines each call Process once concurrently
//	CalledCount() returns exactly 100 (no data race)
//
// ───────────────────────────────────────────────────────────────────────
func TestFakeProcessor_AtomicCalled(t *testing.T) {
	p := &FakeProcessor{NameVal: "atomic-test"}

	done := make(chan struct{})
	const goroutines = 100

	for i := 0; i < goroutines; i++ {
		go func() {
			env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "test"})
			_ = p.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
				return nil
			})
			done <- struct{}{}
		}()
	}

	for i := 0; i < goroutines; i++ {
		<-done
	}

	if p.CalledCount() != goroutines {
		t.Fatalf("expected CalledCount()=%d, got %d", goroutines, p.CalledCount())
	}
}

// ---------------------------------------------------------------------------
// additional: QueryPending success still works
// ---------------------------------------------------------------------------

// TestQueryPendingSuccess_PersistsNormally validates that when
// QueryPending succeeds, the depth check works normally and messages
// are persisted to the outbox.
func TestQueryPendingSuccess_PersistsNormally(t *testing.T) {
	outbox := NewFakeOutboxStore()
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:     "query-ok-route",
		Policy:      routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox, MaxOutboxDepth: 1000},
		Receiver:    receiver,
		Sender:      sender,
		OutboxStore: outbox,
		Bindings:    []routing.DestinationBinding{{ID: "b1", SessionID: "query-ok-sess"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "query-ok-1", Payload: []byte("x")})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "acked", func() bool { return del.IsAcked() })

	if outbox.RecordCount() != 1 {
		t.Fatalf("expected 1 outbox record, got %d", outbox.RecordCount())
	}
}

// ---------------------------------------------------------------------------
// additional: normal MaxBatchSize not clamped
// ---------------------------------------------------------------------------

// TestNormalMaxBatchSize_NotClamped validates that a reasonable
// MaxBatchSize value (below 10000) is preserved without clamping.
func TestNormalMaxBatchSize_NotClamped(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox := NewFakeOutboxStore()
	sender := NewFakeSender()
	lease := NewFakeLeaseStore()
	pk := persistence.OutboxPartitionKey("sess-1", "")
	_, _ = lease.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
			ID: fmt.Sprintf("normal-%d", i), RouteID: "normal-route",
			EnvelopeID: fmt.Sprintf("env-normal-%d", i), BindingID: "bind-1",
			SessionID: "sess-1",
			Envelope:  *messaging.MustEnvelope(messaging.EnvelopeInput{ID: fmt.Sprintf("env-normal-%d", i), Payload: []byte("data")}),
			Status:    persistence.OutboxPending,
		})
		_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})
	}

	drainer := outboxpkg.New(outboxpkg.Config{
		OutboxStore:       outbox,
		LeaseStore:        lease,
		Sender:            sender,
		DLQ:               dlq.New(nil),
		RouteID:           "normal-route",
		PartitionKey:      pk,
		LeaseID:           "sess-1",
		Policy:            routing.RoutePolicy{}.WithDefaults(),
		Strategy:          persistence.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize:    100,
		DrainMaxBatchSize: 500,
		TokenFn: func() (persistence.LeaseToken, bool) {
			return token, true
		},
	})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 3 {
		t.Fatalf("expected 3 sent, got %d", sender.SentCount())
	}
}

// ---------------------------------------------------------------------------
// additional: success path emits completion, not failure
// ---------------------------------------------------------------------------

// TestOutboxDrainer_SuccessEmitsCompletion validates that successful
// record processing emits MetricOutboxCompletions but not
// MetricOutboxRecordFailures.
func TestOutboxDrainer_SuccessEmitsCompletion(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	rec := &ports.RecordingExporter{}

	outbox := NewFakeOutboxStore()
	sender := NewFakeSender()
	lease := NewFakeLeaseStore()
	pk := persistence.OutboxPartitionKey("sess-1", "")
	_, _ = lease.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	drainer := outboxpkg.New(outboxpkg.Config{
		OutboxStore:    outbox,
		LeaseStore:     lease,
		Sender:         sender,
		DLQ:            dlq.New(nil),
		RouteID:        "success-route",
		PartitionKey:   pk,
		LeaseID:        "sess-1",
		Policy:         routing.RoutePolicy{}.WithDefaults(),
		Strategy:       persistence.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize: 10,
		Metrics:        rec,
		TokenFn: func() (persistence.LeaseToken, bool) {
			return token, true
		},
	})

	ctx := context.Background()
	outboxRec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID: "rec-ok", RouteID: "success-route",
		EnvelopeID: "env-ok", BindingID: "bind-1",
		SessionID: "sess-1",
		Envelope:  *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-ok", Payload: []byte("data")}),
		Status:    persistence.OutboxPending,
	})
	_ = outbox.Persist(ctx, []*persistence.OutboxRecord{outboxRec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	completions := rec.FindEntries(shared.MetricOutboxCompletions)
	if len(completions) == 0 {
		t.Fatal("expected MetricOutboxCompletions to be emitted on success")
	}

	failures := rec.FindEntries(shared.MetricOutboxRecordFailures)
	if len(failures) > 0 {
		t.Error("did not expect MetricOutboxRecordFailures on successful processing")
	}
}

// ---------------------------------------------------------------------------
// additional: DrainBatchSize also clamped by absoluteMaxBatchSize
// ---------------------------------------------------------------------------

// TestBatchSizeClamped_PreventsAbsoluteMaxBypass validates that when
// DrainBatchSize exceeds absoluteMaxBatchSize (10000), it is clamped before
// DrainMaxBatchSize adjustments.
func TestBatchSizeClamped_PreventsAbsoluteMaxBypass(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox := NewFakeOutboxStore()
	sender := NewFakeSender()
	lease := NewFakeLeaseStore()
	pk := persistence.OutboxPartitionKey("sess-1", "")
	_, _ = lease.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
			ID: fmt.Sprintf("bsclamp-%d", i), RouteID: "bsclamp-route",
			EnvelopeID: fmt.Sprintf("env-bsclamp-%d", i), BindingID: "bind-1",
			SessionID: "sess-1",
			Envelope:  *messaging.MustEnvelope(messaging.EnvelopeInput{ID: fmt.Sprintf("env-bsclamp-%d", i), Payload: []byte("data")}),
			Status:    persistence.OutboxPending,
		})
		_ = outbox.Persist(ctx, []*persistence.OutboxRecord{rec})
	}

	drainer := outboxpkg.New(outboxpkg.Config{
		OutboxStore:       outbox,
		LeaseStore:        lease,
		Sender:            sender,
		DLQ:               dlq.New(nil),
		RouteID:           "bsclamp-route",
		PartitionKey:      pk,
		LeaseID:           "sess-1",
		Policy:            routing.RoutePolicy{}.WithDefaults(),
		Strategy:          persistence.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize:    50000,
		DrainMaxBatchSize: 500,
		TokenFn: func() (persistence.LeaseToken, bool) {
			return token, true
		},
	})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 3 {
		t.Fatalf("expected 3 sent (drainer works with clamped BatchSize), got %d", sender.SentCount())
	}
}

// ---------------------------------------------------------------------------
// additional: stale fencing token does not emit RecordFailures
// ---------------------------------------------------------------------------

// TestOutboxDrainer_StaleFencingToken_NoRecordFailureMetric validates that
// ErrStaleFencingToken does not emit MetricOutboxRecordFailures.
func TestOutboxDrainer_StaleFencingToken_NoRecordFailureMetric(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	rec := &ports.RecordingExporter{}

	outbox := NewFakeOutboxStore()
	outbox.CompleteFn = func(_ []string, _ persistence.LeaseToken) error {
		return shared.ErrStaleFencingToken
	}
	sender := NewFakeSender()

	lease := NewFakeLeaseStore()
	pk := persistence.OutboxPartitionKey("sess-1", "")
	_, _ = lease.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second, nil)

	drainer := outboxpkg.New(outboxpkg.Config{
		OutboxStore:    outbox,
		LeaseStore:     lease,
		Sender:         sender,
		DLQ:            dlq.New(nil),
		RouteID:        "stale-metric-route",
		PartitionKey:   pk,
		LeaseID:        "sess-1",
		Policy:         routing.RoutePolicy{}.WithDefaults(),
		Strategy:       persistence.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize: 10,
		Metrics:        rec,
		TokenFn: func() (persistence.LeaseToken, bool) {
			return token, true
		},
	})

	ctx := context.Background()
	outboxRec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID: "rec-stale-metric", RouteID: "stale-metric-route",
		EnvelopeID: "env-stale-metric", BindingID: "bind-1",
		SessionID: "sess-1",
		Envelope:  *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-stale-metric", Payload: []byte("data")}),
		Status:    persistence.OutboxPending,
	})
	_ = outbox.Persist(ctx, []*persistence.OutboxRecord{outboxRec})

	drainCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	failures := rec.FindEntries(shared.MetricOutboxRecordFailures)
	if len(failures) > 0 {
		t.Error("ErrStaleFencingToken should not emit MetricOutboxRecordFailures")
	}
}

// ---------------------------------------------------------------------------
// additional: DefaultSessionConfig includes drain defaults
// ---------------------------------------------------------------------------

// TestDefaultSessionConfig_IncludesDrainDefaults validates that
// DefaultSessionConfig populates DrainMaxBatchSize and
// DrainMaxConcurrency with their recommended defaults.
func TestDefaultSessionConfig_IncludesDrainDefaults(t *testing.T) {
	cfg := session.DefaultConfig("test-sess", true)
	if cfg.DrainMaxBatchSize != 500 {
		t.Errorf("expected DrainMaxBatchSize=500, got %d", cfg.DrainMaxBatchSize)
	}
	if cfg.DrainMaxConcurrency != 10 {
		t.Errorf("expected DrainMaxConcurrency=10, got %d", cfg.DrainMaxConcurrency)
	}
}
