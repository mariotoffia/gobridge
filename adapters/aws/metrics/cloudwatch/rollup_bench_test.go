package cloudwatch

import (
	"fmt"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// Rolling a metric up double-publishes every emission of it, so growing the
// default rollup list grows the work on the emission path — the path a
// per-session MQTT counter runs on. These measure the two things that could make
// that expensive: whether the rollup decision costs more as the list grows (it
// must not; it is a map lookup), and what a matched emission costs over an
// unmatched one.
//
// Measured on an M4 Pro at 300k iterations: an unmatched emission is ~92 ns and
// 4 allocations, a matched one ~123 ns and 6 — the second aggregate entry the
// rollup copy needs. A 500-entry rollup list measures the same as the shipped
// one, so the list length costs nothing. Run these with a real iteration count;
// at a few hundred they report warm-up, not steady state.

const benchRollupMetric = shared.MetricDLQEntries

func benchBatcher(tb testing.TB, rollups []string) *batcher {
	tb.Helper()
	return newBatcher(Config{
		BufferSize:        1 << 16,
		MaxBufferedDatums: 1 << 20,
		RollupMetrics:     rollups,
		DefaultTags:       []shared.Tag{{Key: "service", Value: "bridge"}},
		Clock:             clocktest.NewAt(time.Unix(1700000000, 0)),
	})
}

func benchTags(route string) []shared.Tag {
	return []shared.Tag{
		{Key: shared.TagKeyRouteID, Value: route},
		{Key: shared.TagKeyCategory, Value: "permanent"},
	}
}

// BenchmarkBatcherAdd_RollupMatchCost is the simple case: one counter emission,
// with and without the metric being rolled up. The delta is the whole cost of
// adding a metric to the default list.
func BenchmarkBatcherAdd_RollupMatchCost(b *testing.B) {
	for _, c := range []struct {
		name    string
		rollups []string
	}{
		{"unmatched", DefaultRollupMetrics()},
		{"matched", append(DefaultRollupMetrics(), benchRollupMetric)},
	} {
		b.Run(c.name, func(b *testing.B) {
			name := benchRollupMetric
			if c.name == "unmatched" {
				name = "NotRolledUp"
			}
			bt := benchBatcher(b, c.rollups)
			tags := benchTags("orders")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				bt.addCounter(name, 1, tags)
			}
		})
	}
}

// BenchmarkBatcherAdd_RollupListSize proves the decision is O(1) in the list
// length. If it ever became a scan, every emission on a large deployment would
// pay for every metric anyone chose to roll up.
func BenchmarkBatcherAdd_RollupListSize(b *testing.B) {
	shipped := DefaultRollupMetrics()
	wide := append([]string(nil), shipped...)
	for i := len(wide); i < 500; i++ {
		wide = append(wide, fmt.Sprintf("SyntheticRollup%03d", i))
	}

	for _, c := range []struct {
		name    string
		rollups []string
	}{
		{"shipped", shipped},
		{"wide", wide},
	} {
		b.Run(c.name, func(b *testing.B) {
			bt := benchBatcher(b, c.rollups)
			tags := benchTags("orders")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				bt.addCounter(benchRollupMetric, 1, tags)
			}
		})
	}
}

// BenchmarkBatcherAdd_FleetShapedMix is the complex case: a fleet's worth of
// mixed emissions across many routes and sessions, half of them on rolled-up
// metrics, drained on the flush cadence so aggregate maps are rebuilt the way a
// running exporter rebuilds them.
func BenchmarkBatcherAdd_FleetShapedMix(b *testing.B) {
	routes := []string{"orders", "invoices", "telemetry", "audit", "billing", "webhooks"}
	sessions := []string{"mqtt-a", "mqtt-b", "sqs-a", "asb-a"}
	bt := benchBatcher(b, DefaultRollupMetrics())

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		route := routes[i%len(routes)]
		session := sessions[i%len(sessions)]
		bt.addCounter(shared.MetricMessagesReceived, 1, benchTags(route))
		bt.addCounter(shared.MetricDLQEntries, 1, benchTags(route))
		bt.addCounter(shared.MetricReconcileFailures, 1,
			[]shared.Tag{{Key: shared.TagKeySessionID, Value: session}})
		bt.addGauge(shared.MetricOutboxDepth, float64(i%1000),
			[]shared.Tag{{Key: shared.TagKeyPartition, Value: route}})
		bt.addTimer(shared.MetricOutboxDrainLatency, time.Duration(i%50)*time.Millisecond,
			[]shared.Tag{{Key: shared.TagKeySessionID, Value: session}})
		if i%512 == 0 {
			bt.drain()
		}
	}
}
