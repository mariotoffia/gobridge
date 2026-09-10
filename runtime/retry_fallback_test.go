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

// TestDirectHold_RetryUnsupported_DLQAlsoFails_ReturnsError validates that
// when both del.Retry (ErrNotSupported) and the DLQ fallback write fail,
// the delivery is neither acked nor successfully retried.
//
// Data flow:
// ───────────────────────────────────────────────────────────────
//
//	Sender → ErrUnavailable → del.Retry → ErrNotSupported
//	      → DLQ.Route → ✗ (WriteErr)
//	      → error returned
//
// ───────────────────────────────────────────────────────────────
//
// Assertions:
//   - Delivery is NOT acked
//   - DLQ has no entries
func TestDirectHold_RetryUnsupported_DLQAlsoFails_ReturnsError(t *testing.T) {
	receiver, sender, dlqStore, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
	})
	sender.SendErr = shared.ErrUnavailable
	dlqStore.WriteErr = errors.New("store down")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(func() *messaging.Envelope {
		e := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-both-fail", Payload: []byte("data")})
		_ = e.SetExpiry(time.Now().Add(time.Hour))
		return e
	}())
	del.RetryFnErr = shared.ErrNotSupported

	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "delivery retried", del.IsRetried)
	time.Sleep(50 * time.Millisecond) // NEGATIVE: verify delivery is not acked when both retry and DLQ fail

	if del.IsAcked() {
		t.Fatal("delivery should NOT be acked when both retry and DLQ fail")
	}
	if dlqStore.Count() != 0 {
		t.Fatalf("expected 0 DLQ entries (write failed), got %d", dlqStore.Count())
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
	receiver, sender, dlqStore, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
	})
	sender.SendErr = shared.ErrUnavailable

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(func() *messaging.Envelope {
		e := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-retry-supported", Payload: []byte("data")})
		_ = e.SetExpiry(time.Now().Add(time.Hour))
		return e
	}())
	// RetryFnErr defaults to nil — retry succeeds

	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "delivery retried", del.IsRetried)
	time.Sleep(50 * time.Millisecond) // NEGATIVE: verify delivery is not acked when retry succeeds (redelivery expected)

	if del.IsAcked() {
		t.Fatal("delivery should NOT be acked when retry succeeds (redelivery expected)")
	}
	if dlqStore.Count() != 0 {
		t.Fatalf("expected 0 DLQ entries, got %d", dlqStore.Count())
	}
}
