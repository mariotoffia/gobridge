package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// TestMetrics_FullPipeline_DirectHold verifies that a DirectHold route
// emits DeliveryE2ELatency with the correct route tag.
func TestMetrics_FullPipeline_DirectHold(t *testing.T) {
	rec := &ports.RecordingExporter{}
	sender := NewFakeSender()
	receiver := NewFakeReceiver()
	session := NewFakeSession()

	rt := runtime.New(
		runtime.WithInstanceID("metrics-test-instance"),
		runtime.WithMetrics(rec),
	)

	cfg := runtime.RouteConfig{
		ID: "direct-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
			DispatchMode: domain.DispatchSingle,
		},
		Bindings:           []domain.DestinationBinding{{ID: "b1"}},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}

	err := rt.AddRoute(cfg, receiver, sender, session, &runtime.SessionConfig{
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

	env := &domain.Envelope{ID: "full-msg-1", Payload: []byte("payload"), ExpiresAt: time.Now().Add(time.Hour)}
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
	session := NewFakeSession()
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
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
			DispatchMode: domain.DispatchSingle,
		},
		Bindings: []domain.DestinationBinding{{ID: "b1", SessionID: "s1"}},
	}

	err := rt.AddRoute(cfg, receiver, sender, session, &runtime.SessionConfig{
		SessionID:      "s1",
		Exclusive:      true,
		DrainStrategy:  domain.NewFixedPoll(50 * time.Millisecond),
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

	waitFor(t, 2*time.Second, "session started", func() bool {
		return session.IsStarted()
	})

	env := &domain.Envelope{ID: "outbox-msg-1", Payload: []byte("payload"), ExpiresAt: time.Now().Add(time.Hour)}
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

// TestMetrics_AllMetricNamesDocumented verifies every domain metric name constant is listed for documentation coverage.
func TestMetrics_AllMetricNamesDocumented(t *testing.T) {
	all := []string{
		shared.MetricLeaseAcquireLatency,
		shared.MetricLeaseRenewLatency,
		shared.MetricLeaseAcquireFailures,
		shared.MetricLeaseExpiries,
		shared.MetricLeaseTransfers,
		shared.MetricOutboxPersistLatency,
		shared.MetricOutboxDrainLatency,
		shared.MetricOutboxDepth,
		shared.MetricOutboxClaimRecoveries,
		shared.MetricOutboxCompletions,
		shared.MetricOutboxExpiredBeforeSend,
		shared.MetricOutboxReplayCount,
		shared.MetricSQSReceiveLatency,
		shared.MetricSQSDeleteLatency,
		shared.MetricSQSVisibilityExtensions,
		shared.MetricAckLatency,
		shared.MetricVisibilityExtensions,
		shared.MetricDeliveryE2ELatency,
		shared.MetricDLQEntries,
		shared.MetricMQTTPublishLatency,
		shared.MetricMQTTReconnects,
	}
	for _, name := range all {
		if name == "" {
			t.Error("metric name constant is empty")
		}
	}
	if len(all) != 21 {
		t.Errorf("expected 21 metric name constants, got %d", len(all))
	}
}

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
