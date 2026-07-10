package paho

import (
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// blocking-#4 (non-blocking review item): the common path — a publish arrives
// DURING grace, is buffered, and the grace-end sweep then sees it is still
// covered and keeps it pending — did NOT count it on
// MetricMQTTRouterCoveredRetained, so a slow/absent receiver was invisible on
// the metric. The grace-end sweep now counts (and WARN-logs, deduped) covered
// retentions — exactly once per entry, never double-counting a retention a
// post-grace dispatch already counted or a second grace window re-sweeps.
// ═══════════════════════════════════════════════════════════════════════════

// TestBug_GraceSweep_CountsCoveredRetained_Once pins blocking-#4: an in-grace
// buffered covered publish retained by the grace-end sweep is counted on
// MetricMQTTRouterCoveredRetained, and a repeat sweep does NOT re-count it.
//
// Mutation killed: revert sweepUnmatched to `settlePending`-without-counting
// (remove the retainedByTopic/noteCoveredRetained accounting). Then the sweep
// retains but does not count, CoveredRetainedCount stays 0, and the first
// require.Equal(int64(1), ...) FAILs.
func TestBug_GraceSweep_CountsCoveredRetained_Once(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	logs := &recordingLogHandler{}

	covered := map[string]bool{"live/route": true}
	r := newRouter(slog.New(logs), rec,
		withRouterClock(clk),
		withUnmatchedGrace(testGrace),
		withCovered(func(topic string) bool { return covered[topic] }),
	)
	defer r.shutdown()

	// A covered QoS 1 publish arrives DURING the grace window (newRouter armed
	// graceDeadline = now + testGrace): it is buffered, NOT yet counted as a
	// retention (that is the grace-backlog case, MetricMQTTRouterBuffered).
	var acked atomic.Int32
	r.dispatch(&pahov5.Publish{Topic: "live/route", QoS: 1, Payload: []byte("x")},
		func() error { acked.Add(1); return nil })

	require.Equal(t, 1, r.PendingCount(), "the covered publish is buffered during grace")
	require.Equal(t, int64(0), r.CoveredRetainedCount(),
		"in-grace buffering does not count a retention yet")
	require.Len(t, rec.FindEntries(MetricMQTTRouterBuffered), 1)

	// Grace ends. Run the sweep synchronously (it is exactly what the grace
	// worker calls when the timer fires) so the assertions are deterministic.
	clk.Advance(testGrace + time.Second)
	r.sweepUnmatched()

	require.Equal(t, int64(1), r.CoveredRetainedCount(),
		"the grace-end sweep counts the still-covered retained publish (blocking-#4)")
	require.Len(t, rec.FindEntries(MetricMQTTRouterCoveredRetained), 1,
		"the covered-retained metric is emitted exactly once by the sweep")
	require.Equal(t, 1, logs.warnCountContaining("RETAINED covered"),
		"the sweep WARN-logs the covered retention once")
	require.Equal(t, 1, r.PendingCount(), "the covered publish stays retained (never dropped)")
	require.Equal(t, int32(0), acked.Load(), "a covered QoS 1 retention is never acked (at-least-once)")

	// A SECOND sweep (e.g. another grace window on reconnect) must NOT
	// double-count the still-retained entry — the per-entry retainCounted latch
	// guards it.
	r.sweepUnmatched()
	require.Equal(t, int64(1), r.CoveredRetainedCount(),
		"a repeat sweep must NOT re-count an already-counted retention (dedup)")
	require.Len(t, rec.FindEntries(MetricMQTTRouterCoveredRetained), 1,
		"no duplicate covered-retained metric on the repeat sweep")
}
