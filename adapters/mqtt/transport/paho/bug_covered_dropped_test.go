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
// a still-COVERED live-route publish must NEVER be acked-and-dropped
// after the grace window — that converts startup slowness into acknowledged
// loss and breaks at-least-once.
//
// After the grace window an unmatched publish is classified by whether its
// topic is STILL covered by a subscription the session wants. A COVERED topic
// (a live route whose receiver handler registered late) is RETAINED un-acked in
// the bounded pending buffer (bounded by receive_maximum) so it is delivered
// once the handler registers, or redelivered by the broker on reconnect — never
// lost. Only a genuine ORPHAN (a route removed from config) is acked, dropped,
// and unsubscribed.
//
// Metrics: covered retentions are counted on MetricMQTTRouterCoveredRetained
// (NOT lost); genuine orphan cleanup stays on MetricMQTTRouterUnmatchedDropped;
// MetricMQTTRouterCoveredDropped now fires ONLY for a covered QoS 0 the buffer
// could not hold (best-effort, QoS 0 has no redelivery contract). A nil covered
// predicate preserves the legacy behaviour (every unmatched publish is an
// orphan).
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
	return h.messageCountContaining(slog.LevelWarn, substr)
}

// messageCountContaining counts recorded messages at exactly the given level
// whose text contains substr. Level matters when a test must distinguish a
// claim made at Debug (an operation that succeeded) from a Warn (one that did
// not).
func (h *recordingLogHandler) messageCountContaining(level slog.Level, substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level == level && strings.Contains(r.Message, substr) {
			n++
		}
	}
	return n
}

// TestBug_SettleUnmatched_CoveredRetained_OrphanDropped drives settleUnmatched
// via two post-grace unmatched publishes — one on a COVERED topic (a
// still-desired subscription whose handler registered late) and one on an
// ORPHAN topic (a route removed from config) — and asserts: the covered
// publish is RETAINED un-acked (never ack-dropped, so at-least-once holds) and
// is delivered once its handler finally registers, while the orphan is
// acked-and-dropped.
//
// Counterfactual (the pre-fix ack-and-drop, reproduced by ack-dropping the
// covered publish in settleUnmatched): the covered publish was ACKED and
// dropped (coveredAcked==1, PendingCount==0) and the late-registering handler
// received NOTHING — acknowledged live-route loss. The require.Equal on
// coveredAcked==0 / PendingCount==1 / the delivered payload FAIL.
func TestBug_SettleUnmatched_CoveredRetained_OrphanDropped(t *testing.T) {
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

	clk.Advance(testGrace + time.Second) // past grace → unmatched publishes are settled

	// Covered-topic publish: a still-desired route whose handler registered late.
	var coveredAcked atomic.Int32
	r.dispatch(&pahov5.Publish{Topic: "live/route/1", QoS: 1, Payload: []byte("x")},
		func() error { coveredAcked.Add(1); return nil })

	// Orphan-topic publish: a route removed from config.
	var orphanAcked atomic.Int32
	r.dispatch(&pahov5.Publish{Topic: "removed/route/9", QoS: 1, Payload: []byte("y")},
		func() error { orphanAcked.Add(1); return nil })

	// the covered publish is RETAINED un-acked (never ack-dropped); only
	// the orphan is acked-and-dropped.
	require.Equal(t, int32(0), coveredAcked.Load(),
		"the covered publish must NOT be acked-and-dropped — that would be acknowledged live-route loss")
	require.Equal(t, int32(1), orphanAcked.Load(), "the orphan publish is still acked-and-dropped")
	require.Equal(t, 1, r.PendingCount(), "the covered publish is retained in the pending buffer")

	require.Equal(t, int64(1), r.CoveredRetainedCount(),
		"the covered publish is counted as a retention, not a loss")
	require.Equal(t, int64(0), r.CoveredDroppedCount(), "a retained QoS 1 covered publish is not dropped")
	require.Equal(t, int64(1), r.UnmatchedDroppedCount(), "the orphan publish is benign cleanup")
	require.Len(t, rec.FindEntries(MetricMQTTRouterCoveredRetained), 1,
		"the covered-retained metric is emitted exactly once")
	require.Len(t, rec.FindEntries(MetricMQTTRouterUnmatchedDropped), 1,
		"the orphan-drop metric is emitted exactly once")
	require.Empty(t, rec.FindEntries(MetricMQTTRouterCoveredDropped),
		"no covered QoS 1/2 is ever dropped")

	// The covered retention WARNs so a slow/absent receiver is alarming.
	require.Equal(t, 1, logs.warnCountContaining("RETAINED covered"),
		"the covered-topic retention must WARN")

	// The retained covered publish is delivered once its handler registers —
	// proof it was NOT lost. RegisterFiltered enrolls the flush in r.wg before
	// returning, so r.Wait() is a deterministic barrier (no sleep).
	var mu sync.Mutex
	var delivered []string
	r.RegisterFiltered("rx", []string{"live/route/1"}, func(pub *pahov5.Publish, ack func() error) {
		mu.Lock()
		delivered = append(delivered, string(pub.Payload))
		mu.Unlock()
		if ack != nil {
			_ = ack()
		}
	})
	r.Wait()

	mu.Lock()
	got := append([]string(nil), delivered...)
	mu.Unlock()
	require.Equal(t, []string{"x"}, got,
		"the RETAINED covered publish is delivered (not lost) once its handler registers")
	require.Equal(t, int32(1), coveredAcked.Load(),
		"the covered publish is acked only AFTER real delivery, preserving at-least-once")
	require.Equal(t, 0, r.PendingCount(), "the pending buffer is drained after the flush")
}

// TestBug_SettleUnmatched_NilCovered_LegacyAllOrphan pins the legacy contract:
// with no covered predicate wired (the direct-router / Route path),
// settleUnmatched treats EVERY post-grace unmatched publish as an orphan — so
// existing router-only tests keep their previous ack-and-drop semantics.
func TestBug_SettleUnmatched_NilCovered_LegacyAllOrphan(t *testing.T) {
	clk := testClock()
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec, withRouterClock(clk), withUnmatchedGrace(testGrace))
	defer r.shutdown()

	clk.Advance(testGrace + time.Second)

	var acked atomic.Int32
	r.dispatch(&pahov5.Publish{Topic: "live/route/1", QoS: 1, Payload: []byte("x")},
		func() error { acked.Add(1); return nil })

	require.Equal(t, int32(1), acked.Load(),
		"a nil covered predicate acks-and-drops every unmatched publish (legacy behaviour)")
	require.Equal(t, int64(0), r.CoveredRetainedCount(),
		"a nil covered predicate never retains a covered publish")
	require.Equal(t, int64(0), r.CoveredDroppedCount(),
		"a nil covered predicate never counts a covered drop")
	require.Equal(t, int64(1), r.UnmatchedDroppedCount(),
		"a nil covered predicate treats every drop as an orphan (legacy behaviour)")
	require.Equal(t, 0, r.PendingCount(), "nothing is retained")
}
