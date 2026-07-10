package outbox

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// depthReportingStore is an OutboxStore that ALSO implements
// ports.OutboxDepthReporter. Claim returns a small batch (simulating the claim
// ceiling) while CountPending reports a large standing backlog, so a test can
// prove the drain-path MetricOutboxDepth reflects the TRUE backlog rather than
// the claim batch size (H-OBS OutboxDepth collapse).
type depthReportingStore struct {
	claimable    []*persistence.OutboxRecord
	pendingCount int
	countCalls   int
	// countErr, when set, is returned by CountPending to simulate either a REAL
	// backend failure or the ports.ErrOutboxDepthUnsupported sentinel.
	countErr error
}

var (
	_ ports.OutboxStore         = (*depthReportingStore)(nil)
	_ ports.OutboxDepthReporter = (*depthReportingStore)(nil)
)

func (s *depthReportingStore) Persist(context.Context, []*persistence.OutboxRecord) error {
	return nil
}

func (s *depthReportingStore) Claim(_ context.Context, _ string, _ persistence.LeaseToken, _ int) ([]*persistence.OutboxRecord, error) {
	recs := s.claimable
	s.claimable = nil
	return recs, nil
}

func (s *depthReportingStore) Complete(context.Context, []string, persistence.LeaseToken) error {
	return nil
}

func (s *depthReportingStore) Expire(context.Context, time.Time, string) (int, error) {
	return 0, nil
}

func (s *depthReportingStore) QueryPending(context.Context, string, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

func (s *depthReportingStore) CountPending(context.Context, string) (int, error) {
	s.countCalls++
	if s.countErr != nil {
		return 0, s.countErr
	}
	return s.pendingCount, nil
}

// TestDrainBatch_OutboxDepthReportsBacklogNotBatchSize pins the H-OBS fix: when
// the store reports a true pending count via ports.OutboxDepthReporter, the
// drain path emits that backlog as MetricOutboxDepth — NOT the (saturating)
// claim batch size — and emits the claimed count separately as
// MetricOutboxClaimBatchSize. Fails before the fix (OutboxDepth would be the
// 2-record claim size, masking a 9500-deep backlog).
func TestDrainBatch_OutboxDepthReportsBacklogNotBatchSize(t *testing.T) {
	const partition = "SESSION#sess-backlog"

	rec1 := deferredTestRecord(t, "rec-1", "")
	rec2 := deferredTestRecord(t, "rec-2", "")
	store := &depthReportingStore{
		claimable:    []*persistence.OutboxRecord{rec1, rec2},
		pendingCount: 9500,
	}

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

	if _, _, err := d.drainBatch(context.Background(), deferredTestToken()); err != nil {
		t.Fatalf("drainBatch: %v", err)
	}

	depth := rec.FindEntries(shared.MetricOutboxDepth)
	if len(depth) != 1 {
		t.Fatalf("expected one OutboxDepth emission, got %d", len(depth))
	}
	if depth[0].Kind != "gauge" {
		t.Errorf("OutboxDepth Kind = %q, want gauge", depth[0].Kind)
	}
	if depth[0].FValue != 9500 {
		t.Errorf("OutboxDepth = %v, want 9500 (true backlog from CountPending, not the 2-record claim batch)", depth[0].FValue)
	}
	if len(depth[0].Tags) != 1 ||
		depth[0].Tags[0].Key != shared.TagKeyPartition ||
		depth[0].Tags[0].Value != partition {
		t.Errorf("OutboxDepth tags = %+v, want [{%s %s}]", depth[0].Tags, shared.TagKeyPartition, partition)
	}
	if store.countCalls != 1 {
		t.Errorf("CountPending calls = %d, want exactly 1 (once per drain cycle with a backlog)", store.countCalls)
	}

	batch := rec.FindEntries(shared.MetricOutboxClaimBatchSize)
	if len(batch) != 1 {
		t.Fatalf("expected one OutboxClaimBatchSize emission, got %d", len(batch))
	}
	if batch[0].FValue != 2 {
		t.Errorf("OutboxClaimBatchSize = %v, want 2 (claimed count this cycle)", batch[0].FValue)
	}
	if len(batch[0].Tags) != 1 || batch[0].Tags[0].Key != shared.TagKeyPartition {
		t.Errorf("OutboxClaimBatchSize tags = %+v, want partition tag", batch[0].Tags)
	}
}

// TestDrainBatch_OutboxDepthFallsBackToClaimCount pins the fail-safe: a store
// WITHOUT ports.OutboxDepthReporter keeps the legacy claimed-count OutboxDepth
// (a saturating lower bound) so the continuous gauge and its breaching-on-
// missing alarm stay alive, and the honest claim-batch-size gauge is emitted
// too. No CountPending round-trip happens.
func TestDrainBatch_OutboxDepthFallsBackToClaimCount(t *testing.T) {
	const partition = "SESSION#sess-fallback"

	rec1 := deferredTestRecord(t, "rec-1", "")
	rec2 := deferredTestRecord(t, "rec-2", "")
	// deferredFakeStore implements OutboxStore(+Releaser) but NOT DepthReporter.
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

	if _, _, err := d.drainBatch(context.Background(), deferredTestToken()); err != nil {
		t.Fatalf("drainBatch: %v", err)
	}

	depth := rec.FindEntries(shared.MetricOutboxDepth)
	if len(depth) != 1 || depth[0].FValue != 2 {
		t.Fatalf("fallback OutboxDepth = %+v, want single emission of 2 (claimed count)", depth)
	}
	batch := rec.FindEntries(shared.MetricOutboxClaimBatchSize)
	if len(batch) != 1 || batch[0].FValue != 2 {
		t.Fatalf("OutboxClaimBatchSize = %+v, want single emission of 2", batch)
	}
}

// TestDrainBatch_OutboxDepthRealErrorNotMaskedAsFallback pins the H-OBS
// blocking fix: when a SUPPORTED depth reporter's CountPending returns a REAL
// error (a DB/read failure, NOT ports.ErrOutboxDepthUnsupported), the drainer
// must NOT mask it behind the saturating claimed-count fallback. It skips the
// OutboxDepth emission for that cycle (so the missing-data alarm can catch a
// persistently broken query) and records the failure on MetricOutboxDepthFailures
// plus a structured error log. Mutation check: the pre-fix "swallow any error →
// depth = claimedThisCycle" would emit OutboxDepth=2 and fail this test.
func TestDrainBatch_OutboxDepthRealErrorNotMaskedAsFallback(t *testing.T) {
	const partition = "SESSION#sess-realerr"

	rec1 := deferredTestRecord(t, "rec-1", "")
	rec2 := deferredTestRecord(t, "rec-2", "")
	store := &depthReportingStore{
		claimable: []*persistence.OutboxRecord{rec1, rec2},
		countErr:  errors.New("dynamodb: internal server error"),
	}

	var logbuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelError}))

	sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }}
	rec := &ports.RecordingExporter{}

	d := New(Config{
		OutboxStore:  store,
		Sender:       sender,
		RouteID:      "r1",
		PartitionKey: partition,
		Policy:       routing.RoutePolicy{SendTimeout: 5 * time.Second, MaxReplayAttempts: 5},
		Metrics:      rec,
		Logger:       logger,
		TokenFn:      func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})

	if _, _, err := d.drainBatch(context.Background(), deferredTestToken()); err != nil {
		t.Fatalf("drainBatch: %v", err)
	}

	if got := rec.FindEntries(shared.MetricOutboxDepth); len(got) != 0 {
		t.Errorf("OutboxDepth must NOT be emitted on a real count failure, got %+v", got)
	}
	fails := rec.FindEntries(shared.MetricOutboxDepthFailures)
	if len(fails) != 1 || fails[0].Kind != "counter" || fails[0].IValue != 1 {
		t.Errorf("OutboxDepthFailures = %+v, want single counter of 1", fails)
	}
	// Liveness gauge still flows so the drainer is not silent.
	if batch := rec.FindEntries(shared.MetricOutboxClaimBatchSize); len(batch) != 1 || batch[0].FValue != 2 {
		t.Errorf("OutboxClaimBatchSize = %+v, want single emission of 2", batch)
	}
	if !strings.Contains(logbuf.String(), "outbox depth query failed") {
		t.Errorf("expected a structured error log for the failed depth query, got: %q", logbuf.String())
	}
}

// TestDrainBatch_OutboxDepthUnsupportedSentinelFallsBack pins the OTHER half of
// the blocking fix: the ports.ErrOutboxDepthUnsupported sentinel is benign — the
// drainer falls back to the claimed-count lower bound silently (no failure
// counter, no error log), exactly as an adapter that has not adopted the
// capability behaves.
func TestDrainBatch_OutboxDepthUnsupportedSentinelFallsBack(t *testing.T) {
	const partition = "SESSION#sess-unsupported"

	rec1 := deferredTestRecord(t, "rec-1", "")
	rec2 := deferredTestRecord(t, "rec-2", "")
	store := &depthReportingStore{
		claimable: []*persistence.OutboxRecord{rec1, rec2},
		countErr:  ports.ErrOutboxDepthUnsupported,
	}

	var logbuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelError}))

	sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }}
	rec := &ports.RecordingExporter{}

	d := New(Config{
		OutboxStore:  store,
		Sender:       sender,
		RouteID:      "r1",
		PartitionKey: partition,
		Policy:       routing.RoutePolicy{SendTimeout: 5 * time.Second, MaxReplayAttempts: 5},
		Metrics:      rec,
		Logger:       logger,
		TokenFn:      func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})

	if _, _, err := d.drainBatch(context.Background(), deferredTestToken()); err != nil {
		t.Fatalf("drainBatch: %v", err)
	}

	depth := rec.FindEntries(shared.MetricOutboxDepth)
	if len(depth) != 1 || depth[0].FValue != 2 {
		t.Errorf("OutboxDepth = %+v, want single fallback emission of 2 (claimed count)", depth)
	}
	if got := rec.FindEntries(shared.MetricOutboxDepthFailures); len(got) != 0 {
		t.Errorf("unsupported sentinel must NOT count as a failure, got %+v", got)
	}
	if strings.Contains(logbuf.String(), "outbox depth query failed") {
		t.Errorf("unsupported sentinel must NOT log an error, got: %q", logbuf.String())
	}
}

// TestDrainBatch_OutboxDepthReportsBacklogOnZeroClaimCycle pins the non-blocking
// fix: a SUPPORTED depth reporter is consulted on EVERY drain cycle, including a
// zero-claim cycle (e.g. Claim returned 0 under contention while records still
// pend). The backlog reports its real size (9500) instead of a false-healthy
// zero. Mutation check: the pre-fix "probe only when claimedThisCycle > 0" would
// emit OutboxDepth=0 here and fail this test.
func TestDrainBatch_OutboxDepthReportsBacklogOnZeroClaimCycle(t *testing.T) {
	const partition = "SESSION#sess-zeroclaim"

	// No claimable records: Claim returns 0 this cycle.
	store := &depthReportingStore{claimable: nil, pendingCount: 9500}

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

	if _, _, err := d.drainBatch(context.Background(), deferredTestToken()); err != nil {
		t.Fatalf("drainBatch: %v", err)
	}

	depth := rec.FindEntries(shared.MetricOutboxDepth)
	if len(depth) != 1 || depth[0].FValue != 9500 {
		t.Fatalf("OutboxDepth = %+v, want single emission of 9500 (true backlog on a zero-claim cycle)", depth)
	}
	batch := rec.FindEntries(shared.MetricOutboxClaimBatchSize)
	if len(batch) != 1 || batch[0].FValue != 0 {
		t.Errorf("OutboxClaimBatchSize = %+v, want single emission of 0 (nothing claimed)", batch)
	}
	if store.countCalls != 1 {
		t.Errorf("CountPending calls = %d, want 1 (probed even on a zero-claim cycle)", store.countCalls)
	}
}
