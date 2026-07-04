package tenant

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// readerTracker implements BOTH ports.TenantUsageTracker and the optional
// ports.TenantUsageReader extension. It returns a settable TenantUsage (or a
// settable error) from Usage, counts Usage calls, and records the in-flight
// deltas passed to IncrementInFlight so tests can assert the +1/-1 balance.
// It is the reader-capable counterpart to the reader-LESS stubTracker.
type readerTracker struct {
	usage      ports.TenantUsage
	usageErr   error
	usageCalls atomic.Int64

	mu             sync.Mutex
	inFlightDeltas []int64
}

// Compile-time capability assertions: readerTracker satisfies both the
// increment-only tracker and the optional reader extension; the reader-less
// stubTracker satisfies only the tracker (proving the split is real).
var (
	_ ports.TenantUsageTracker = (*readerTracker)(nil)
	_ ports.TenantUsageReader  = (*readerTracker)(nil)
	_ ports.TenantUsageTracker = (*stubTracker)(nil)
)

func (r *readerTracker) IncrementMessages(context.Context, string, int64) error { return nil }

func (r *readerTracker) IncrementInFlight(_ context.Context, _ string, delta int64) error {
	r.mu.Lock()
	r.inFlightDeltas = append(r.inFlightDeltas, delta)
	r.mu.Unlock()
	return nil
}

func (r *readerTracker) Usage(_ context.Context, _ string) (ports.TenantUsage, error) {
	r.usageCalls.Add(1)
	if r.usageErr != nil {
		return ports.TenantUsage{}, r.usageErr
	}
	return r.usage, nil
}

func (r *readerTracker) usageCallCount() int64 { return r.usageCalls.Load() }

func (r *readerTracker) deltas() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int64, len(r.inFlightDeltas))
	copy(out, r.inFlightDeltas)
	return out
}

// TestProcess_QuotaInFlightCeiling_RejectsTransient pins the reject path: when
// the reader reports InFlight == MaxInFlight the delivery is refused
// transiently, next is never called, and the reject metric carries
// reason=quota_inflight. In-flight is NOT incremented on rejection.
func TestProcess_QuotaInFlightCeiling_RejectsTransient(t *testing.T) {
	const ceiling = int64(5)
	v := &stubValidator{info: ports.TenantInfo{ID: "acme", Active: true, MaxInFlight: ceiling}}
	tracker := &readerTracker{usage: ports.TenantUsage{InFlight: ceiling}}
	metrics := &ports.RecordingExporter{}
	p := mustNew(t, Config{}, WithValidator(v), WithUsageTracker(tracker), WithMetrics(metrics))
	env := envelope("acme", 0)

	nextCalled := false
	err := p.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		nextCalled = true
		return nil
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrTenantQuotaExceeded)
	assert.True(t, shared.IsRecoverableError(err), "quota rejection must be transient (retry-driven)")
	assert.False(t, nextCalled, "next must NOT be called when at the in-flight ceiling")

	assert.Equal(t, int64(1), tracker.usageCallCount(), "reader must be consulted exactly once")
	assert.Empty(t, tracker.deltas(), "in-flight must not be incremented on rejection")

	entries := metrics.FindEntries(metricTenantRejects)
	require.Len(t, entries, 1)
	assert.True(t, hasTag(entries[0].Tags, "reason", "quota_inflight"),
		"reject metric must be tagged reason=quota_inflight")
}

// TestProcess_QuotaBelowCeiling_Passes pins the boundary just under the
// ceiling: InFlight == ceiling-1 passes, next runs, and in-flight is
// incremented (+1).
func TestProcess_QuotaBelowCeiling_Passes(t *testing.T) {
	const ceiling = int64(5)
	v := &stubValidator{info: ports.TenantInfo{ID: "acme", Active: true, MaxInFlight: ceiling}}
	tracker := &readerTracker{usage: ports.TenantUsage{InFlight: ceiling - 1}}
	p := mustNew(t, Config{}, WithValidator(v), WithUsageTracker(tracker))
	env := envelope("acme", 0)

	nextCalled := false
	err := p.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		nextCalled = true
		return nil
	})

	require.NoError(t, err)
	assert.True(t, nextCalled, "next must be called below the ceiling")
	assert.Equal(t, int64(1), tracker.usageCallCount())
	assert.Contains(t, tracker.deltas(), int64(1), "in-flight must be incremented (+1) below the ceiling")
}

// TestProcess_QuotaZeroCeiling_Unlimited pins the zero-means-off convention:
// with MaxInFlight == 0 the reader is never consulted even though it is
// present, and the delivery passes regardless of current in-flight.
func TestProcess_QuotaZeroCeiling_Unlimited(t *testing.T) {
	v := &stubValidator{info: ports.TenantInfo{ID: "acme", Active: true, MaxInFlight: 0}}
	tracker := &readerTracker{usage: ports.TenantUsage{InFlight: 1000}}
	p := mustNew(t, Config{}, WithValidator(v), WithUsageTracker(tracker))
	env := envelope("acme", 0)

	nextCalled := false
	err := p.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		nextCalled = true
		return nil
	})

	require.NoError(t, err)
	assert.True(t, nextCalled)
	assert.Equal(t, int64(0), tracker.usageCallCount(),
		"reader must NOT be consulted when MaxInFlight == 0 (unlimited)")
}

// TestProcess_TrackerWithoutReader_NoEnforcement pins the capability gate: an
// increment-only tracker (no TenantUsageReader) leaves p.reader nil, so a
// non-zero MaxInFlight is not enforced and the delivery passes.
func TestProcess_TrackerWithoutReader_NoEnforcement(t *testing.T) {
	v := &stubValidator{info: ports.TenantInfo{ID: "acme", Active: true, MaxInFlight: 1}}
	tracker := &stubTracker{} // increment-only: does NOT implement TenantUsageReader
	p := mustNew(t, Config{}, WithValidator(v), WithUsageTracker(tracker))
	env := envelope("acme", 0)

	assert.Nil(t, p.reader, "reader must be nil for an increment-only tracker")

	nextCalled := false
	err := p.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		nextCalled = true
		return nil
	})

	require.NoError(t, err)
	assert.True(t, nextCalled, "delivery must proceed when the tracker lacks the reader capability")
	assert.Equal(t, int64(1), tracker.messages.Load())
}

// TestProcess_NoValidator_ReaderNotConsulted pins the `validated` gate: with a
// reader-capable tracker but NO validator there is no TenantInfo (hence no
// MaxInFlight), so the reader must never be consulted and the delivery
// proceeds. Guards against a regression that drops the `validated` guard and
// reads a zero-value TenantInfo.
func TestProcess_NoValidator_ReaderNotConsulted(t *testing.T) {
	tracker := &readerTracker{usage: ports.TenantUsage{InFlight: 1000}}
	p := mustNew(t, Config{}, WithUsageTracker(tracker))
	env := envelope("acme", 0)

	require.NotNil(t, p.reader, "reader capability must be detected even without a validator")

	nextCalled := false
	err := p.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		nextCalled = true
		return nil
	})

	require.NoError(t, err)
	assert.True(t, nextCalled, "delivery must proceed with no validator")
	assert.Equal(t, int64(0), tracker.usageCallCount(),
		"reader must NOT be consulted without a validator (no TenantInfo/MaxInFlight)")
}

// TestProcess_ReaderError_FailsOpen pins the fail-open decision: when the
// usage read errors, enforcement is skipped (delivery proceeds, in-flight is
// incremented) but the error is surfaced via a tracker-error metric and a log.
func TestProcess_ReaderError_FailsOpen(t *testing.T) {
	const ceiling = int64(2)
	v := &stubValidator{info: ports.TenantInfo{ID: "acme", Active: true, MaxInFlight: ceiling}}
	tracker := &readerTracker{usageErr: errors.New("usage store outage")}
	metrics := &ports.RecordingExporter{}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := mustNew(t, Config{}, WithValidator(v), WithUsageTracker(tracker),
		WithMetrics(metrics), WithLogger(logger))
	env := envelope("acme", 0)

	nextCalled := false
	err := p.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		nextCalled = true
		return nil
	})

	require.NoError(t, err, "a reader error must fail open, not reject")
	assert.True(t, nextCalled, "delivery must proceed when the usage read errors")
	assert.Contains(t, tracker.deltas(), int64(1), "fail-open still increments in-flight")

	entries := metrics.FindEntries(metricTenantTrackerErrors)
	require.Len(t, entries, 1)
	assert.True(t, hasTag(entries[0].Tags, "op", "usage_read"),
		"fail-open must surface a tracker-error metric tagged op=usage_read")
	assert.Contains(t, logBuf.String(), "tenant usage tracker error")
}

// TestTenantUsageReader_CompileTime documents the capability split enforced by
// the package-level assertions above and guards the runtime type-assertion the
// processor relies on: a reader-less tracker must NOT assert as a reader.
func TestTenantUsageReader_CompileTime(t *testing.T) {
	var trackerOnly ports.TenantUsageTracker = &stubTracker{}
	_, isReader := trackerOnly.(ports.TenantUsageReader)
	assert.False(t, isReader, "increment-only stubTracker must NOT implement TenantUsageReader")

	var withReader ports.TenantUsageTracker = &readerTracker{}
	_, isReader = withReader.(ports.TenantUsageReader)
	assert.True(t, isReader, "readerTracker must implement TenantUsageReader")
}
