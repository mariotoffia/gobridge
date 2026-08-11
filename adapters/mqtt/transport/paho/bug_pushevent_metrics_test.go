package paho

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG RES-011: MQTT Session pushEvent drops without metrics
//
// When the event channel is full and pushEvent drops an event (the final
// default case), no metric is emitted. The fix adds a counter for
// MetricMQTTEventDropped so operators can detect event loss.
// ═══════════════════════════════════════════════════════════════════════════

// TestBugRES011_PushEvent_DropOldest_EmitsMetric verifies the drop-oldest
// eviction — the COMMON back-pressure path when the event buffer is full —
// increments MetricMQTTEventDropped for the evicted event, while preserving the
// newest event and the buffer depth.
//
// NOTE: the original RES-011 test asserted this path emitted NO
// metric ("normal drop-oldest"), which was precisely the doc/code disagreement
// identified — the comment and operator guidance promise an alertable
// MetricMQTTEventDropped that the common eviction never incremented. Evicting
// the oldest undelivered event IS event loss, so it now meters exactly one drop.
func TestBugRES011_PushEvent_DropOldest_EmitsMetric(t *testing.T) {
	rec := &ports.RecordingExporter{}
	s := NewSession(
		SessionOptions{BrokerURLs: []string{"tcp://localhost:1883"}, ClientID: "test"},
		connectivity.SessionEphemeral,
		nil,
		rec,
	)

	// Fill the buffer to capacity.
	for i := 0; i < sessionEventsBuffer; i++ {
		s.pushEvent(ports.SessionConnected, nil)
	}

	// One more: this evicts the oldest event and re-inserts the new one.
	s.pushEvent(ports.SessionError, nil)

	// The evicted oldest event is a real loss and must be metered exactly once.
	entries := rec.FindEntries(MetricMQTTEventDropped)
	if len(entries) != 1 {
		t.Fatalf("expected 1 dropped-event metric for the evicted oldest, got %d", len(entries))
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
	if len(events) != sessionEventsBuffer {
		t.Fatalf("expected %d events, got %d", sessionEventsBuffer, len(events))
	}
	last := events[len(events)-1]
	if last.Type != ports.SessionError {
		t.Fatalf("expected last event SessionError, got %d", last.Type)
	}
}

// TestBugRES011_MetricConstant_Exists verifies MetricMQTTEventDropped
// is defined and usable.
func TestBugRES011_MetricConstant_Exists(t *testing.T) {
	if MetricMQTTEventDropped == "" {
		t.Fatal("MetricMQTTEventDropped should not be empty")
	}
}
