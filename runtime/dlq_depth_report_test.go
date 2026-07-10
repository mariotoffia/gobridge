package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// fakeDLQDepth is a DLQ read port that also implements ports.DLQDepthReporter.
type fakeDLQDepth struct {
	depth      int
	depthErr   error
	depthCalls int
}

var (
	_ ports.DLQReader        = (*fakeDLQDepth)(nil)
	_ ports.DLQDepthReporter = (*fakeDLQDepth)(nil)
)

func (s *fakeDLQDepth) Get(context.Context, string) (routing.DLQEntry, error) {
	return routing.DLQEntry{}, nil
}

func (s *fakeDLQDepth) List(context.Context, routing.DLQFilter) ([]routing.DLQEntry, error) {
	return nil, nil
}

func (s *fakeDLQDepth) Depth(context.Context) (int, error) {
	s.depthCalls++
	return s.depth, s.depthErr
}

// fakeDLQNoDepth is a DLQ read port WITHOUT the optional depth capability.
type fakeDLQNoDepth struct{}

func (fakeDLQNoDepth) Get(context.Context, string) (routing.DLQEntry, error) {
	return routing.DLQEntry{}, nil
}

func (fakeDLQNoDepth) List(context.Context, routing.DLQFilter) ([]routing.DLQEntry, error) {
	return nil, nil
}

// H-OBS DLQ-1: ReportDLQDepth samples the standing DLQ backlog via the optional
// reporter and emits it as MetricDLQDepth. Fails before the metric/hook exist.
func TestReportDLQDepth_EmitsGaugeWhenReporterPresent(t *testing.T) {
	rec := &ports.RecordingExporter{}
	store := &fakeDLQDepth{depth: 10000}

	depth, reported, err := runtime.ReportDLQDepth(context.Background(), store, rec)
	if err != nil {
		t.Fatalf("ReportDLQDepth: %v", err)
	}
	if !reported {
		t.Fatal("expected reported=true for a store that implements DLQDepthReporter")
	}
	if depth != 10000 {
		t.Errorf("depth = %d, want 10000", depth)
	}
	entries := rec.FindEntries(shared.MetricDLQDepth)
	if len(entries) != 1 {
		t.Fatalf("expected 1 DLQDepth gauge, got %d", len(entries))
	}
	if entries[0].Kind != "gauge" || entries[0].FValue != 10000 {
		t.Errorf("DLQDepth entry = %+v, want gauge 10000", entries[0])
	}
	if store.depthCalls != 1 {
		t.Errorf("Depth calls = %d, want 1", store.depthCalls)
	}
}

// Fail-safe: a DLQ store without the capability yields no emission and no error,
// so the build/behaviour stays green until adapters adopt DLQDepthReporter.
func TestReportDLQDepth_NoOpWhenReporterAbsent(t *testing.T) {
	rec := &ports.RecordingExporter{}

	depth, reported, err := runtime.ReportDLQDepth(context.Background(), fakeDLQNoDepth{}, rec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reported {
		t.Error("expected reported=false for a store without DLQDepthReporter")
	}
	if depth != 0 {
		t.Errorf("depth = %d, want 0", depth)
	}
	if got := len(rec.FindEntries(shared.MetricDLQDepth)); got != 0 {
		t.Errorf("expected no DLQDepth emission, got %d", got)
	}
}

// A backend error on Depth is surfaced and emits nothing (do not publish a
// misleading zero).
func TestReportDLQDepth_PropagatesErrorAndEmitsNothing(t *testing.T) {
	rec := &ports.RecordingExporter{}
	boom := errors.New("count failed")
	store := &fakeDLQDepth{depthErr: boom}

	depth, reported, err := runtime.ReportDLQDepth(context.Background(), store, rec)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if reported || depth != 0 {
		t.Errorf("reported = %v, depth = %d, want false/0 on error", reported, depth)
	}
	if got := len(rec.FindEntries(shared.MetricDLQDepth)); got != 0 {
		t.Errorf("expected no DLQDepth emission on error, got %d", got)
	}
}

// countingOutbox wraps a base OutboxStore and adds ports.OutboxDepthReporter.
type countingOutbox struct {
	ports.OutboxStore
	pending int
	calls   int
}

func (s *countingOutbox) CountPending(context.Context, string) (int, error) {
	s.calls++
	return s.pending, nil
}

// H-OBS: the instrumentation wrapper must FORWARD the optional
// OutboxDepthReporter capability so the drainer (which in production holds the
// wrapper, not the raw store) can read the true backlog. Fails before the fix.
func TestInstrumentedOutboxStore_CountPendingForwardsToInner(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := &countingOutbox{OutboxStore: NewFakeOutboxStore(), pending: 4242}

	var reporter ports.OutboxDepthReporter = runtime.NewInstrumentedOutboxStore(inner, rec, clock.System)
	n, err := reporter.CountPending(context.Background(), "SESSION#s1")
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}
	if n != 4242 {
		t.Errorf("CountPending = %d, want 4242 (delegated to inner)", n)
	}
	if inner.calls != 1 {
		t.Errorf("inner CountPending calls = %d, want 1", inner.calls)
	}
}

// The wrapper always advertises the capability so the drainer probe succeeds;
// when the inner store cannot count, CountPending returns an error (which the
// drainer treats as "fall back to the claimed-count lower bound").
func TestInstrumentedOutboxStore_CountPendingUnsupportedInner(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := NewFakeOutboxStore() // no OutboxDepthReporter

	var reporter ports.OutboxDepthReporter = runtime.NewInstrumentedOutboxStore(inner, rec, clock.System)
	// The wrapper must return the EXPORTED sentinel (not a bespoke error) so the
	// drainer can distinguish "unsupported" (benign fallback) from a real count
	// failure via errors.Is.
	if _, err := reporter.CountPending(context.Background(), "SESSION#s1"); !errors.Is(err, ports.ErrOutboxDepthUnsupported) {
		t.Errorf("CountPending err = %v, want ports.ErrOutboxDepthUnsupported", err)
	}
}

// The capability-preserving constructor (used by the composition root) must also
// forward CountPending so production drainers see the true backlog.
func TestInstrumentedOutboxStoreCapabilityPreserving_ForwardsCountPending(t *testing.T) {
	rec := &ports.RecordingExporter{}
	inner := &countingOutbox{OutboxStore: NewFakeOutboxStore(), pending: 77}

	store := runtime.NewInstrumentedOutboxStoreCapabilityPreserving(inner, rec, clock.System)
	reporter, ok := store.(ports.OutboxDepthReporter)
	if !ok {
		t.Fatal("capability-preserving wrapper must satisfy ports.OutboxDepthReporter")
	}
	n, err := reporter.CountPending(context.Background(), "p")
	if err != nil || n != 77 {
		t.Fatalf("CountPending = %d, err = %v, want 77/nil", n, err)
	}
}
