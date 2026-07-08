package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// ══════════════════════════════════════════════════════════════════════════
// Chunk 10 dispatch regressions (F1, F2, F3)
// ══════════════════════════════════════════════════════════════════════════

// newDLQFailRunner builds a direct_hold runner whose DLQ store ALWAYS fails to
// write (single attempt, no backoff), plus an always-timing-out processor. Used
// to drive the DLQ-write-failure redelivery path (F1). WriteMaxAttempts=1 keeps
// the failing write synchronous — no real backoff sleep between attempts.
func newDLQFailRunner(maxReplay int, dlqStore *FakeDLQStore) *route.RouteRunner {
	cfg := route.RouteRunnerConfig{
		RouteID: "dlq-fail-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliveryDirectHold,
			MaxReplayAttempts: maxReplay,
		}.WithDefaults(),
		Receiver:   NewFakeReceiver(),
		Sender:     NewFakeSender(),
		DLQ:        dlq.NewFromConfig(dlq.Config{Store: dlqStore, WriteMaxAttempts: 1}),
		Bindings:   []routing.DestinationBinding{{ID: "bind-1", Address: "topic"}},
		Processors: []ports.Processor{&timeoutProcessor{}},
	}
	return route.NewRouteRunnerFromConfig(cfg)
}

// TestPoisonDLQWriteFailure_RedeliversWithPolicyDelay is the F1 regression: when
// the poison path's DLQ write FAILS (store down), the delivery must be
// redelivered with the policy-computed backoff, NOT the old zero-delay
// (ChangeMessageVisibility(0)) that looped every poison message at broker
// round-trip speed across the whole backlog while the DLQ was already degraded.
func TestPoisonDLQWriteFailure_RedeliversWithPolicyDelay(t *testing.T) {
	dlqStore := NewFakeDLQStore()
	dlqStore.WriteErr = errors.New("dlq store down")
	runner := newDLQFailRunner(3, dlqStore)

	// Receive count at the cap → the delivery takes the poison→DLQ path, whose
	// write then fails.
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "poison-dlqfail",
		Subject: "test",
		Headers: map[string]any{"sqs.ApproximateReceiveCount": 3},
	})
	del := NewFakeDelivery(env)

	if err := runner.HandleDelivery(context.Background(), del); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	// The write failed: the source must NOT be acked (else the message is lost),
	// and it must be redelivered so a healthy DLQ later captures it.
	if del.IsAcked() {
		t.Fatal("delivery acked despite DLQ write failure; the poison message would be lost")
	}
	if !del.IsRetried() {
		t.Fatal("delivery not redelivered after DLQ write failure")
	}
	// F1 core assertion: the redelivery delay is the policy backoff, not 0. A
	// regression to the literal `0` reintroduces the broker-hammering hot loop.
	if del.RetryAfter <= 0 {
		t.Fatalf("retry delay = %v, want > 0 (policy backoff, not zero-delay redelivery)", del.RetryAfter)
	}
}

// newSharedOutboxRunner builds a shared_outbox runner with the given replay cap,
// resolver, and bindings, wired to the supplied outbox + DLQ stores.
func newSharedOutboxRunner(
	maxReplay int,
	outbox *FakeOutboxStore,
	dlqStore *FakeDLQStore,
	resolver *FakeResolver,
	bindings []routing.DestinationBinding,
) *route.RouteRunner {
	cfg := route.RouteRunnerConfig{
		RouteID: "shared-outbox-cap-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliverySharedOutbox,
			MaxReplayAttempts: maxReplay,
		}.WithDefaults(),
		Receiver:    NewFakeReceiver(),
		Sender:      NewFakeSender(),
		DLQ:         dlq.New(dlqStore),
		OutboxStore: outbox,
		Resolver:    resolver,
		Bindings:    bindings,
	}
	return route.NewRouteRunnerFromConfig(cfg)
}

// TestSharedOutboxPersistFailure_ReplayCap_Poisons is the F2 regression for the
// Persist-failure branch: a permanently-failing outbox Persist previously
// retried FOREVER (no replay-cap gate), pinning an in-flight slot every pass on
// sources without a native redrive cap. At/above MaxReplayAttempts it must now
// poison to the DLQ and settle; below the cap it must retry with the policy
// backoff (never a zero-delay loop).
func TestSharedOutboxPersistFailure_ReplayCap_Poisons(t *testing.T) {
	bindings := []routing.DestinationBinding{{ID: "bind-1", Address: "topic", SessionID: "sess-1"}}
	resolver := &FakeResolver{Plans: []routing.DispatchPlan{{BindingID: "bind-1", Address: "topic"}}}

	t.Run("at cap poisons to DLQ and settles", func(t *testing.T) {
		outbox := NewFakeOutboxStore()
		outbox.PersistErr = errors.New("outbox store down")
		dlqStore := NewFakeDLQStore()
		runner := newSharedOutboxRunner(3, outbox, dlqStore, resolver, bindings)

		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "outbox-persist-poison",
			Subject: "test",
			Headers: map[string]any{"sqs.ApproximateReceiveCount": 3},
		})
		del := NewFakeDelivery(env)

		if err := runner.HandleDelivery(context.Background(), del); err != nil {
			t.Fatalf("HandleDelivery: %v", err)
		}
		if got := dlqStore.Count(); got != 1 {
			t.Fatalf("expected 1 DLQ entry after replay cap, got %d", got)
		}
		if !del.IsAcked() {
			t.Fatal("expected terminal ack after poisoning")
		}
		if del.IsRetried() {
			t.Fatal("delivery retried; the replay cap must stop the infinite persist-retry loop")
		}
	})

	t.Run("below cap retries with policy backoff", func(t *testing.T) {
		outbox := NewFakeOutboxStore()
		outbox.PersistErr = errors.New("outbox store down")
		dlqStore := NewFakeDLQStore()
		runner := newSharedOutboxRunner(3, outbox, dlqStore, resolver, bindings)

		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "outbox-persist-retry",
			Subject: "test",
			Headers: map[string]any{"sqs.ApproximateReceiveCount": 1},
		})
		del := NewFakeDelivery(env)

		if err := runner.HandleDelivery(context.Background(), del); err != nil {
			t.Fatalf("HandleDelivery: %v", err)
		}
		if got := dlqStore.Count(); got != 0 {
			t.Fatalf("expected no DLQ entry below the cap, got %d", got)
		}
		if !del.IsRetried() {
			t.Fatal("expected retry below the replay cap")
		}
		if del.IsAcked() {
			t.Fatal("delivery acked below the cap; a persist failure must retry, not settle")
		}
		// F1 residual on this path: the persist-failure retry must carry the
		// policy backoff, not the old zero-delay redelivery.
		if del.RetryAfter <= 0 {
			t.Fatalf("retry delay = %v, want > 0 (policy backoff)", del.RetryAfter)
		}
	})
}

// TestSharedOutboxUnknownBinding_ReplayCap_Poisons is the F2 regression for the
// record-BUILD-failure branch: a resolver emitting a BindingID absent from the
// route's bindings makes buildOutboxRecords fail permanently (the record would
// orphan under a BINDING#<id> partition no drainer polls). Without the gate this
// retried forever; at/above the cap it must poison to the DLQ.
func TestSharedOutboxUnknownBinding_ReplayCap_Poisons(t *testing.T) {
	bindings := []routing.DestinationBinding{{ID: "bind-1", Address: "topic", SessionID: "sess-1"}}
	// Resolver emits a plan for a binding that does not exist on the route.
	resolver := &FakeResolver{Plans: []routing.DispatchPlan{{BindingID: "ghost", Address: "topic"}}}

	outbox := NewFakeOutboxStore()
	dlqStore := NewFakeDLQStore()
	runner := newSharedOutboxRunner(2, outbox, dlqStore, resolver, bindings)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "outbox-build-poison",
		Subject: "test",
		Headers: map[string]any{"sqs.ApproximateReceiveCount": 2},
	})
	del := NewFakeDelivery(env)

	if err := runner.HandleDelivery(context.Background(), del); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	if got := dlqStore.Count(); got != 1 {
		t.Fatalf("expected 1 DLQ entry after replay cap on unknown-binding build failure, got %d", got)
	}
	if !del.IsAcked() {
		t.Fatal("expected terminal ack after poisoning the unresolvable build")
	}
	if del.IsRetried() {
		t.Fatal("delivery retried; the build-failure replay cap must stop the infinite loop")
	}
	if got := outbox.RecordCount(); got != 0 {
		t.Fatalf("expected no outbox records for an unresolvable plan, got %d", got)
	}
}

// newForgedCountRunner builds a direct_hold runner declaring the given SOURCE
// transport, an always-timing-out (transient) processor, and MaxReplayAttempts=1.
// The ingress F3 strip decides whether a forged foreign count survives to
// receiveCount.
func newForgedCountRunner(sourceTransport string, dlqStore *FakeDLQStore) *route.RouteRunner {
	cfg := route.RouteRunnerConfig{
		RouteID:         "forged-count-route",
		SourceTransport: sourceTransport,
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliveryDirectHold,
			MaxReplayAttempts: 1,
		}.WithDefaults(),
		Receiver:   NewFakeReceiver(),
		Sender:     NewFakeSender(),
		DLQ:        dlq.New(dlqStore),
		Bindings:   []routing.DestinationBinding{{ID: "bind-1", Address: "topic"}},
		Processors: []ports.Processor{&timeoutProcessor{}},
	}
	return route.NewRouteRunnerFromConfig(cfg)
}

// TestForgedIngressReceiveCount_StrippedBeforeReplayCap is the F3 integration
// regression: an untrusted producer on a count-less source (MQTT) forges
// sqs.ApproximateReceiveCount: 999. The ingress strip must delete it BEFORE
// receiveCount is read, so the FIRST delivery is treated as a first delivery
// (count 0, below the cap) and a genuine transient error still gets its retry —
// instead of the forged count poison-routing a healthy message to the DLQ
// without a single genuine attempt. A native SQS source (the transport that
// legitimately stamps the key) keeps it, proving the strip is per-transport.
func TestForgedIngressReceiveCount_StrippedBeforeReplayCap(t *testing.T) {
	forged := map[string]any{"sqs.ApproximateReceiveCount": 999}

	t.Run("count-less MQTT source strips the forged count and retries", func(t *testing.T) {
		dlqStore := NewFakeDLQStore()
		runner := newForgedCountRunner("mqtt", dlqStore)

		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "forged-mqtt",
			Subject: "test",
			Headers: forged,
		})
		del := NewFakeDelivery(env)

		if err := runner.HandleDelivery(context.Background(), del); err != nil {
			t.Fatalf("HandleDelivery: %v", err)
		}
		if got := dlqStore.Count(); got != 0 {
			t.Fatalf("forged count was not stripped: message poisoned to DLQ (count=%d) on first delivery", got)
		}
		if !del.IsRetried() {
			t.Fatal("expected retry: the forged count must be stripped so the first delivery is below the cap")
		}
		if del.IsAcked() {
			t.Fatal("delivery settled; a first-delivery transient error must retry, not poison")
		}
	})

	t.Run("native SQS source keeps its own count and poisons at the cap", func(t *testing.T) {
		dlqStore := NewFakeDLQStore()
		runner := newForgedCountRunner("sqs", dlqStore)

		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "genuine-sqs",
			Subject: "test",
			Headers: forged, // for a real SQS source this is the broker's own count
		})
		del := NewFakeDelivery(env)

		if err := runner.HandleDelivery(context.Background(), del); err != nil {
			t.Fatalf("HandleDelivery: %v", err)
		}
		if got := dlqStore.Count(); got != 1 {
			t.Fatalf("expected the native SQS count (999 >= cap) to poison to DLQ, got count=%d", got)
		}
		if !del.IsAcked() {
			t.Fatal("expected terminal ack after poisoning a genuinely over-cap SQS delivery")
		}
	})
}
