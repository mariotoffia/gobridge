//go:build longrunning

package longrunning_test

import (
	"context"
	"runtime"
	"sort"
	"sync"
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
