package bridge

import (
	"testing"

	"github.com/mariotoffia/gobridge/ports"

	"github.com/stretchr/testify/require"
)

// TestOutboxRuntimeOptions_ThreadsMetricsExporter guards the production wiring
// for shared.MetricOutboxClaimConflicts: outboxRuntimeOptions() MUST thread the
// builder's exporter into OutboxRuntimeOptions.Metrics so a config-driven
// DynamoDB outbox store actually emits the counter (rather than defaulting to a
// no-op meter and dropping it into the void). It also asserts the absence path:
// with no exporter configured, Metrics stays nil so the factory substitutes a
// no-op instead of dereferencing it.
func TestOutboxRuntimeOptions_ThreadsMetricsExporter(t *testing.T) {
	rec := &ports.RecordingExporter{}
	cfg := directHoldConfig()
	b := NewBuilder(cfg, WithMetrics(rec))

	// Derived path: store config present, no explicit stale_claim_duration.
	derived, err := b.outboxRuntimeOptions(&ports.StoreConfig{Type: "dynamodb"})
	require.NoError(t, err)
	if derived.Metrics != ports.MetricsExporter(rec) {
		t.Fatalf("derived path must thread the builder's exporter, got %v", derived.Metrics)
	}

	// nil store config path (early return) still threads the exporter.
	nilSC, err := b.outboxRuntimeOptions(nil)
	require.NoError(t, err)
	if nilSC.Metrics != ports.MetricsExporter(rec) {
		t.Fatalf("nil-store-config path must thread the builder's exporter, got %v", nilSC.Metrics)
	}

	// No exporter configured => Metrics nil (factory treats nil as no-op).
	none, err := NewBuilder(cfg).outboxRuntimeOptions(&ports.StoreConfig{Type: "dynamodb"})
	require.NoError(t, err)
	if none.Metrics != nil {
		t.Fatalf("no configured exporter must leave Metrics nil, got %v", none.Metrics)
	}
}
