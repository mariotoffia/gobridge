package types

import (
	"context"
	"time"
)

// ============================================================================
// Metrics Types
// ============================================================================

// BridgeMetrics contains aggregated metrics for the entire bridge.
type BridgeMetrics struct {
	// BridgeID is the identifier of this bridge instance.
	BridgeID string `json:"bridgeId"`
	// Uptime is how long the bridge has been running.
	Uptime time.Duration `json:"uptime"`
	// StartedAt is when the bridge started.
	StartedAt time.Time `json:"startedAt"`
	// Pipelines contains metrics for each pipeline.
	Pipelines map[string]PipelineMetrics `json:"pipelines,omitempty"`
	// Connections contains metrics for each connection.
	Connections map[string]ConnectionMetrics `json:"connections,omitempty"`
	// Retry contains retry system metrics.
	Retry RetryMetrics `json:"retry"`
	// FlowControl contains flow control metrics.
	FlowControl FlowControlMetrics `json:"flowControl"`
}

// PipelineMetrics contains metrics for a single pipeline.
type PipelineMetrics struct {
	// ID is the pipeline identifier.
	ID string `json:"id"`
	// Stats contains the pipeline statistics.
	Stats PipelineStats `json:"stats"`
	// Status is the current pipeline status.
	Status string `json:"status"`
	// LastMessageAt is when the last message was processed.
	LastMessageAt time.Time `json:"lastMessageAt,omitempty"`
	// AverageLatency is the average message processing latency.
	AverageLatency time.Duration `json:"averageLatency,omitempty"`
}

// ConnectionMetrics contains metrics for a single connection.
type ConnectionMetrics struct {
	// ID is the connection identifier.
	ID string `json:"id"`
	// TransportType is the transport type (MQTT, SQS, etc.).
	TransportType TransportType `json:"transportType"`
	// Status is the current connection status.
	Status string `json:"status"`
	// ConnectedAt is when the connection was established.
	ConnectedAt time.Time `json:"connectedAt,omitempty"`
	// ReconnectCount is the number of reconnections.
	ReconnectCount int64 `json:"reconnectCount"`
	// LastError is the last error message if any.
	LastError string `json:"lastError,omitempty"`
}

// RetryMetrics contains metrics for the retry system.
type RetryMetrics struct {
	// TransportRetryAttempts is the total transport retry attempts.
	TransportRetryAttempts int64 `json:"transportRetryAttempts"`
	// TransportRetrySuccesses is the number of successful transport retries.
	TransportRetrySuccesses int64 `json:"transportRetrySuccesses"`
	// MessageRetryAttempts is the total message retry attempts.
	MessageRetryAttempts int64 `json:"messageRetryAttempts"`
	// MessageRetrySuccesses is the number of successful message retries.
	MessageRetrySuccesses int64 `json:"messageRetrySuccesses"`
	// DLQMessages is the number of messages in DLQ.
	DLQMessages int64 `json:"dlqMessages"`
	// ExpiredMessages is the number of messages dropped due to TTL.
	ExpiredMessages int64 `json:"expiredMessages"`
}

// FlowControlMetrics contains flow control metrics.
type FlowControlMetrics struct {
	// CurrentInFlight is the current number of in-flight messages.
	CurrentInFlight int64 `json:"currentInFlight"`
	// MaxInFlight is the configured maximum.
	MaxInFlight int `json:"maxInFlight"`
	// BackpressureEvents is the number of backpressure events.
	BackpressureEvents int64 `json:"backpressureEvents"`
}

// ============================================================================
// Metrics Collector Interface
// ============================================================================

// MetricsCollector collects and exports metrics.
type MetricsCollector interface {
	// RecordMessageReceived records a message received from a source.
	RecordMessageReceived(pipelineID string)
	// RecordMessageSent records a message sent to a target.
	RecordMessageSent(pipelineID string)
	// RecordMessageFailed records a failed message.
	RecordMessageFailed(pipelineID string, err error)
	// RecordRetry records a retry attempt.
	RecordRetry(pipelineID string, isTransport bool)
	// RecordLatency records message processing latency.
	RecordLatency(pipelineID string, duration time.Duration)
	// RecordBackpressure records a backpressure event.
	RecordBackpressure(pipelineID string)
}

// MetricsProvider provides access to collected metrics.
type MetricsProvider interface {
	// Metrics returns the current bridge metrics.
	Metrics(ctx context.Context) *BridgeMetrics
}

// ============================================================================
// Metrics Exporter Interface
// ============================================================================

// MetricsExporter exports metrics to external systems (CloudWatch, OTEL, etc.).
// This is a low-level interface for pushing metrics to backends.
type MetricsExporter interface {
	// Counter increments a counter metric.
	Counter(name string, value int64, tags ...Tag)
	// Gauge sets a gauge metric value.
	Gauge(name string, value float64, tags ...Tag)
	// Histogram records a histogram/distribution value.
	Histogram(name string, value float64, tags ...Tag)
	// Timer records a duration (typically as milliseconds).
	Timer(name string, duration time.Duration, tags ...Tag)
	// Flush sends all buffered metrics to the backend.
	Flush(ctx context.Context) error
	// Close stops the exporter and flushes remaining metrics.
	Close(ctx context.Context) error
}

// ============================================================================
// No-op Metrics Collector
// ============================================================================

// NoopMetricsCollector is a no-op implementation of MetricsCollector.
type NoopMetricsCollector struct{}

var _ MetricsCollector = (*NoopMetricsCollector)(nil)

func (n *NoopMetricsCollector) RecordMessageReceived(pipelineID string)          {}
func (n *NoopMetricsCollector) RecordMessageSent(pipelineID string)              {}
func (n *NoopMetricsCollector) RecordMessageFailed(pipelineID string, err error) {}
func (n *NoopMetricsCollector) RecordRetry(pipelineID string, isTransport bool)  {}
func (n *NoopMetricsCollector) RecordLatency(pipelineID string, d time.Duration) {}
func (n *NoopMetricsCollector) RecordBackpressure(pipelineID string)             {}
