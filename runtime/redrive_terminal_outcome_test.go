package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// A DLQ redrive is an operator action, not a broker redelivery loop. Two rules
// follow from that and are pinned here:
//
//  1. The redriven message must not inherit the ADAPTER-GENERATED identity
//     marker of the original. That marker means "the source supplies no stable
//     identity, so the replay ledger cannot count this message" and makes the
//     runtime sink the message terminally on its FIRST transient failure. A
//     redrive carries a fresh, bridge-minted identity, so it is countable and
//     must get its normal retry budget.
//
//  2. When the redriven delivery is nevertheless settled terminally without
//     being delivered — dropped under on_permanent_failure=drop, or written
//     back to the DLQ — the caller must learn about it. The synthetic delivery
//     used for an inject always ACKs successfully, so without this the admin
//     redrive path reads "delivered", deletes the original DLQ entry, and both
//     the message and its failure evidence are gone.

// newDirectHoldRedriveRuntime starts a runtime with one direct_hold route whose
// sender is controlled by the caller. on_permanent_failure=drop is the loss-prone
// policy from the issue: nothing is retained when a delivery settles terminally.
func newDirectHoldRedriveRuntime(t *testing.T, sender *FakeSender) *goruntime.Runtime {
	t.Helper()

	rt := newTestRuntime("bridge-redrive-terminal", nil, nil, NewFakeDLQStore())

	cfg := goruntime.RouteConfig{
		ID: "direct-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			MaxReplayAttempts:  3,
			SendTimeout:        time.Second,
			OnPermanentFailure: routing.FailureDrop,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "binding-a", Address: "devices/a/state"},
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	if err := rt.AddRoute(cfg, NewFakeReceiver(), sender, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	return rt
}

// dlqEntryEnvelope builds the envelope shape a DLQ entry holds for a message
// whose source minted the identity (the common MQTT publish): the reserved
// generated-identity marker survives the DLQ JSON round trip.
func dlqEntryEnvelope(t *testing.T, id string) *messaging.Envelope {
	t.Helper()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      id,
		Subject: "device.state.update",
		Payload: []byte("hello"),
	})
	env.SetHeader(messaging.HeaderGeneratedID, "true")
	return env
}

// TestInjectRedrive_DroppedRedrive_ReportsFailure pins rule 2: a redrive whose
// send fails and is then dropped by policy must be reported as a FAILURE, so the
// admin redrive path keeps the DLQ entry instead of deleting the only remaining
// evidence of the message.
//
// Mutation check: stop surfacing the terminal outcome on the synthetic delivery
// and this fails — InjectRedrive returns nil for a message that was discarded.
func TestInjectRedrive_DroppedRedrive_ReportsFailure(t *testing.T) {
	sender := NewFakeSender()
	sender.SendErr = shared.ErrInvalidPayload // permanent -> dropped under FailureDrop
	rt := newDirectHoldRedriveRuntime(t, sender)

	err := rt.InjectRedrive(context.Background(), "direct-route", "binding-a",
		dlqEntryEnvelope(t, "orig-dropped"))
	if err == nil {
		t.Fatal("InjectRedrive reported success for a redrive the route DROPPED; " +
			"the admin path would delete the DLQ entry and lose the message")
	}
	if sender.SentCount() != 0 {
		t.Fatalf("sender recorded %d successful sends, want 0", sender.SentCount())
	}
}

// TestInjectRedrive_StripsGeneratedIdentityMarker pins rule 1: the redriven
// message must not inherit the original's adapter-generated identity marker,
// because a redrive is issued by an operator under a fresh bridge-minted ID.
// Keeping the marker makes the message UNCOUNTABLE, which sinks it terminally on
// the first transient failure instead of retrying it.
//
// Mutation check: stop deleting the marker in InjectRedrive and this fails — the
// first transient send failure settles terminally instead of retrying.
func TestInjectRedrive_StripsGeneratedIdentityMarker(t *testing.T) {
	sender := NewFakeSender()
	// TRANSIENT failure: a countable message retries (the synthetic delivery
	// cannot retry, so the redrive is reported as failed but is NOT dropped);
	// an uncountable one is poisoned and DROPPED on the first attempt.
	sender.SendErr = shared.ErrUnavailable
	rt := newDirectHoldRedriveRuntime(t, sender)

	var seen map[string]any
	sender.SendFn = func(env *messaging.Envelope) error {
		seen = env.HeadersSnapshot()
		return shared.ErrUnavailable
	}

	_ = rt.InjectRedrive(context.Background(), "direct-route", "binding-a",
		dlqEntryEnvelope(t, "orig-marker"))

	if seen == nil {
		t.Fatal("the redriven message never reached the sender")
	}
	if _, ok := seen[messaging.HeaderGeneratedID]; ok {
		t.Fatalf("the redriven message inherited %s; a redrive is operator-issued, "+
			"so its identity is bridge-minted and countable", messaging.HeaderGeneratedID)
	}
}

// TestInjectRedrive_StripsSourceRedeliveryCount pins the count-bearing half of
// rule 1. A DLQ entry written for a message that exhausted its replay cap still
// carries the SOURCE transport's redelivery counter (sqs.ApproximateReceiveCount
// and its siblings) — that counter is what exhausted the cap. The route reads a
// native counter in PREFERENCE to its own ledger, so a redrive that inherited it
// would arrive already over the cap and be poisoned on its first attempt: the
// operator's redrive would be a no-op dressed up as a retry.
//
// A redrive is a fresh, operator-issued delivery attempt, so it must carry no
// redelivery history at all.
//
// Mutation check: stop stripping the source redelivery counters in InjectRedrive
// and this fails — the redrive is poisoned (dropped) on its first send failure
// instead of being retried.
func TestInjectRedrive_StripsSourceRedeliveryCount(t *testing.T) {
	sender := NewFakeSender()
	sender.SendErr = shared.ErrUnavailable // TRANSIENT: a countable message retries
	dlqStore := NewFakeDLQStore()
	metrics := &ports.RecordingExporter{}

	rt := newTestRuntime("bridge-redrive-count", nil, nil, dlqStore,
		goruntime.WithMetrics(metrics))
	cfg := goruntime.RouteConfig{
		ID: "count-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliveryDirectHold,
			MaxReplayAttempts: 3,
			SendTimeout:       time.Second,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "binding-a", Address: "devices/a/state"},
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	if err := rt.AddRoute(cfg, NewFakeReceiver(), sender, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	// The DLQ'd envelope as a real store holds it: the source counter that
	// exhausted the cap is still on it.
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "orig-counted",
		Subject: "device.state.update",
		Payload: []byte("hello"),
		Headers: map[string]any{"sqs.ApproximateReceiveCount": 5},
	})

	_ = rt.InjectRedrive(context.Background(), "count-route", "binding-a", env)

	// A redrive that still looked over the cap is POISONED on its first failure
	// (category max_retries); one that got its budget back is RETRIED, and only
	// because a synthetic delivery cannot retry does it fall back to the DLQ
	// (category retry_unsupported).
	var categories []string
	for _, e := range metrics.FindEntries(shared.MetricDLQEntries) {
		for _, tag := range e.Tags {
			if tag.Key == shared.TagKeyCategory {
				categories = append(categories, tag.Value)
			}
		}
	}
	if len(categories) != 1 {
		t.Fatalf("DLQEntries categories = %v, want exactly one", categories)
	}
	if categories[0] != "retry_unsupported" {
		t.Fatalf("DLQ category = %q, want %q: the redrive inherited the source "+
			"redelivery count and was poisoned instead of retried", categories[0], "retry_unsupported")
	}
	if len(dlqStore.Entries) != 1 {
		t.Fatalf("DLQ entries = %d, want 1", len(dlqStore.Entries))
	}
}

// TestInject_SuccessfulDelivery_ReportsSuccess pins the scope of rule 2: an
// inject whose delivery actually lands still reports success. The terminal
// outcome must only surface a delivery that was DISCARDED.
//
// Mutation check: report every settle as a failure and this fails.
func TestInject_SuccessfulDelivery_ReportsSuccess(t *testing.T) {
	sender := NewFakeSender()
	rt := newDirectHoldRedriveRuntime(t, sender)

	if err := rt.Inject(context.Background(), "direct-route",
		messaging.MustEnvelope(messaging.EnvelopeInput{ID: "ok-1", Payload: []byte("p")})); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if sender.SentCount() != 1 {
		t.Fatalf("sends = %d, want 1", sender.SentCount())
	}
}
