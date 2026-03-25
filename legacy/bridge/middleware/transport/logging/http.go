package logging

import (
	"net/http"
	"strings"
)

// HTTPCorrelationMiddleware extracts correlation and trace IDs from HTTP headers
// and injects them into the request context. It also adds the IDs to the response headers.
func HTTPCorrelationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// Extract or generate correlation ID
		correlationID := extractCorrelationIDFromHeaders(r)
		if correlationID == "" {
			correlationID = GenerateCorrelationID()
		}
		ctx = WithCorrelationID(ctx, correlationID)

		// Extract or generate trace ID
		traceID, spanID := extractTraceContextFromHeaders(r)
		if traceID == "" {
			traceID = GenerateTraceID()
		}
		ctx = WithTraceID(ctx, traceID)

		if spanID == "" {
			spanID = GenerateSpanID()
		}
		ctx = WithSpanID(ctx, spanID)

		// Add IDs to response headers
		w.Header().Set(CorrelationIDHeaderKey, correlationID)
		w.Header().Set("X-Trace-ID", traceID)
		w.Header().Set("X-Span-ID", spanID)

		// Continue with the updated context
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// HTTPCorrelationHandler wraps an http.HandlerFunc with correlation ID propagation.
func HTTPCorrelationHandler(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		HTTPCorrelationMiddleware(http.HandlerFunc(handler)).ServeHTTP(w, r)
	}
}

// extractCorrelationIDFromHeaders extracts correlation ID from various header formats.
func extractCorrelationIDFromHeaders(r *http.Request) string {
	// Check X-Correlation-ID header
	if id := r.Header.Get(CorrelationIDHeaderKey); id != "" {
		return id
	}

	// Check X-Request-ID header (common alternative)
	if id := r.Header.Get(RequestIDHeaderKey); id != "" {
		return id
	}

	// Check lowercase variants
	if id := r.Header.Get("x-correlation-id"); id != "" {
		return id
	}

	if id := r.Header.Get("x-request-id"); id != "" {
		return id
	}

	return ""
}

// extractTraceContextFromHeaders extracts W3C TraceContext from headers.
// Returns (traceID, spanID).
// TraceContext format: "00-{trace-id}-{parent-id}-{flags}"
func extractTraceContextFromHeaders(r *http.Request) (string, string) {
	// Check traceparent header (W3C TraceContext)
	traceparent := r.Header.Get(TraceIDHeaderKey)
	if traceparent == "" {
		traceparent = r.Header.Get("Traceparent")
	}

	if traceparent != "" {
		// Parse W3C TraceContext format: "00-traceId-spanId-flags"
		parts := strings.Split(traceparent, "-")
		if len(parts) >= 3 {
			return parts[1], parts[2]
		}
	}

	// Check X-Trace-ID header (non-standard but common)
	traceID := r.Header.Get("X-Trace-ID")
	if traceID == "" {
		traceID = r.Header.Get("x-trace-id")
	}

	// Check X-Span-ID header
	spanID := r.Header.Get("X-Span-ID")
	if spanID == "" {
		spanID = r.Header.Get("x-span-id")
	}

	return traceID, spanID
}

// InjectTraceContextToRequest adds correlation and trace IDs to outgoing HTTP requests.
func InjectTraceContextToRequest(r *http.Request, lc LogContext) {
	if lc.CorrelationID != "" {
		r.Header.Set(CorrelationIDHeaderKey, lc.CorrelationID)
	}

	if lc.TraceID != "" && lc.SpanID != "" {
		// Use W3C TraceContext format
		traceparent := "00-" + lc.TraceID + "-" + lc.SpanID + "-01"
		r.Header.Set(TraceIDHeaderKey, traceparent)

		// Also set non-standard headers for compatibility
		r.Header.Set("X-Trace-ID", lc.TraceID)
		r.Header.Set("X-Span-ID", lc.SpanID)
	}
}

// InjectTraceContextToResponse adds correlation and trace IDs to HTTP responses.
func InjectTraceContextToResponse(w http.ResponseWriter, lc LogContext) {
	if lc.CorrelationID != "" {
		w.Header().Set(CorrelationIDHeaderKey, lc.CorrelationID)
	}
	if lc.TraceID != "" {
		w.Header().Set("X-Trace-ID", lc.TraceID)
	}
	if lc.SpanID != "" {
		w.Header().Set("X-Span-ID", lc.SpanID)
	}
}

// ============================================================================
// HTTP Client Helpers
// ============================================================================

// RoundTripperWithCorrelation wraps an http.RoundTripper to inject correlation IDs.
type RoundTripperWithCorrelation struct {
	Base http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (rt *RoundTripperWithCorrelation) RoundTrip(r *http.Request) (*http.Response, error) {
	// Extract log context from request context
	lc := GetLogContext(r.Context())

	// Inject into request headers
	InjectTraceContextToRequest(r, lc)

	// Perform the request
	base := rt.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// NewHTTPClientWithCorrelation creates an HTTP client that propagates correlation IDs.
func NewHTTPClientWithCorrelation() *http.Client {
	return &http.Client{
		Transport: &RoundTripperWithCorrelation{
			Base: http.DefaultTransport,
		},
	}
}
