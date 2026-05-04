package ports

import (
	"context"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

// MetricsExporter exports metrics to an external monitoring backend
// such as AWS CloudWatch or OpenTelemetry. Implementations must be
// safe for concurrent use from multiple goroutines.
type MetricsExporter interface {
	// Counter increments a counter metric by the given value.
	Counter(name string, value int64, tags ...domain.Tag)
	// Gauge sets the current value of a gauge metric.
	Gauge(name string, value float64, tags ...domain.Tag)
	// Histogram records a single observation into a distribution.
	Histogram(name string, value float64, tags ...domain.Tag)
	// Timer records a duration (stored as milliseconds).
	Timer(name string, duration time.Duration, tags ...domain.Tag)
	// Flush sends all buffered metrics to the backend.
	Flush(ctx context.Context) error
	// Close stops the exporter and flushes remaining metrics.
	Close(ctx context.Context) error
}

// NoopExporter is a MetricsExporter that discards all metrics.
// It is the default when no exporter is configured.
type NoopExporter struct{}

var _ MetricsExporter = (*NoopExporter)(nil)

func (n *NoopExporter) Counter(string, int64, ...domain.Tag)       {}
func (n *NoopExporter) Gauge(string, float64, ...domain.Tag)       {}
func (n *NoopExporter) Histogram(string, float64, ...domain.Tag)   {}
func (n *NoopExporter) Timer(string, time.Duration, ...domain.Tag) {}
func (n *NoopExporter) Flush(context.Context) error                { return nil }
func (n *NoopExporter) Close(context.Context) error                { return nil }

// RecordingExporter is a MetricsExporter that records all emitted
// metrics in memory. It is safe for concurrent use and intended
// for testing only.
type RecordingExporter struct {
	mu      sync.Mutex
	entries []MetricEntry
}

// MetricEntry represents a single recorded metric emission.
type MetricEntry struct {
	Kind     string // "counter", "gauge", "histogram", "timer"
	Name     string
	IValue   int64
	FValue   float64
	Duration time.Duration
	Tags     []domain.Tag
}

var _ MetricsExporter = (*RecordingExporter)(nil)

func (r *RecordingExporter) Counter(name string, value int64, tags ...domain.Tag) {
	r.mu.Lock()
	r.entries = append(r.entries, MetricEntry{Kind: "counter", Name: name, IValue: value, Tags: tags})
	r.mu.Unlock()
}

func (r *RecordingExporter) Gauge(name string, value float64, tags ...domain.Tag) {
	r.mu.Lock()
	r.entries = append(r.entries, MetricEntry{Kind: "gauge", Name: name, FValue: value, Tags: tags})
	r.mu.Unlock()
}

func (r *RecordingExporter) Histogram(name string, value float64, tags ...domain.Tag) {
	r.mu.Lock()
	r.entries = append(r.entries, MetricEntry{Kind: "histogram", Name: name, FValue: value, Tags: tags})
	r.mu.Unlock()
}

func (r *RecordingExporter) Timer(name string, d time.Duration, tags ...domain.Tag) {
	r.mu.Lock()
	r.entries = append(r.entries, MetricEntry{Kind: "timer", Name: name, Duration: d, Tags: tags})
	r.mu.Unlock()
}

func (r *RecordingExporter) Flush(context.Context) error { return nil }
func (r *RecordingExporter) Close(context.Context) error { return nil }

// Entries returns a snapshot of all recorded entries.
func (r *RecordingExporter) Entries() []MetricEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]MetricEntry, len(r.entries))
	copy(cp, r.entries)
	return cp
}

// FindEntries returns all entries matching the given metric name.
func (r *RecordingExporter) FindEntries(name string) []MetricEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []MetricEntry
	for _, e := range r.entries {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// Reset clears all recorded entries.
func (r *RecordingExporter) Reset() {
	r.mu.Lock()
	r.entries = nil
	r.mu.Unlock()
}
