package cloudwatch

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// MF-4: a metric listed in RollupMetrics is double-published — the
// normal dimensioned datum plus a zero-dimension fleet-rollup copy that
// dimensionless CloudWatch alarms can match.
func TestBatcher_RollupDoublePublishesWithoutDimensions(t *testing.T) {
	b := newBatcher(Config{
		BufferSize:    100,
		RollupMetrics: []string{shared.MetricDLQEntries},
		DefaultTags:   []shared.Tag{{Key: "service", Value: "bridge"}},
		Clock:         clocktest.NewAt(time.Unix(1700000000, 0)),
	})

	b.addCounter(shared.MetricDLQEntries, 1, []shared.Tag{
		{Key: shared.TagKeyRouteID, Value: "orders"},
		{Key: shared.TagKeyCategory, Value: "permanent"},
	})
	b.addCounter(shared.MetricDLQEntries, 2, []shared.Tag{
		{Key: shared.TagKeyRouteID, Value: "invoices"},
		{Key: shared.TagKeyCategory, Value: "expired"},
	})

	data := b.drain()
	// 2 dimensioned series + 1 rollup series (aggregating both).
	if len(data) != 3 {
		t.Fatalf("expected 3 datums (2 dimensioned + 1 rollup), got %d", len(data))
	}
	var rollupSum float64 = -1
	dimensioned := 0
	for _, d := range data {
		if len(d.Dimensions) == 0 {
			rollupSum = *d.Value
		} else {
			dimensioned++
			// Dimensioned copies carry default tags too.
			found := false
			for _, dim := range d.Dimensions {
				if *dim.Name == "service" {
					found = true
				}
			}
			if !found {
				t.Error("dimensioned datum lost the default tag")
			}
		}
	}
	if dimensioned != 2 {
		t.Errorf("dimensioned datums = %d, want 2", dimensioned)
	}
	if rollupSum != 3 {
		t.Errorf("rollup sum = %f, want 3 (fleet aggregate)", rollupSum)
	}
}

// MF-4: gauges and histograms in the rollup set are also double-published.
func TestBatcher_RollupCoversGaugesAndHistograms(t *testing.T) {
	b := newBatcher(Config{
		BufferSize:    100,
		RollupMetrics: []string{shared.MetricOutboxDepth, "Latency"},
		Clock:         clocktest.NewAt(time.Unix(1700000000, 0)),
	})

	b.addGauge(shared.MetricOutboxDepth, 42, []shared.Tag{{Key: shared.TagKeyPartition, Value: "p1"}})
	b.addTimer("Latency", 100*time.Millisecond, []shared.Tag{{Key: "r", Value: "A"}})

	data := b.drain()
	if len(data) != 4 {
		t.Fatalf("expected 4 datums (2 primary + 2 rollup), got %d", len(data))
	}
	zeroDim := 0
	for _, d := range data {
		if len(d.Dimensions) == 0 {
			zeroDim++
		}
	}
	if zeroDim != 2 {
		t.Errorf("zero-dimension rollup datums = %d, want 2", zeroDim)
	}
}

// MF-4: every metric the default alarms target must be in the canonical
// rollup list, otherwise the alarms can never fire (a dimensionless
// alarm never matches dimensioned data).
func TestDefaultAlarms_TargetsAreRollupMetrics(t *testing.T) {
	rollups := map[string]bool{}
	for _, name := range DefaultRollupMetrics() {
		rollups[name] = true
	}
	for _, a := range DefaultAlarms("", "") {
		if !rollups[a.MetricName] {
			t.Errorf("alarm %s targets %s which is not in DefaultRollupMetrics()", a.Name, a.MetricName)
		}
	}
}

// MF-8: rollup copies must not carry the instance tag — they aggregate
// the fleet by design; the dimensioned primary carries it.
func TestBatcher_RollupIgnoresInstanceTag(t *testing.T) {
	cfg := Config{
		BufferSize:    100,
		RollupMetrics: []string{shared.MetricDLQEntries},
		InstanceID:    "bridge-1",
		Clock:         clocktest.NewAt(time.Unix(1700000000, 0)),
	}
	applyDefaults(&cfg)
	b := newBatcher(cfg)

	b.addCounter(shared.MetricDLQEntries, 1, []shared.Tag{{Key: shared.TagKeyRouteID, Value: "orders"}})

	data := b.drain()
	if len(data) != 2 {
		t.Fatalf("expected 2 datums, got %d", len(data))
	}
	for _, d := range data {
		hasInstance := false
		for _, dim := range d.Dimensions {
			if *dim.Name == TagKeyInstanceID {
				hasInstance = true
			}
		}
		if len(d.Dimensions) == 0 && hasInstance {
			t.Error("rollup datum must not carry instance_id")
		}
		if len(d.Dimensions) > 0 && !hasInstance {
			t.Error("dimensioned datum must carry instance_id when configured")
		}
	}
}
