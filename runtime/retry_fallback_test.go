package runtime_test

// ═══════════════════════════════════════════════════════════════════════
// MQTT Delivery.Retry Unsupported: retryOrFallback tests
//
// When a source transport does not support Retry (returns ErrNotSupported),
// the RouteRunner must fall back to DLQ routing with "retry_unsupported"
// category rather than propagating a permanent error that kills the receiver.
//
// ┌───────────┐    send/proc    ┌──────────┐   ErrNotSupported   ┌─────────┐
// │  Receiver │───────err──────▶│ del.Retry│───────────────────▶│  DLQ    │
// └───────────┘                 └──────────┘                     └─────────┘
//                                    │ nil                          │
//                                    ▼                              ▼
//                               del.Ack()                     emitDLQ("retry_unsupported")
//
// Summary:
// ┌──────┬─────────────────────────────────────────────────┬──────────┐
// │ ID   │ Description                                     │ Type     │
// ├──────┼─────────────────────────────────────────────────┼──────────┤
// │ T001 │ DirectHold + unsupported retry → DLQ fallback   │ unit     │
// │ T002 │ DirectHold + unsupported + DLQ fails → error    │ unit     │
// │ T003 │ Processor error + unsupported retry → DLQ       │ unit     │
// │ T004 │ SharedOutbox persist + unsupported retry → DLQ  │ unit     │
// │ T005 │ Expired + DLQ fail + unsupported retry → DLQ    │ unit     │
// │ T006 │ Resolve error + unsupported retry → DLQ         │ unit     │
// │ T007 │ Retry supported → normal retry, no DLQ          │ unit     │
// └──────┴─────────────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════

import (
	"context"
	"errors"
	"sync/atomic"
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
	"github.com/mariotoffia/gobridge/runtime/route"
)

// ---------------------------------------------------------------------------
// FailOnceDLQStore — first Write fails, subsequent writes succeed
// ---------------------------------------------------------------------------

type FailOnceDLQStore struct {
	inner     *FakeDLQStore
	failsLeft int32
}

func NewFailOnceDLQStore() *FailOnceDLQStore {
	return &FailOnceDLQStore{inner: NewFakeDLQStore(), failsLeft: 1}
}

func (s *FailOnceDLQStore) Write(ctx context.Context, entry routing.DLQEntry) error {
	if atomic.AddInt32(&s.failsLeft, -1) >= 0 {
		return errors.New("store temporarily down")
	}
	return s.inner.Write(ctx, entry)
}

func (s *FailOnceDLQStore) List(ctx context.Context, filter routing.DLQFilter) ([]routing.DLQEntry, error) {
	return s.inner.List(ctx, filter)
}

func (s *FailOnceDLQStore) Get(ctx context.Context, id string) (routing.DLQEntry, error) {
	return s.inner.Get(ctx, id)
}

func (s *FailOnceDLQStore) Delete(ctx context.Context, ids []string) (int, error) {
	return s.inner.Delete(ctx, ids)
}

func (s *FailOnceDLQStore) DeleteByFilter(ctx context.Context, filter routing.DLQFilter) (int, error) {
	return s.inner.DeleteByFilter(ctx, filter)
}

func (s *FailOnceDLQStore) Purge(ctx context.Context, before time.Time) (int, error) {
	return s.inner.Purge(ctx, before)
}

func (s *FailOnceDLQStore) Count() int { return s.inner.Count() }

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestDirectHold_RetryUnsupported_FallsToDLQ validates that when
// del.Retry returns ErrNotSupported on a recoverable send failure,
// the message is routed to DLQ with "retry_unsupported" category
// and the delivery is acked.
//
// Data flow:
// ───────────────────────────────────────────────────────────────
//
//	Sender → ErrUnavailable → del.Retry → ErrNotSupported
//	      → DLQ.Route(reason=ErrUnavailable) → ✓
//	      → del.Ack → ✓
//
// ───────────────────────────────────────────────────────────────
//
// Assertions:
//   - Delivery is acked
//   - DLQ contains 1 entry
//   - DLQEntries metric emitted with category "retry_unsupported"
func TestDirectHold_RetryUnsupported_FallsToDLQ(t *testing.T) {
	rec := &ports.RecordingExporter{}
	receiver, sender, dlqStore, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
		// One send per delivery: this pins the replay decision, not the
		// in-process send retry that precedes it.
		cfg.Policy.SendRetryBudget = routing.SendRetryBudgetDisabled
		cfg.Metrics = rec
	})
	sender.SendErr = shared.ErrUnavailable

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(func() *messaging.Envelope {
		e := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-retry-unsup", Payload: []byte("data")})
		_ = e.SetExpiry(time.Now().Add(time.Hour))
		return e
	}())
	del.RetryFnErr = shared.ErrNotSupported

	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "delivery acked via DLQ fallback", del.IsAcked)

	if dlqStore.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry (retry_unsupported fallback), got %d", dlqStore.Count())
	}

	dlqCounters := rec.FindEntries(shared.MetricDLQEntries)
	found := false
	for _, entry := range dlqCounters {
		for _, tag := range entry.Tags {
			if tag.Key == shared.TagKeyCategory && tag.Value == "retry_unsupported" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected DLQEntries metric with category=retry_unsupported")
	}
}

func TestDirectHold_RetryUnsupported_DLQAlsoFails_CountsTerminalLoss(t *testing.T) {
	rec := &ports.RecordingExporter{}
	hook := &recordingHook{}
	store := NewFakeDLQStore()
	storeFailure := errors.New("DLQ unavailable")
	store.WriteErr = storeFailure
	clk := clocktest.NewAt(time.Unix(0, 0))
	_, sender, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
		cfg.Policy.MaxInFlight = 1
		// One send per delivery: this pins the replay decision, not the
		// in-process send retry that precedes it.
		cfg.Policy.SendRetryBudget = routing.SendRetryBudgetDisabled
		cfg.Clock = clk
		cfg.Metrics = rec
		cfg.Hook = hook
		cfg.DLQ = dlq.NewFromConfig(dlq.Config{
			Store: store, Clock: clk, Metrics: rec, WriteMaxAttempts: 1,
		})
	})
	sender.SendErr = shared.ErrUnavailable
	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "failed-delivery", Payload: []byte("payload"),
	}))
	del.RetryFnErr = shared.ErrNotSupported

	err := runner.HandleDelivery(t.Context(), del)

	require.ErrorIs(t, err, storeFailure)
	assert.True(t, del.IsAcked())
	assert.Zero(t, store.Count())
	assert.Zero(t, runner.InFlight())
	assert.Empty(t, rec.FindEntries(shared.MetricDLQEntries))
	drops := rec.FindEntries(shared.MetricMessagesDropped)
	require.Len(t, drops, 1)
	assert.EqualValues(t, 1, drops[0].IValue)
	assert.Contains(t, drops[0].Tags, shared.Tag{Key: shared.TagKeyReason, Value: "retry_unsupported_dlq_failed"})
	require.Len(t, hook.Settled(), 1)
	assert.ErrorIs(t, hook.Settled()[0].Err, storeFailure)
	assert.True(t, hook.Settled()[0].Terminal)

	sender.SendErr = nil
	next := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "following-delivery"}))
	require.NoError(t, runner.HandleDelivery(t.Context(), next))
	assert.True(t, next.IsAcked())
	assert.Equal(t, 1, sender.SentCount())
}

func TestRetryUnsupported_FailedDLQAcrossDispatchBranches(t *testing.T) {
	for _, phase := range []string{"processor", "resolver", "expired", "filtered", "replay cap", "outbox persist"} {
		t.Run(phase, func(t *testing.T) {
			clk := clocktest.NewAt(time.Unix(1000, 0))
			rec := &ports.RecordingExporter{}
			hook := &recordingHook{}
			failure := errors.New("DLQ unavailable")
			store := NewFakeDLQStore()
			store.WriteErr = failure
			_, sender, _, outbox, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
				cfg.Clock, cfg.Metrics, cfg.Hook = clk, rec, hook
				cfg.DLQ = dlq.NewFromConfig(dlq.Config{Store: store, Clock: clk, Metrics: rec, WriteMaxAttempts: 1})
				cfg.Policy.MaxReplayAttempts = 3
				switch phase {
				case "processor":
					cfg.Processors = []ports.Processor{&FakeProcessor{NameVal: "reject", ProcessErr: shared.ErrInvalidPayload}}
				case "resolver":
					cfg.Resolver = &FakeResolver{ResolveErr: shared.ErrInvalidTopic}
				case "replay cap":
					// One send per delivery: this pins the replay-cap decision,
					// not the in-process send retry that precedes it.
					cfg.Policy.SendRetryBudget = routing.SendRetryBudgetDisabled
				case "filtered":
					cfg.Policy.OnFiltered = routing.FilteredDLQ
					cfg.Processors = []ports.Processor{&FakeProcessor{NameVal: "filter", ProcessErr: shared.ErrMessageFiltered}}
				case "outbox persist":
					cfg.Policy.DeliveryMode = routing.DeliverySharedOutbox
					cfg.Bindings = []routing.DestinationBinding{{ID: "b", SessionID: "drain"}}
					cfg.Resolver = &FakeResolver{Plans: []routing.DispatchPlan{{BindingID: "b", Address: "queue"}}}
				}
			})
			env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "failed", CreatedAt: time.Unix(1, 0)})
			if phase == "expired" {
				require.NoError(t, env.SetExpiry(time.Unix(999, 0)))
			}
			if phase == "replay cap" {
				env.SetHeader("sqs.ApproximateReceiveCount", 3)
				sender.SendErr = shared.ErrUnavailable
			}
			outbox.PersistErr = shared.ErrUnavailable
			delivery := NewFakeDelivery(env)
			delivery.RetryFnErr = shared.ErrNotSupported
			require.ErrorIs(t, runner.HandleDelivery(t.Context(), delivery), failure)
			assert.True(t, delivery.IsAcked())
			require.Len(t, hook.Settled(), 1)
			assert.ErrorIs(t, hook.Settled()[0].Err, failure)
			require.Len(t, rec.FindEntries(shared.MetricMessagesDropped), 1)
			assert.Contains(t, rec.FindEntries(shared.MetricMessagesDropped)[0].Tags,
				shared.Tag{Key: shared.TagKeyReason, Value: "retry_unsupported_dlq_failed"})
			assert.Empty(t, rec.FindEntries(shared.MetricDLQEntries))
			assert.Empty(t, rec.FindEntries(shared.MetricMessagesExpired))
			assert.Empty(t, rec.FindEntries(shared.MetricMessagesFiltered))
			assert.Zero(t, runner.InFlight())
		})
	}
}

// TestHandleProcessorError_RetryUnsupported_FallsToDLQ validates that when
// a processor returns a recoverable error and del.Retry returns
// ErrNotSupported, the message is routed to DLQ.
//
// Data flow:
// ───────────────────────────────────────────────────────────────
//
//	Processor → ErrThrottled → del.Retry → ErrNotSupported
//	         → DLQ.Route(reason=ErrThrottled) → ✓
//	         → del.Ack → ✓
//
// ───────────────────────────────────────────────────────────────
//
// Assertions:
//   - Delivery is acked
//   - DLQ contains 1 entry
func TestHandleProcessorError_RetryUnsupported_FallsToDLQ(t *testing.T) {
	receiver, _, dlqStore, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
		cfg.Processors = []ports.Processor{
			&FakeProcessor{
				NameVal:    "throttle-sim",
				ProcessErr: shared.ErrThrottled,
			},
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(func() *messaging.Envelope {
		e := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-proc-retry-unsup", Payload: []byte("data")})
		_ = e.SetExpiry(time.Now().Add(time.Hour))
		return e
	}())
	del.RetryFnErr = shared.ErrNotSupported

	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "delivery acked via DLQ fallback", del.IsAcked)

	if dlqStore.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", dlqStore.Count())
	}
}

// TestSharedOutbox_RetryUnsupported_FallsToDLQ validates that when the
// outbox persist fails and del.Retry returns ErrNotSupported, the message
// is routed to DLQ instead of propagating a permanent error.
//
// Data flow:
// ───────────────────────────────────────────────────────────────
//
//	Outbox.Persist → error → del.Retry → ErrNotSupported
//	               → DLQ.Route(reason=persistErr) → ✓
//	               → del.Ack → ✓
//
// ───────────────────────────────────────────────────────────────
//
// Assertions:
//   - Delivery is acked
//   - DLQ contains 1 entry
func TestSharedOutbox_RetryUnsupported_FallsToDLQ(t *testing.T) {
	receiver, _, dlqStore, outbox, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliverySharedOutbox
	})
	outbox.PersistErr = errors.New("outbox persist failed")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(func() *messaging.Envelope {
		e := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-outbox-retry-unsup", Payload: []byte("data")})
		_ = e.SetExpiry(time.Now().Add(time.Hour))
		return e
	}())
	del.RetryFnErr = shared.ErrNotSupported

	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "delivery acked via DLQ fallback", del.IsAcked)

	if dlqStore.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", dlqStore.Count())
	}
}

// TestHandleExpired_RetryUnsupported_FallsToDLQ validates that when an
// expired message's initial DLQ write fails and del.Retry returns
// ErrNotSupported, the retryOrFallback DLQ write succeeds as a second
// attempt.
//
// Data flow:
// ───────────────────────────────────────────────────────────────
//
//	IsExpired → DLQ.Route → ✗ (first write fails)
//	         → del.Retry → ErrNotSupported
//	         → DLQ.Route → ✓ (second write succeeds)
//	         → del.Ack → ✓
//
// ───────────────────────────────────────────────────────────────
//
// Assertions:
//   - Delivery is acked
//   - DLQ contains 1 entry (from the fallback write)
func TestHandleExpired_RetryUnsupported_FallsToDLQ(t *testing.T) {
	failOnceDLQ := NewFailOnceDLQStore()
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:  "route-expired-retry",
		Policy:   routing.RoutePolicy{OnExpired: routing.ExpiredDLQ}.WithDefaults(),
		Receiver: receiver,
		Sender:   sender,
		DLQ:      dlq.New(failOnceDLQ),
		Bindings: []routing.DestinationBinding{{ID: "b1"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(func() *messaging.Envelope {
		e := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-expired-retry-unsup", Payload: []byte("data")})
		_ = e.SetExpiry(time.Now().Add(-time.Hour))
		return e
	}())
	del.RetryFnErr = shared.ErrNotSupported

	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "delivery acked via DLQ fallback", del.IsAcked)

	if failOnceDLQ.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry from fallback write, got %d", failOnceDLQ.Count())
	}
}

// TestHandleResolveError_RetryUnsupported_FallsToDLQ validates that when
// the resolver returns a transient error and del.Retry returns
// ErrNotSupported, the message is routed to DLQ.
//
// Data flow:
// ───────────────────────────────────────────────────────────────
//
//	Resolver → ErrUnavailable (transient)
//	        → del.Retry → ErrNotSupported
//	        → DLQ.Route(reason=ErrUnavailable) → ✓
//	        → del.Ack → ✓
//
// ───────────────────────────────────────────────────────────────
//
// Assertions:
//   - Delivery is acked
//   - DLQ contains 1 entry
func TestHandleResolveError_RetryUnsupported_FallsToDLQ(t *testing.T) {
	receiver, _, dlqStore, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
		cfg.Resolver = &FakeResolver{ResolveErr: shared.ErrUnavailable}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(func() *messaging.Envelope {
		e := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-resolve-retry-unsup", Payload: []byte("data")})
		_ = e.SetExpiry(time.Now().Add(time.Hour))
		return e
	}())
	del.RetryFnErr = shared.ErrNotSupported

	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "delivery acked via DLQ fallback", del.IsAcked)

	if dlqStore.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", dlqStore.Count())
	}
}

// TestDirectHold_RetrySupported_NoFallback validates that when del.Retry
// succeeds (returns nil), no DLQ fallback occurs — confirming no
// regression for transports that support native retry.
//
// Data flow:
// ───────────────────────────────────────────────────────────────
//
//	Sender → ErrUnavailable → del.Retry → nil (supported)
//	      → delivery retried, NOT acked
//
// ───────────────────────────────────────────────────────────────
//
// Assertions:
//   - Delivery is retried
//   - Delivery is NOT acked
//   - DLQ has no entries
func TestDirectHold_RetrySupported_NoFallback(t *testing.T) {
	rec := &ports.RecordingExporter{}
	hook := &recordingHook{}
	_, sender, dlqStore, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
		// One send per delivery: this pins the replay decision, not the
		// in-process send retry that precedes it.
		cfg.Policy.SendRetryBudget = routing.SendRetryBudgetDisabled
		cfg.Clock = clocktest.NewAt(time.Unix(1000, 0))
		cfg.Metrics = rec
		cfg.Hook = hook
	})
	sender.SendErr = shared.ErrUnavailable
	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "msg-retry-supported", Payload: []byte("data"),
	}))
	require.NoError(t, runner.HandleDelivery(t.Context(), del))
	assert.True(t, del.IsRetried())
	assert.False(t, del.IsAcked())
	assert.Zero(t, dlqStore.Count())
	assert.Empty(t, hook.Settled())
	assert.Empty(t, rec.FindEntries(shared.MetricMessagesDropped))
	assert.Empty(t, rec.FindEntries(shared.MetricDLQEntries))
	assert.Zero(t, runner.InFlight())
}
