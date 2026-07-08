package paho

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// M-3 (MEDIUM): covered-topic post-grace ack-and-drop silently loses live-route
// QoS 1/2 and is conflated with benign orphan cleanup.
//
// After the grace window an unmatched publish is acked-and-dropped. If its
// topic is STILL covered by a subscription the session wants (a live route
// whose receiver handler registered late), that is REAL message loss — but the
// old code counted it on the same MetricMQTTRouterUnmatchedDropped as a benign
// orphan (a route removed from config), and warned only at DEBUG.
//
// Fix: split the metric — covered-topic drops are counted on
// MetricMQTTRouterCoveredDropped and WARN-logged; genuine orphan cleanup stays
// on MetricMQTTRouterUnmatchedDropped. A nil covered predicate preserves the
// legacy behaviour (every drop is an orphan).
// ═══════════════════════════════════════════════════════════════════════════

// recordingLogHandler captures slog records so a test can assert that the
// covered-topic (real-loss) drop is WARN-logged.
type recordingLogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}
func (h *recordingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingLogHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingLogHandler) warnCountContaining(substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level == slog.LevelWarn && strings.Contains(r.Message, substr) {
			n++
		}
	}
	return n
}

// TestBug_DropUnmatched_CoveredVsOrphan_MetricSplit drives dropUnmatched via
// two post-grace drops — one on a COVERED topic (real loss) and one on an
// ORPHAN topic (benign) — and asserts the M-3 metric split plus the
// covered-topic WARN.
//
// Counterfactual (proven by forcing isCovered=false in dropUnmatched):
// pre-fix BOTH drops land on MetricMQTTRouterUnmatchedDropped
// (UnmatchedDroppedCount==2, CoveredDroppedCount==0) with no covered WARN — the
// real loss masked by benign cleanup.
func TestBug_DropUnmatched_CoveredVsOrphan_MetricSplit(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	logs := &recordingLogHandler{}

	coveredTopics := map[string]bool{"live/route/1": true}
	r := newRouter(slog.New(logs), rec,
		withRouterClock(clk),
		withUnmatchedGrace(testGrace),
		withCovered(func(topic string) bool { return coveredTopics[topic] }),
	)
	defer r.shutdown()

	clk.Advance(testGrace + time.Second) // past grace → unmatched publishes are dropped

	// Covered-topic drop: a still-desired route whose handler registered late.
	var coveredAcked atomic.Int32
	r.dispatch(&pahov5.Publish{Topic: "live/route/1", QoS: 1, Payload: []byte("x")},
		func() error { coveredAcked.Add(1); return nil })

	// Orphan-topic drop: a route removed from config.
	var orphanAcked atomic.Int32
	r.dispatch(&pahov5.Publish{Topic: "removed/route/9", QoS: 1, Payload: []byte("y")},
		func() error { orphanAcked.Add(1); return nil })

	// Both are acked-and-dropped past grace (freeing the broker in-flight slot).
	require.Equal(t, int32(1), coveredAcked.Load(), "covered-topic drop is still acked")
	require.Equal(t, int32(1), orphanAcked.Load(), "orphan-topic drop is still acked")

	// M-3 metric split.
	require.Equal(t, int64(1), r.CoveredDroppedCount(),
		"the covered-topic drop is counted as REAL live-route loss")
	require.Equal(t, int64(1), r.UnmatchedDroppedCount(),
		"the orphan-topic drop is counted as benign cleanup")
	require.Len(t, rec.FindEntries(MetricMQTTRouterCoveredDropped), 1,
		"covered-drop metric emitted exactly once")
	require.Len(t, rec.FindEntries(MetricMQTTRouterUnmatchedDropped), 1,
		"orphan-drop metric emitted exactly once")

	// The covered (real-loss) drop must WARN so it is alarming; the orphan
	// drop does not warn here (its warn is the deduped unsubscribe path).
	require.Equal(t, 1, logs.warnCountContaining("DROPPED live-route"),
		"the covered-topic real-loss drop must WARN")
}

// TestBug_DropUnmatched_NilCovered_LegacyAllOrphan pins the legacy contract:
// with no covered predicate wired (the direct-router / Route path), EVERY
// post-grace drop is treated as an orphan — so existing router-only tests keep
// their previous metric semantics.
func TestBug_DropUnmatched_NilCovered_LegacyAllOrphan(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk), withUnmatchedGrace(testGrace))
	defer r.shutdown()

	clk.Advance(testGrace + time.Second)

	r.dispatch(&pahov5.Publish{Topic: "live/route/1", QoS: 1, Payload: []byte("x")},
		func() error { return nil })

	require.Equal(t, int64(0), r.CoveredDroppedCount(),
		"a nil covered predicate never counts a covered drop")
	require.Equal(t, int64(1), r.UnmatchedDroppedCount(),
		"a nil covered predicate treats every drop as an orphan (legacy behaviour)")
}
