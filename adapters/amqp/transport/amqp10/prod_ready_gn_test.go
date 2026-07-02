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

// TestDelivery_DelayedRetryUnhonored_MetricAndWarn covers G-N2: an
// unhonored delayed retry (after > 0) increments
// MetricAMQP10DelayedRetryUnhonored once per message and emits the Warn
// once per link (shared delayWarnOnce), while an immediate retry
// (after == 0) touches neither.
func TestDelivery_DelayedRetryUnhonored_MetricAndWarn(t *testing.T) {
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

	// Counter fires once per unhonored delayed retry (per message).
	got := rec.FindEntries(MetricAMQP10DelayedRetryUnhonored)
	if len(got) != 2 {
		t.Fatalf("counter emitted %d times, want 2 (one per message)", len(got))
	}
	if got[0].IValue != 1 {
		t.Fatalf("counter value = %d, want 1", got[0].IValue)
	}

	// Warn fires once per link, not once per message.
	if n := strings.Count(buf.String(), "delayed retry not honored"); n != 1 {
		t.Fatalf("warn emitted %d times, want 1 (once per link)", n)
	}

	// An immediate retry (after == 0) must not touch the unhonored counter.
	rec.Reset()
	if err := newDel(newMockSettler()).Retry(context.Background(), 0, nil); err != nil {
		t.Fatalf("Retry(0) error = %v", err)
	}
	if n := len(rec.FindEntries(MetricAMQP10DelayedRetryUnhonored)); n != 0 {
		t.Fatalf("immediate retry emitted counter %d times, want 0", n)
	}
}

// TestSession_Health_ActiveClampedToRegistered covers G-N3: with a plan
// wanting 2 subscriptions but only 1 registered (link-up) receiver,
// Health reports the link-derived active count (1), not the plan count
// (2). Full is still reported because no registered receiver's link is
// down; only the over-reported active count is corrected.
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
	if h.ServiceLevel != ports.ServiceLevelFull {
		t.Fatalf("ServiceLevel = %q, want %q", h.ServiceLevel, ports.ServiceLevelFull)
	}
}
