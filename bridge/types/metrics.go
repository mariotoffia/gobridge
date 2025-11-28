package types

import (
	"context"
	"time"
)

// ============================================================================
// Standard Metric Names
// ============================================================================

const (
	// Message metrics
	MetricMessagesReceived  = "bridge.messages.received"
	MetricMessagesPublished = "bridge.messages.published"
	MetricMessagesFailed    = "bridge.messages.failed"
	MetricMessagesRetried   = "bridge.messages.retried"
	MetricMessagesDropped   = "bridge.messages.dropped"
	MetricMessagesAcked     = "bridge.messages.acked"
	MetricMessagesNacked    = "bridge.messages.nacked"

	// Latency metrics
	MetricLatencyProcessMs = "bridge.latency.process.ms"
	MetricLatencyPublishMs = "bridge.latency.publish.ms"
	MetricLatencyE2EMs     = "bridge.latency.e2e.ms"

	// Queue/buffer metrics
	MetricQueueDepth   = "bridge.queue.depth"
	MetricInFlight     = "bridge.inflight"
	MetricBufferSize   = "bridge.buffer.size"
	MetricBackpressure = "bridge.backpressure"

	// Connection metrics
	MetricConnectionsActive    = "bridge.connections.active"
	MetricConnectionsTotal     = "bridge.connections.total"
	MetricConnectionErrors     = "bridge.connections.errors"
	MetricConnectionReconnects = "bridge.connections.reconnects"

	// Pipeline metrics
	MetricPipelinesActive = "bridge.pipelines.active"
	MetricPipelineErrors  = "bridge.pipelines.errors"

	// Drain metrics
	MetricDrainProgress = "bridge.drain.progress"
	MetricDrainRemaining = "bridge.drain.remaining"

	// Cluster metrics
	MetricClusterMembers  = "bridge.cluster.members"
	MetricClusterIsLeader = "bridge.cluster.is_leader"
)

// ============================================================================
// Metrics Interface (Push-based for Fargate/Serverless)
// ============================================================================

// MetricsExporter pushes metrics to external systems.
// This interface is designed for push-based metrics in serverless environments
// like AWS Fargate where pull-based scraping (like Prometheus) isn't practical.
//
// Implementations may include:
//   - AWS CloudWatch
//   - Datadog
//   - OTEL Collector
//   - StatsD
//   - In-memory (for testing)
type MetricsExporter interface {
	// Counter increments a counter metric by the given value.
	// Counters are cumulative and only increase.
	Counter(name string, value int64, tags ...Tag)

	// Gauge sets the current value of a gauge metric.
	// Gauges can go up or down and represent a point-in-time value.
	Gauge(name string, value float64, tags ...Tag)

	// Histogram records a value for distribution/histogram analysis.
	// Used for latencies, sizes, etc.
	Histogram(name string, value float64, tags ...Tag)

	// Timer records a duration. This is a convenience wrapper around Histogram
	// that converts the duration to the appropriate unit.
	Timer(name string, duration time.Duration, tags ...Tag)

	// Flush sends all buffered metrics to the backend.
	// This MUST be called before shutdown to ensure metrics are not lost.
	// In serverless, call this at the end of each request/batch.
	Flush(ctx context.Context) error

	// Close closes the exporter and releases resources.
	// Calls Flush internally before closing.
	Close(ctx context.Context) error
}

// MetricsExporterFactory creates MetricsExporter instances.
type MetricsExporterFactory interface {
	// Create creates a new MetricsExporter with the given options.
	Create(ctx context.Context, opts MetricsExporterOptions) (MetricsExporter, error)
}

// MetricsExporterOptions configures a MetricsExporter.
type MetricsExporterOptions struct {
	// Namespace is a prefix for all metric names (e.g., "myapp").
	Namespace string `json:"namespace,omitempty"`
	// DefaultTags are added to all metrics.
	DefaultTags []Tag `json:"defaultTags,omitempty"`
	// FlushInterval is how often to flush buffered metrics.
	FlushInterval time.Duration `json:"flushInterval,omitempty"`
	// BufferSize is the maximum number of metrics to buffer before flushing.
	BufferSize int `json:"bufferSize,omitempty"`
	// Backend-specific configuration
	Backend map[string]any `json:"backend,omitempty"`
}

// ============================================================================
// Metrics Middleware
// ============================================================================

// MetricsMiddleware wraps publishers and subscribers with automatic metrics.
type MetricsMiddleware interface {
	// WrapPublisher wraps a Publisher with metrics collection.
	WrapPublisher(p Publisher) Publisher

	// WrapSubscriber wraps a Subscriber with metrics collection.
	WrapSubscriber(s Subscriber) Subscriber

	// WrapMiddleware wraps a Middleware with metrics collection.
	WrapMiddleware(m Middleware) Middleware
}

// NewMetricsMiddleware creates a MetricsMiddleware using the given exporter.
func NewMetricsMiddleware(exporter MetricsExporter) MetricsMiddleware {
	return &metricsMiddleware{exporter: exporter}
}

type metricsMiddleware struct {
	exporter MetricsExporter
}

func (m *metricsMiddleware) WrapPublisher(p Publisher) Publisher {
	return PublisherAdapter(func(ctx context.Context, topic string, msg Message) error {
		start := time.Now()

		err := p.Publish(ctx, topic, msg)

		duration := time.Since(start)
		tags := []Tag{{Key: "topic", Value: topic}}

		if err != nil {
			m.exporter.Counter(MetricMessagesFailed, 1, tags...)
			return err
		}

		m.exporter.Counter(MetricMessagesPublished, 1, tags...)
		m.exporter.Timer(MetricLatencyPublishMs, duration, tags...)
		return nil
	})
}

func (m *metricsMiddleware) WrapSubscriber(s Subscriber) Subscriber {
	return SubscriberAdapter(func(ctx context.Context, topic string, msg Message) error {
		start := time.Now()

		m.exporter.Counter(MetricMessagesReceived, 1, Tag{Key: "topic", Value: topic})

		err := s.Process(ctx, topic, msg)

		duration := time.Since(start)
		tags := []Tag{{Key: "topic", Value: topic}}

		if err != nil {
			m.exporter.Counter(MetricMessagesFailed, 1, tags...)
		} else {
			m.exporter.Timer(MetricLatencyProcessMs, duration, tags...)
		}

		return err
	})
}

func (m *metricsMiddleware) WrapMiddleware(mw Middleware) Middleware {
	return NewMiddlewareAdapter(mw.Name()+".metrics", func(ctx context.Context, msg *Message, next MiddlewareFunc) error {
		start := time.Now()

		err := mw.Process(ctx, msg, next)

		duration := time.Since(start)
		tags := []Tag{{Key: "middleware", Value: mw.Name()}}

		m.exporter.Timer("bridge.middleware.duration.ms", duration, tags...)
		if err != nil {
			m.exporter.Counter("bridge.middleware.errors", 1, tags...)
		}

		return err
	})
}

// ============================================================================
// No-op Metrics Exporter (for testing/disabled metrics)
// ============================================================================

// NoopMetricsExporter is a MetricsExporter that does nothing.
// Useful for testing or when metrics are disabled.
type NoopMetricsExporter struct{}

func (n *NoopMetricsExporter) Counter(name string, value int64, tags ...Tag)        {}
func (n *NoopMetricsExporter) Gauge(name string, value float64, tags ...Tag)        {}
func (n *NoopMetricsExporter) Histogram(name string, value float64, tags ...Tag)    {}
func (n *NoopMetricsExporter) Timer(name string, duration time.Duration, tags ...Tag) {}
func (n *NoopMetricsExporter) Flush(ctx context.Context) error                      { return nil }
func (n *NoopMetricsExporter) Close(ctx context.Context) error                      { return nil }

// Ensure NoopMetricsExporter implements MetricsExporter
var _ MetricsExporter = (*NoopMetricsExporter)(nil)

