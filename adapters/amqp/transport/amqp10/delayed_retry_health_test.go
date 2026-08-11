package amqp10

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// TestDelivery_DelayedRetryDeferred_MetricAndWarn covers: a
// delayed retry (after > 0) deferred to broker scheduling increments
// MetricAMQP10DelayedRetryDeferred once per message and emits the Warn
// once per link (shared delayWarnOnce), while an immediate retry
// (after == 0) touches neither.
func TestDelivery_DelayedRetryDeferred_MetricAndWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	rec := &ports.RecordingExporter{}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "gn2"})
	// A single shared once models two messages arriving on the same
	// receiver link (receiverLink.Receive hands every Delivery the link's
	// guard).
	linkGuard := &sync.Once{}
	newDel := func(s settler) *Delivery {
		d := NewDelivery(env, &amqp.Message{}, s, logger, rec, nil)
		d.delayWarnOnce = linkGuard
		return d
	}

	if err := newDel(newMockSettler()).Retry(context.Background(), 5*time.Second, nil); err != nil {
		t.Fatalf("Retry(5s) #1 error = %v", err)
	}
	if err := newDel(newMockSettler()).Retry(context.Background(), 5*time.Second, nil); err != nil {
		t.Fatalf("Retry(5s) #2 error = %v", err)
	}

	// Counter fires once per deferred delayed retry (per message).
	got := rec.FindEntries(MetricAMQP10DelayedRetryDeferred)
	if len(got) != 2 {
		t.Fatalf("counter emitted %d times, want 2 (one per message)", len(got))
	}
	if got[0].IValue != 1 {
		t.Fatalf("counter value = %d, want 1", got[0].IValue)
	}

	// Warn fires once per link, not once per message.
	if n := strings.Count(buf.String(), "delayed retry deferred to broker scheduling"); n != 1 {
		t.Fatalf("warn emitted %d times, want 1 (once per link)", n)
	}

	// An immediate retry (after == 0) must not touch the deferred counter.
	rec.Reset()
	if err := newDel(newMockSettler()).Retry(context.Background(), 0, nil); err != nil {
		t.Fatalf("Retry(0) error = %v", err)
	}
	if n := len(rec.FindEntries(MetricAMQP10DelayedRetryDeferred)); n != 0 {
		t.Fatalf("immediate retry emitted counter %d times, want 0", n)
	}
}

// TestSession_Health_ActiveClampedToRegistered covers +: with a
// plan wanting 2 subscriptions but only 1 registered (link-up) receiver,
// Health reports the link-derived active count (1), not the plan count
// (2). Because active (1) < wanted (2) the session is Degraded, NOT Full
// (this supersedes the earlier assertion that kept Full while clamping
// active): reporting Full while a planned subscription has no receiver
// hides the missing route from readiness.
func TestSession_Health_ActiveClampedToRegistered(t *testing.T) {
	s := newTestSession()
	s.mu.Lock()
	s.conn = &mockConn{}
	s.connected = true
	s.plan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "a"}, {Topic: "b"}},
	}
	s.mu.Unlock()

	r := &Receiver{} // pointer used only as a health-tracking map key
	s.registerReceiver(r)
	s.markReceiverLink(r, true)

	h := s.Health(context.Background())
	if h.SubscriptionsWanted != 2 {
		t.Fatalf("SubscriptionsWanted = %d, want 2", h.SubscriptionsWanted)
	}
	if h.SubscriptionsActive != 1 {
		t.Fatalf("SubscriptionsActive = %d, want 1 (clamped to registered up receivers)", h.SubscriptionsActive)
	}
	if h.ServiceLevel != ports.ServiceLevelDegraded {
		t.Fatalf("ServiceLevel = %q, want %q (active < wanted)", h.ServiceLevel, ports.ServiceLevelDegraded)
	}
}
