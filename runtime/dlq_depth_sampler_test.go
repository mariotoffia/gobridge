package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dlqDepthStore is a full ports.DLQStore that ALSO implements the OPTIONAL
// ports.DLQDepthReporter capability, so the runtime's DLQ-depth sampler
// can read a standing backlog. It embeds the shared FakeDLQStore for the store
// methods and adds Depth.
type dlqDepthStore struct {
	*FakeDLQStore
	depth int
}

func (s *dlqDepthStore) Depth(context.Context) (int, error) { return s.depth, nil }

var (
	_ ports.DLQStore         = (*dlqDepthStore)(nil)
	_ ports.DLQDepthReporter = (*dlqDepthStore)(nil)
)

// / H-OBS DLQ-1: unlike OutboxDepth (sampled every drain cycle), the DLQ
// has no loop of its own, so the standing backlog is invisible after traffic
// stops. The runtime lifecycle must spawn a periodic sampler that emits
// shared.MetricDLQDepth on rt.clk cadence.
func TestRuntime_DLQDepthSampler_EmitsGaugePeriodically(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	metrics := &ports.RecordingExporter{}
	store := &dlqDepthStore{FakeDLQStore: NewFakeDLQStore(), depth: 10000}

	rt := goruntime.New(
		goruntime.WithClock(clk),
		goruntime.WithDLQStore(store),
		goruntime.WithMetrics(metrics),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	// The sampler probes the DLQ-depth capability, then blocks on
	// rt.clk.After(interval). Advancing the fake clock past the interval fires it.
	// Loop-advance inside Eventually avoids a scheduling race between the
	// goroutine registering its timer and the clock advance (NO real sleep — the
	// poll cadence is the Eventually tick, TESTS.md compliant). One minute exceeds
	// the 30s sample interval so a single advance is always enough once armed.
	require.Eventually(t, func() bool {
		clk.Advance(time.Minute)
		return len(metrics.FindEntries(shared.MetricDLQDepth)) > 0
	}, 2*time.Second, 5*time.Millisecond)

	entries := metrics.FindEntries(shared.MetricDLQDepth)
	require.NotEmpty(t, entries)
	assert.Equal(t, "gauge", entries[0].Kind)
	assert.Equal(t, float64(10000), entries[0].FValue)
	assert.Empty(t, entries[0].Tags, "MetricDLQDepth is the dimensionless fleet total")
}

// A DLQ store WITHOUT the optional depth capability must not spin a pointless
// ticker: the sampler probes once, finds no capability, and exits — so no
// DLQ-depth gauge is ever emitted.
func TestRuntime_DLQDepthSampler_NoCapability_NeverEmits(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	metrics := &ports.RecordingExporter{}
	store := NewFakeDLQStore() // full DLQStore, no DLQDepthReporter

	rt := goruntime.New(
		goruntime.WithClock(clk),
		goruntime.WithDLQStore(store),
		goruntime.WithMetrics(metrics),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	require.Never(t, func() bool {
		clk.Advance(time.Minute)
		return len(metrics.FindEntries(shared.MetricDLQDepth)) > 0
	}, 200*time.Millisecond, 10*time.Millisecond)
}
