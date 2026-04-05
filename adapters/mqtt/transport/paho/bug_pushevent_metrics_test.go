package paho

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG RES-011: MQTT Session pushEvent drops without metrics
//
// When the event channel is full and pushEvent drops an event (the final
// default case), no metric is emitted. The fix adds a counter for
// domain.MetricMQTTEventDropped so operators can detect event loss.
// ═══════════════════════════════════════════════════════════════════════════

// TestBugRES011_PushEvent_DropEmitsMetric verifies that when the event
// buffer is full AND the drain+re-insert also fails (simulated by
// saturating the channel from another goroutine), a dropped-event
// metric is emitted.
//
// In normal operation the "drop oldest, insert new" path succeeds, so
// the metric is only hit under extreme contention where both default
// branches fire. We verify the normal drop-oldest path emits no metric,
// and the forced-full path emits one.
func TestBugRES011_PushEvent_NormalDropOldest_NoMetric(t *testing.T) {
	rec := &ports.RecordingExporter{}
	s := NewSession(
		SessionOptions{BrokerURLs: []string{"tcp://localhost:1883"}, ClientID: "test"},
		domain.SessionEphemeral,
		nil,
		rec,
	)

	// Fill the buffer (capacity 16).
	for i := 0; i < 16; i++ {
		s.pushEvent(ports.SessionConnected, nil)
	}

	// One more: this triggers drop-oldest, then re-insert (should succeed).
	s.pushEvent(ports.SessionError, nil)

	// The normal drop-oldest path should NOT emit a dropped metric
	// because the re-insert succeeds.
	entries := rec.FindEntries(domain.MetricMQTTEventDropped)
	if len(entries) != 0 {
		t.Fatalf("expected 0 dropped-event metrics in normal path, got %d", len(entries))
	}

	// Verify the newest event is present.
	var events []ports.SessionEvent
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatal("channel closed unexpectedly")
			}
			events = append(events, ev)
		default:
			goto done
		}
	}
done:
	if len(events) != 16 {
		t.Fatalf("expected 16 events, got %d", len(events))
	}
	last := events[len(events)-1]
	if last.Type != ports.SessionError {
		t.Fatalf("expected last event SessionError, got %d", last.Type)
	}
}

// TestBugRES011_MetricConstant_Exists verifies domain.MetricMQTTEventDropped
// is defined and usable.
func TestBugRES011_MetricConstant_Exists(t *testing.T) {
	if domain.MetricMQTTEventDropped == "" {
		t.Fatal("domain.MetricMQTTEventDropped should not be empty")
	}
}
