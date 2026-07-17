package paho

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// B-2 / D-2: pushEvent's drop-oldest eviction is the COMMON back-pressure path,
// yet it used to drop the evicted event silently — only the (unreachable under
// s.mu) double-failure incremented MetricMQTTEventDropped, contradicting the
// comment that claimed the eviction was metered. Every actually-lost event must
// increment the metric exactly once.
//
// Mutation killed: remove the `s.metrics.Counter(MetricMQTTEventDropped, 1)`
// from the drain (`case <-s.events:`) branch → the eviction is silent again and
// this test sees zero drop entries.
// ═══════════════════════════════════════════════════════════════════════════
func TestBug_PushEvent_MetersEvictedOldest(t *testing.T) {
	rec := &ports.RecordingExporter{}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "evict-meter",
	}, connectivity.SessionEphemeral, nil, rec)

	// Fill the events buffer to capacity WITHOUT draining it.
	for range sessionEventsBuffer {
		s.pushEvent(ports.SessionReconnecting, nil)
	}
	require.Empty(t, rec.FindEntries(MetricMQTTEventDropped),
		"no event is dropped while the buffer still has room")

	// One more push must evict the oldest buffered event and meter that drop.
	s.pushEvent(ports.SessionReconnecting, nil)

	require.Len(t, rec.FindEntries(MetricMQTTEventDropped), 1,
		"B-2: evicting the oldest event to make room must increment MetricMQTTEventDropped")

	// A second overflowing push evicts again — the counter tracks every loss.
	s.pushEvent(ports.SessionReconnecting, nil)
	require.Len(t, rec.FindEntries(MetricMQTTEventDropped), 2,
		"B-2: each evicted event increments the drop counter")
}
