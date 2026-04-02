//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// ---------------------------------------------------------------------------
// latencyRecorder — processor measuring next() call duration
// ---------------------------------------------------------------------------

// latencyRecorder is a processor that records how long the downstream chain
// (next) takes for each successfully processed message. Use percentile()
// after the test to compute P50/P95/P99 latencies.
type latencyRecorder struct {
	mu        sync.Mutex
	latencies []time.Duration
}

func (p *latencyRecorder) Name() string { return "latency-recorder" }

func (p *latencyRecorder) Process(
	ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc,
) error {
	start := time.Now()
	err := next(ctx, env)
	if err == nil {
		p.mu.Lock()
		p.latencies = append(p.latencies, time.Since(start))
		p.mu.Unlock()
	}
	return err
}

func (p *latencyRecorder) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.latencies)
}

func (p *latencyRecorder) percentile(pct float64) time.Duration {
	p.mu.Lock()
	cp := make([]time.Duration, len(p.latencies))
	copy(cp, p.latencies)
	p.mu.Unlock()

	if len(cp) == 0 {
		return 0
	}
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })

	idx := int(float64(len(cp)) * pct)
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

// ---------------------------------------------------------------------------
// heapSampler — periodic runtime.ReadMemStats sampling
// ---------------------------------------------------------------------------

// heapSampler runs a background goroutine that periodically records
// HeapAlloc. Call stop() before reading results.
type heapSampler struct {
	mu      sync.Mutex
	samples []uint64
	initial uint64
	cancel  context.CancelFunc
	done    chan struct{}
}

func newHeapSampler(interval time.Duration) *heapSampler {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	ctx, cancel := context.WithCancel(context.Background())
	h := &heapSampler{
		initial: ms.HeapAlloc,
		cancel:  cancel,
		done:    make(chan struct{}),
	}

	go func() {
		defer close(h.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				h.mu.Lock()
				h.samples = append(h.samples, m.HeapAlloc)
				h.mu.Unlock()
			}
		}
	}()

	return h
}

func (h *heapSampler) stop() {
	h.cancel()
	<-h.done
}

func (h *heapSampler) initialHeap() uint64 { return h.initial }

func (h *heapSampler) maxHeap() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	mx := h.initial
	for _, s := range h.samples {
		if s > mx {
			mx = s
		}
	}
	return mx
}

func (h *heapSampler) finalHeap() uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// ---------------------------------------------------------------------------
// tenantSlowProcessor — adds delay for a specific tenant
// ---------------------------------------------------------------------------

// tenantSlowProcessor adds a configurable delay for messages whose
// "tenant_id" header matches slowTenant. All other messages pass through
// immediately. This simulates one degraded tenant target.
type tenantSlowProcessor struct {
	delay      time.Duration
	slowTenant string
}

func (p *tenantSlowProcessor) Name() string { return "tenant-slow" }

func (p *tenantSlowProcessor) Process(
	ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc,
) error {
	if tid, ok := domain.GetHeaderString(env.Headers, "tenant_id"); ok && tid == p.slowTenant {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return next(ctx, env)
}

// ---------------------------------------------------------------------------
// benchmarkReport — structured performance summary
// ---------------------------------------------------------------------------

// benchmarkReport aggregates high-level benchmark results and prints a
// formatted report including latency percentiles from a RecordingExporter.
type benchmarkReport struct {
	TestName      string
	MsgsSent      int
	MsgsDelivered int
	MsgsDLQ       int
	Duration      time.Duration
	HeapInitial   uint64
	HeapPeak      uint64
	HeapFinal     uint64
}

func (r *benchmarkReport) throughput() float64 {
	if r.Duration == 0 {
		return 0
	}
	return float64(r.MsgsDelivered) / r.Duration.Seconds()
}

// logReport prints a structured benchmark report to t.Log, including
// stage latency breakdowns (P50/P95/P99) and counter totals extracted
// from the RecordingExporter.
func (r *benchmarkReport) logReport(t *testing.T, exporter *ports.RecordingExporter) {
	t.Helper()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\n=== BENCHMARK REPORT: %s ===\n", r.TestName))
	sb.WriteString(fmt.Sprintf("Messages:    %d sent, %d delivered, %d DLQ\n",
		r.MsgsSent, r.MsgsDelivered, r.MsgsDLQ))
	sb.WriteString(fmt.Sprintf("Throughput:  %.0f msgs/sec\n", r.throughput()))
	sb.WriteString(fmt.Sprintf("Duration:    %s\n", r.Duration.Round(time.Millisecond)))

	// Stage latency breakdown.
	stages := []string{
		domain.MetricSQSPollLatency,
		domain.MetricSQSReceiveLatency,
		domain.MetricMQTTPublishLatency,
		domain.MetricOutboxPersistLatency,
		domain.MetricOutboxDrainLatency,
		domain.MetricDeliveryE2ELatency,
		domain.MetricAckLatency,
		domain.MetricSQSDeleteLatency,
	}

	type latRow struct {
		name       string
		p50, p95, p99 time.Duration
		count      int
	}
	var rows []latRow
	for _, stage := range stages {
		durations := timerDurations(exporter, stage)
		if len(durations) == 0 {
			continue
		}
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		rows = append(rows, latRow{
			name:  stage,
			p50:   pctDuration(durations, 50),
			p95:   pctDuration(durations, 95),
			p99:   pctDuration(durations, 99),
			count: len(durations),
		})
	}

	if len(rows) > 0 {
		sb.WriteString("\nStage Latency Breakdown:\n")
		sb.WriteString(fmt.Sprintf("  %-30s| %8s | %8s | %8s | %s\n",
			"Stage", "P50", "P95", "P99", "Count"))
		sb.WriteString(fmt.Sprintf("  %s|%s|%s|%s|%s\n",
			strings.Repeat("-", 30),
			strings.Repeat("-", 10),
			strings.Repeat("-", 10),
			strings.Repeat("-", 10),
			strings.Repeat("-", 8)))
		for _, row := range rows {
			sb.WriteString(fmt.Sprintf("  %-30s| %8s | %8s | %8s | %d\n",
				row.name,
				row.p50.Round(time.Millisecond),
				row.p95.Round(time.Millisecond),
				row.p99.Round(time.Millisecond),
				row.count))
		}
	}

	// Counter totals.
	counterNames := []string{
		domain.MetricMessagesReceived,
		domain.MetricMessagesSent,
		domain.MetricMessagesDropped,
		domain.MetricRouteErrors,
	}
	sb.WriteString("\nCounters:\n")
	for _, cn := range counterNames {
		total := counterTotal(exporter, cn)
		sb.WriteString(fmt.Sprintf("  %-24s %d\n", cn+":", total))
	}

	// Memory stats.
	if r.HeapInitial > 0 {
		sb.WriteString("\nMemory:\n")
		sb.WriteString(fmt.Sprintf("  Heap Initial: %d MB\n", r.HeapInitial/(1<<20)))
		sb.WriteString(fmt.Sprintf("  Heap Peak:    %d MB\n", r.HeapPeak/(1<<20)))
		sb.WriteString(fmt.Sprintf("  Heap Final:   %d MB\n", r.HeapFinal/(1<<20)))
	}

	sb.WriteString("=== END REPORT ===\n")
	t.Log(sb.String())
}

// timerDurations extracts Duration values from timer entries for a metric.
func timerDurations(exp *ports.RecordingExporter, name string) []time.Duration {
	entries := exp.FindEntries(name)
	var out []time.Duration
	for _, e := range entries {
		if e.Kind == "timer" {
			out = append(out, e.Duration)
		}
	}
	return out
}

// pctDuration computes a percentile from a sorted duration slice.
func pctDuration(sorted []time.Duration, pct int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := len(sorted) * pct / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// counterTotal sums IValue from all counter entries matching the name.
func counterTotal(exp *ports.RecordingExporter, name string) int64 {
	entries := exp.FindEntries(name)
	var total int64
	for _, e := range entries {
		if e.Kind == "counter" {
			total += e.IValue
		}
	}
	return total
}
