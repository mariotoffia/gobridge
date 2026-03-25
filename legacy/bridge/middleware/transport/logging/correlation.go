package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ============================================================================
// Context Keys
// ============================================================================

// correlationIDKeyType is the context key type for correlation IDs.
type correlationIDKeyType struct{}

// traceIDKeyType is the context key type for trace IDs.
type traceIDKeyType struct{}

// spanIDKeyType is the context key type for span IDs.
type spanIDKeyType struct{}

var (
	correlationIDKey = correlationIDKeyType{}
	traceIDKey       = traceIDKeyType{}
	spanIDKey        = spanIDKeyType{}
)

// ============================================================================
// Metadata and Header Keys
// ============================================================================

// CorrelationIDMetadataKey is the message metadata key for correlation IDs.
const CorrelationIDMetadataKey = "_correlationId"

// CorrelationIDHeaderKey is the standard header/attribute key for correlation IDs.
const CorrelationIDHeaderKey = "X-Correlation-ID"

// TraceIDMetadataKey is the message metadata key for trace IDs.
const TraceIDMetadataKey = "_traceId"

// TraceIDHeaderKey is the W3C TraceContext header key.
const TraceIDHeaderKey = "traceparent"

// SpanIDMetadataKey is the message metadata key for span IDs.
const SpanIDMetadataKey = "_spanId"

// RequestIDHeaderKey is an alternative header key used by some systems.
const RequestIDHeaderKey = "X-Request-ID"

// ============================================================================
// Correlation ID Functions
// ============================================================================

// GetCorrelationID extracts the correlation ID from the context.
// Returns empty string if not present.
func GetCorrelationID(ctx context.Context) string {
	if id, ok := ctx.Value(correlationIDKey).(string); ok {
		return id
	}
	return ""
}

// WithCorrelationID returns a new context with the correlation ID set.
func WithCorrelationID(ctx context.Context, correlationID string) context.Context {
	return context.WithValue(ctx, correlationIDKey, correlationID)
}

// GenerateCorrelationID creates a new random correlation ID.
func GenerateCorrelationID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp-based ID if random fails
		return "cid-" + hex.EncodeToString([]byte{0})
	}
	return hex.EncodeToString(b)
}

// ExtractOrGenerateCorrelationID extracts correlation ID from message metadata
// or generates a new one if not present.
func ExtractOrGenerateCorrelationID(ctx context.Context, msg *types.Message) (context.Context, string) {
	// First check context
	if id := GetCorrelationID(ctx); id != "" {
		return ctx, id
	}

	// Check message metadata
	if msg != nil && msg.Metadata != nil {
		if id, ok := msg.Metadata[CorrelationIDMetadataKey].(string); ok && id != "" {
			return WithCorrelationID(ctx, id), id
		}
	}

	// Generate new correlation ID
	id := GenerateCorrelationID()
	return WithCorrelationID(ctx, id), id
}

// InjectCorrelationID ensures the message has a correlation ID in its metadata.
func InjectCorrelationID(msg *types.Message, correlationID string) {
	if msg.Metadata == nil {
		msg.Metadata = make(map[string]any)
	}
	msg.Metadata[CorrelationIDMetadataKey] = correlationID
}

// ============================================================================
// Trace ID Functions (OpenTelemetry Integration)
// ============================================================================

// GetTraceID extracts the trace ID from the context.
// Returns empty string if not present.
func GetTraceID(ctx context.Context) string {
	if id, ok := ctx.Value(traceIDKey).(string); ok {
		return id
	}
	return ""
}

// WithTraceID returns a new context with the trace ID set.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey, traceID)
}

// GetSpanID extracts the span ID from the context.
// Returns empty string if not present.
func GetSpanID(ctx context.Context) string {
	if id, ok := ctx.Value(spanIDKey).(string); ok {
		return id
	}
	return ""
}

// WithSpanID returns a new context with the span ID set.
func WithSpanID(ctx context.Context, spanID string) context.Context {
	return context.WithValue(ctx, spanIDKey, spanID)
}

// GenerateTraceID creates a new random trace ID (32 hex characters).
func GenerateTraceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "0000000000000000" + GenerateCorrelationID()
	}
	return hex.EncodeToString(b)
}

// GenerateSpanID creates a new random span ID (16 hex characters).
func GenerateSpanID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b)
}

// ExtractOrGenerateTraceID extracts trace ID from message metadata
// or generates a new one if not present.
func ExtractOrGenerateTraceID(ctx context.Context, msg *types.Message) (context.Context, string) {
	// First check context
	if id := GetTraceID(ctx); id != "" {
		return ctx, id
	}

	// Check message metadata
	if msg != nil && msg.Metadata != nil {
		if id, ok := msg.Metadata[TraceIDMetadataKey].(string); ok && id != "" {
			return WithTraceID(ctx, id), id
		}
	}

	// Generate new trace ID
	id := GenerateTraceID()
	return WithTraceID(ctx, id), id
}

// InjectTraceID ensures the message has a trace ID in its metadata.
func InjectTraceID(msg *types.Message, traceID string) {
	if msg.Metadata == nil {
		msg.Metadata = make(map[string]any)
	}
	msg.Metadata[TraceIDMetadataKey] = traceID
}

// InjectSpanID ensures the message has a span ID in its metadata.
func InjectSpanID(msg *types.Message, spanID string) {
	if msg.Metadata == nil {
		msg.Metadata = make(map[string]any)
	}
	msg.Metadata[SpanIDMetadataKey] = spanID
}

// ============================================================================
// Combined Context Functions
// ============================================================================

// LogContext holds all correlation and tracing IDs for logging.
type LogContext struct {
	CorrelationID string
	TraceID       string
	SpanID        string
}

// GetLogContext extracts all logging context from the context.
func GetLogContext(ctx context.Context) LogContext {
	return LogContext{
		CorrelationID: GetCorrelationID(ctx),
		TraceID:       GetTraceID(ctx),
		SpanID:        GetSpanID(ctx),
	}
}

// WithLogContext returns a new context with all logging IDs set.
func WithLogContext(ctx context.Context, lc LogContext) context.Context {
	if lc.CorrelationID != "" {
		ctx = WithCorrelationID(ctx, lc.CorrelationID)
	}
	if lc.TraceID != "" {
		ctx = WithTraceID(ctx, lc.TraceID)
	}
	if lc.SpanID != "" {
		ctx = WithSpanID(ctx, lc.SpanID)
	}
	return ctx
}

// ExtractOrGenerateLogContext extracts or generates all logging IDs.
func ExtractOrGenerateLogContext(ctx context.Context, msg *types.Message) (context.Context, LogContext) {
	var lc LogContext

	ctx, lc.CorrelationID = ExtractOrGenerateCorrelationID(ctx, msg)
	ctx, lc.TraceID = ExtractOrGenerateTraceID(ctx, msg)
	lc.SpanID = GetSpanID(ctx)

	// Generate span ID if not present
	if lc.SpanID == "" {
		lc.SpanID = GenerateSpanID()
		ctx = WithSpanID(ctx, lc.SpanID)
	}

	return ctx, lc
}

// InjectLogContext ensures the message has all logging IDs in its metadata.
func InjectLogContext(msg *types.Message, lc LogContext) {
	if lc.CorrelationID != "" {
		InjectCorrelationID(msg, lc.CorrelationID)
	}
	if lc.TraceID != "" {
		InjectTraceID(msg, lc.TraceID)
	}
	if lc.SpanID != "" {
		InjectSpanID(msg, lc.SpanID)
	}
}
