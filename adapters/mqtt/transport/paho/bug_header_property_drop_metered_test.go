package paho

import (
	"strings"
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// (LOW): oversized/unsafe MQTT v5 properties (CorrelationData, ContentType,
// ResponseTopic) were dropped SILENTLY — only user-property drops incremented
// MetricMQTTIngressHeaderDropped, so a correlation-id loss was invisible. Every
// header dropped by the safety filter (property or user property) must feed the
// same metric.
//
// Mutation killed: remove any of the three `else { droppedHeaders++ }` branches
// on the property filters → the recorded drop count falls below 3 and this test
// fails.
// ═══════════════════════════════════════════════════════════════════════════
func TestBug_EnvelopeFromPublish_MetersDroppedProperties(t *testing.T) {
	rec := &ports.RecordingExporter{}
	big := strings.Repeat("a", maxHeaderValueLen+1)

	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			CorrelationData: []byte(big),
			ContentType:     big,
			ResponseTopic:   big,
		},
	}
	env := EnvelopeFromPublish(pub, nil, rec)

	// None of the oversized properties survive the filter.
	require.NotContains(t, env.Headers(), messaging.HeaderCorrelationID)
	require.NotContains(t, env.Headers(), messaging.HeaderContentType)
	require.NotContains(t, env.Headers(), headerMQTTResponseTopic)

	// all three property drops are metered on the ingress-header counter.
	entries := rec.FindEntries(MetricMQTTIngressHeaderDropped)
	require.Len(t, entries, 1, "the drop counter is emitted once with the total")
	require.Equal(t, int64(3), entries[0].IValue,
		"each dropped MQTT property (correlation data, content type, response topic) must be counted")
}
