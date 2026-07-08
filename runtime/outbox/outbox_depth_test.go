package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestDrainBatch_EmitsOutboxDepthGauge validates that the drainer emits
// shared.MetricOutboxDepth from its own poll cadence (each drainBatch),
// independent of any backpressure config (MaxOutboxDepth is an ingress-only
// concern the drainer does not consult) and independent of ingress. The gauge
// reflects the claimed count and is emitted even when caught up (0), so the
// series is continuous while an outbox is configured.
//
// White-box: drainBatch is exercised directly so the emission can be asserted
// deterministically without a poll loop.
func TestDrainBatch_EmitsOutboxDepthGauge(t *testing.T) {
	const partition = "SESSION#sess-depth"

	rec1 := deferredTestRecord(t, "rec-1", "")
	rec2 := deferredTestRecord(t, "rec-2", "")
	store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{rec1, rec2}}

	sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }}
	rec := &ports.RecordingExporter{}

	d := New(Config{
		OutboxStore:  store,
		Sender:       sender,
		RouteID:      "r1",
		PartitionKey: partition,
		Policy:       routing.RoutePolicy{SendTimeout: 5 * time.Second, MaxReplayAttempts: 5},
		Metrics:      rec,
		TokenFn:      func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})

	// First drain: two records claimed → depth gauge = 2, tagged partition.
	if _, _, err := d.drainBatch(context.Background(), deferredTestToken()); err != nil {
		t.Fatalf("drainBatch #1: %v", err)
	}
	depth := rec.FindEntries(shared.MetricOutboxDepth)
	if len(depth) != 1 {
		t.Fatalf("expected one OutboxDepth emission, got %d", len(depth))
	}
	if depth[0].Kind != "gauge" {
		t.Errorf("OutboxDepth Kind = %q, want gauge", depth[0].Kind)
	}
	if depth[0].FValue != 2 {
		t.Errorf("OutboxDepth = %v, want 2 (claimed count)", depth[0].FValue)
	}
	if len(depth[0].Tags) != 1 ||
		depth[0].Tags[0].Key != shared.TagKeyPartition ||
		depth[0].Tags[0].Value != partition {
		t.Errorf("OutboxDepth tags = %+v, want [{%s %s}]", depth[0].Tags, shared.TagKeyPartition, partition)
	}

	// Second drain: the store is now empty (Claim nils its backlog) → the
	// gauge still emits, this time 0, so the series is continuous when caught up.
	if _, _, err := d.drainBatch(context.Background(), deferredTestToken()); err != nil {
		t.Fatalf("drainBatch #2: %v", err)
	}
	depth = rec.FindEntries(shared.MetricOutboxDepth)
	if len(depth) != 2 {
		t.Fatalf("expected a second OutboxDepth emission when caught up, got %d", len(depth))
	}
	if depth[1].FValue != 0 {
		t.Errorf("caught-up OutboxDepth = %v, want 0", depth[1].FValue)
	}
}
