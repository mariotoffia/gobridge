package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// TestMetrics_FullPipeline_DirectHold verifies that a DirectHold route
// emits DeliveryE2ELatency with the correct route tag.
func TestMetrics_FullPipeline_DirectHold(t *testing.T) {
	rec := &ports.RecordingExporter{}
	sender := NewFakeSender()
	receiver := NewFakeReceiver()
	sess := NewFakeSession()

	rt := runtime.New(
		runtime.WithInstanceID("metrics-test-instance"),
		runtime.WithMetrics(rec),
		runtime.WithDLQStore(NewFakeDLQStore()),
	)

	cfg := runtime.RouteConfig{
		ID: "direct-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			DispatchMode: routing.DispatchSingle,
		},
		Bindings:           []routing.DestinationBinding{{ID: "b1"}},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
	}

	err := rt.AddRoute(cfg, receiver, sender, sess, &session.Config{
		SessionID: "s1",
		Exclusive: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "full-msg-1", Payload: []byte("payload"), ExpiresAt: time.Now().Add(time.Hour)})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "delivery e2e metric recorded", func() bool {
		return len(rec.FindEntries(shared.MetricDeliveryE2ELatency)) > 0
	})
	cancel()
	_ = rt.Stop(context.Background())

	e2e := rec.FindEntries(shared.MetricDeliveryE2ELatency)
	if len(e2e) == 0 {
		t.Fatal("expected DeliveryE2ELatency metric")
	}
	assertTag(t, e2e[0].Tags, shared.TagKeyRouteID, "direct-route")
}

// TestMetrics_FullPipeline_SharedOutbox verifies the full SharedOutbox
// path emits outbox and delivery metrics.
func TestMetrics_FullPipeline_SharedOutbox(t *testing.T) {
	rec := &ports.RecordingExporter{}
	sender := NewFakeSender()
	receiver := NewFakeReceiver()
	sess := NewFakeSession()
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()

	rt := runtime.New(
		runtime.WithInstanceID("metrics-outbox-test"),
		runtime.WithMetrics(rec),
		runtime.WithOutboxStore(outbox),
		runtime.WithLeaseStore(lease),
		runtime.WithDLQStore(NewFakeDLQStore()),
	)

	cfg := runtime.RouteConfig{
		ID: "outbox-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
			DispatchMode: routing.DispatchSingle,
		},
		Bindings: []routing.DestinationBinding{{ID: "b1", SessionID: "s1"}},
	}

	err := rt.AddRoute(cfg, receiver, sender, sess, &session.Config{
		SessionID:      "s1",
		Exclusive:      true,
		DrainStrategy:  persistence.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize: 10,
		LeaseTTL:       30 * time.Second,
		RenewInterval:  100 * time.Millisecond,
		RenewJitter:    0,
		MaxRenewFails:  3,
		StepDownGrace:  50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "outbox-msg-1", Payload: []byte("payload"), ExpiresAt: time.Now().Add(time.Hour)})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	waitFor(t, 3*time.Second, "sender received message", func() bool {
		return sender.SentCount() >= 1
	})

	// Wait for at least one lease renewal to complete.
	waitFor(t, 3*time.Second, "lease renew latency metric", func() bool {
		return len(rec.FindEntries(shared.MetricLeaseRenewLatency)) > 0
	})

	cancel()
	_ = rt.Stop(context.Background())

	e2e := rec.FindEntries(shared.MetricDeliveryE2ELatency)
	if len(e2e) == 0 {
		t.Error("expected DeliveryE2ELatency metric")
	}

	acquireLatency := rec.FindEntries(shared.MetricLeaseAcquireLatency)
	if len(acquireLatency) == 0 {
		t.Error("expected LeaseAcquireLatency metric")
	}

	drainLatency := rec.FindEntries(shared.MetricOutboxDrainLatency)
	if len(drainLatency) == 0 {
		t.Error("expected OutboxDrainLatency metric")
	}

	completions := rec.FindEntries(shared.MetricOutboxCompletions)
	if len(completions) == 0 {
		t.Error("expected OutboxCompletions metric")
	}
}

// Metric name constants are non-empty and collision-free — that is
// shared.TestMetricConstants_NonEmpty's job, in the package that
// declares them and against every constant rather than a subset. A
// stale 17-name copy lived here and asserted `len(all) != 17`, i.e.
// that the literal above it had the length it was written with.

func assertTag(t *testing.T, tags []shared.Tag, key, wantValue string) {
	t.Helper()
	for _, tag := range tags {
		if tag.Key == key {
			if tag.Value != wantValue {
				t.Errorf("tag %s = %q, want %q", key, tag.Value, wantValue)
			}
			return
		}
	}
	t.Errorf("tag %s not found", key)
}
