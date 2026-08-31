package outbox

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// CountPending excludes CLAIMED records, so a record left claimed by a failed
// release, an abandoned batch, or a dead owner is invisible: the backlog gauge
// reads zero while messages sit undelivered. The claimed-depth gauge is what
// separates "nothing to do" from "work is stuck" — and on a route using
// ordering keys it is also what a group stalled behind a stranded head looks
// like.

// claimedDepthStore reports a fixed claimed count and records the context shape
// the call received, so the bound can be asserted alongside the value.
type claimedDepthStore struct {
	claimed         int
	claimedErr      error
	claimedCalls    atomic.Int32
	claimedDeadline atomic.Bool
	pending         int
}

func (s *claimedDepthStore) Persist(context.Context, []*persistence.OutboxRecord) error { return nil }

func (s *claimedDepthStore) Claim(context.Context, string, persistence.LeaseToken, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

func (s *claimedDepthStore) Complete(context.Context, []string, persistence.LeaseToken) error {
	return nil
}

func (s *claimedDepthStore) Expire(context.Context, time.Time, string, persistence.LeaseToken) (int, error) {
	return 0, nil
}

func (s *claimedDepthStore) QueryPending(context.Context, string, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

func (s *claimedDepthStore) CountPending(context.Context, string) (int, error) {
	return s.pending, nil
}

func (s *claimedDepthStore) CountClaimed(ctx context.Context, _ string) (int, error) {
	if _, ok := ctx.Deadline(); ok {
		s.claimedDeadline.Store(true)
	}
	s.claimedCalls.Add(1)
	return s.claimed, s.claimedErr
}

var (
	_ ports.OutboxStore                = (*claimedDepthStore)(nil)
	_ ports.OutboxDepthReporter        = (*claimedDepthStore)(nil)
	_ ports.OutboxClaimedDepthReporter = (*claimedDepthStore)(nil)
)

func TestEmitDepth_ReportsClaimedDepthAlongsidePending(t *testing.T) {
	store := &claimedDepthStore{claimed: 4, pending: 0}
	metrics := &ports.RecordingExporter{}
	d := newDepthProbeDrainer(t, store, metrics)

	d.emitDepth(context.Background(), 0)

	if store.claimedCalls.Load() != 1 {
		t.Fatalf("expected exactly one CountClaimed per cycle, got %d", store.claimedCalls.Load())
	}
	if !store.claimedDeadline.Load() {
		t.Fatal("the claimed-depth count must run under an operation deadline")
	}
	gauges := metrics.FindEntries(shared.MetricOutboxClaimedDepth)
	if len(gauges) != 1 || gauges[0].FValue != 4 {
		t.Fatalf("claimed-depth gauge = %v, want a single sample of 4", gauges)
	}
	// The pending gauge must still be the pending count, not the claimed one:
	// depth 0 with claimed 4 is exactly the stranded-work signature.
	pending := metrics.FindEntries(shared.MetricOutboxDepth)
	if len(pending) != 1 || pending[0].FValue != 0 {
		t.Fatalf("pending depth gauge = %v, want a single sample of 0", pending)
	}
}

// A store that has not adopted the capability must not be probed and must not
// produce a gauge: an approximated stranded-work number would be worse than
// none, because the drainer cannot see work stranded by earlier cycles.
func TestEmitDepth_ClaimedDepthSkippedWhenUnsupported(t *testing.T) {
	// expireProbeStore reports pending depth but has NOT adopted the claimed
	// capability — the shape of every backend that opts out.
	store := &expireProbeStore{}
	metrics := &ports.RecordingExporter{}
	d := newDepthProbeDrainer(t, store, metrics)

	d.emitDepth(context.Background(), 0)

	if got := metrics.FindEntries(shared.MetricOutboxClaimedDepth); len(got) != 0 {
		t.Fatalf("a store without the capability must emit no claimed-depth gauge, got %v", got)
	}
}

// A REAL count failure must not be masked: no gauge for that cycle (so the
// missing-data alarm can fire) plus the depth-failure counter.
func TestEmitDepth_ClaimedDepthFailureIsCountedNotMasked(t *testing.T) {
	store := &claimedDepthStore{claimedErr: errors.New("store: count failed (simulated)")}
	metrics := &ports.RecordingExporter{}
	d := newDepthProbeDrainer(t, store, metrics)

	d.emitDepth(context.Background(), 0)

	if got := metrics.FindEntries(shared.MetricOutboxClaimedDepth); len(got) != 0 {
		t.Fatalf("a failed count must emit no gauge, got %v", got)
	}
	if got := metrics.FindEntries(shared.MetricOutboxDepthFailures); len(got) == 0 {
		t.Fatal("a failed claimed-depth count must be recorded on the depth-failure counter")
	}
}

// The exported sentinel means "cannot report", not "failed": no gauge, no
// failure counter.
func TestEmitDepth_ClaimedDepthUnsupportedSentinelIsBenign(t *testing.T) {
	store := &claimedDepthStore{claimedErr: ports.ErrOutboxDepthUnsupported}
	metrics := &ports.RecordingExporter{}
	d := newDepthProbeDrainer(t, store, metrics)

	d.emitDepth(context.Background(), 0)

	if got := metrics.FindEntries(shared.MetricOutboxClaimedDepth); len(got) != 0 {
		t.Fatalf("the unsupported sentinel must emit no gauge, got %v", got)
	}
	if got := metrics.FindEntries(shared.MetricOutboxDepthFailures); len(got) != 0 {
		t.Fatalf("the unsupported sentinel is not a failure, got %v", got)
	}
}

// newDepthProbeDrainer builds a drainer whose only job is to emit depth gauges
// for the supplied store.
func newDepthProbeDrainer(t *testing.T, store ports.OutboxStore, metrics ports.MetricsExporter) *Drainer {
	t.Helper()
	return New(Config{
		OutboxStore:  store,
		Sender:       &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }},
		RouteID:      "route-depth",
		PartitionKey: "SESSION#sess-depth",
		Policy:       routing.RoutePolicy{SendTimeout: 2 * time.Second, MaxReplayAttempts: 5},
		Metrics:      metrics,
		TokenFn:      func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})
}
