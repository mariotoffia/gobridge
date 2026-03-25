package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mariotoffia/gobridge/observability"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestServer() *Server {
	return New(testRuntime(), testConfig())
}

// captureHandler records the context values seen by the inner handler.
type captureHandler struct {
	correlationID string
	traceID       string
	spanID        string
}

func (c *captureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.correlationID = observability.CorrelationIDFromContext(r.Context())
	c.traceID = observability.TraceIDFromContext(r.Context())
	c.spanID = observability.SpanIDFromContext(r.Context())
	w.WriteHeader(http.StatusOK)
}

// Verifies X-Correlation-ID from the request is applied to context and echoed on the response.
func TestCorrelationMW_ExtractsExistingCorrelationID(t *testing.T) {
	s := newTestServer()
	cap := &captureHandler{}
	handler := s.correlationMW(cap)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Correlation-ID", "incoming-corr-id")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, "incoming-corr-id", cap.correlationID)
	assert.Equal(t, "incoming-corr-id", rec.Header().Get("X-Correlation-ID"))
}

// Verifies correlation ID falls back to X-Request-ID when X-Correlation-ID is absent.
func TestCorrelationMW_FallsBackToXRequestID(t *testing.T) {
	s := newTestServer()
	cap := &captureHandler{}
	handler := s.correlationMW(cap)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "request-id-fallback")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, "request-id-fallback", cap.correlationID)
	assert.Equal(t, "request-id-fallback", rec.Header().Get("X-Correlation-ID"))
}

// Verifies X-Correlation-ID takes precedence over X-Request-ID when both are present.
func TestCorrelationMW_PrefersCorrelationIDOverRequestID(t *testing.T) {
	s := newTestServer()
	cap := &captureHandler{}
	handler := s.correlationMW(cap)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Correlation-ID", "corr-wins")
	req.Header.Set("X-Request-ID", "request-loses")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, "corr-wins", cap.correlationID)
}

// Verifies a 32-character hex correlation ID is generated and returned when no incoming ID headers exist.
func TestCorrelationMW_GeneratesCorrelationIDWhenMissing(t *testing.T) {
	s := newTestServer()
	cap := &captureHandler{}
	handler := s.correlationMW(cap)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.NotEmpty(t, cap.correlationID)
	assert.Len(t, cap.correlationID, 32, "generated correlation ID should be 32 hex chars")
	assert.Equal(t, cap.correlationID, rec.Header().Get("X-Correlation-ID"))
}

// Verifies a valid W3C traceparent populates trace and span IDs in context and response headers.
func TestCorrelationMW_ParsesValidTraceparent(t *testing.T) {
	s := newTestServer()
	cap := &captureHandler{}
	handler := s.correlationMW(cap)

	traceID := "0af7651916cd43dd8448eb211c80319c"
	spanID := "b7ad6b7169203331"

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Traceparent", "00-"+traceID+"-"+spanID+"-01")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, traceID, cap.traceID)
	assert.Equal(t, spanID, cap.spanID)
	assert.Equal(t, traceID, rec.Header().Get("X-Trace-ID"))
	assert.Equal(t, spanID, rec.Header().Get("X-Span-ID"))
}

// Verifies an invalid traceparent is ignored and new trace/span IDs are generated.
func TestCorrelationMW_InvalidTraceparentIgnored(t *testing.T) {
	s := newTestServer()
	cap := &captureHandler{}
	handler := s.correlationMW(cap)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Traceparent", "not-a-valid-traceparent")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.NotEmpty(t, cap.traceID, "should generate trace ID when traceparent is invalid")
	assert.Len(t, cap.traceID, 32)
	require.NotEmpty(t, cap.spanID, "should generate span ID when traceparent is invalid")
	assert.Len(t, cap.spanID, 16)
}

// Verifies X-Trace-ID and X-Span-ID are used when traceparent is not provided.
func TestCorrelationMW_FallsBackToXTraceIDAndXSpanID(t *testing.T) {
	s := newTestServer()
	cap := &captureHandler{}
	handler := s.correlationMW(cap)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Trace-ID", "legacy-trace-id")
	req.Header.Set("X-Span-ID", "legacy-span-id")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, "legacy-trace-id", cap.traceID)
	assert.Equal(t, "legacy-span-id", cap.spanID)
	assert.Equal(t, "legacy-trace-id", rec.Header().Get("X-Trace-ID"))
	assert.Equal(t, "legacy-span-id", rec.Header().Get("X-Span-ID"))
}

// Verifies traceparent wins over legacy X-Trace-ID and X-Span-ID when all are sent.
func TestCorrelationMW_TraceparentOverridesLegacyHeaders(t *testing.T) {
	s := newTestServer()
	cap := &captureHandler{}
	handler := s.correlationMW(cap)

	traceID := "0af7651916cd43dd8448eb211c80319c"
	spanID := "b7ad6b7169203331"

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Traceparent", "00-"+traceID+"-"+spanID+"-01")
	req.Header.Set("X-Trace-ID", "legacy-trace-loses")
	req.Header.Set("X-Span-ID", "legacy-span-loses")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, traceID, cap.traceID)
	assert.Equal(t, spanID, cap.spanID)
}

// Verifies trace and span IDs are generated and reflected on the response when no tracing headers are present.
func TestCorrelationMW_GeneratesTraceAndSpanIDsWhenMissing(t *testing.T) {
	s := newTestServer()
	cap := &captureHandler{}
	handler := s.correlationMW(cap)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.NotEmpty(t, cap.traceID)
	assert.Len(t, cap.traceID, 32, "generated trace ID should be 32 hex chars")
	require.NotEmpty(t, cap.spanID)
	assert.Len(t, cap.spanID, 16, "generated span ID should be 16 hex chars")

	assert.Equal(t, cap.traceID, rec.Header().Get("X-Trace-ID"))
	assert.Equal(t, cap.spanID, rec.Header().Get("X-Span-ID"))
}

// Verifies correlation, trace, and span IDs from headers all appear together in the handler context.
func TestCorrelationMW_AllIDsInContext(t *testing.T) {
	s := newTestServer()
	cap := &captureHandler{}
	handler := s.correlationMW(cap)

	traceID := "0af7651916cd43dd8448eb211c80319c"
	spanID := "b7ad6b7169203331"

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Correlation-ID", "ctx-corr")
	req.Header.Set("Traceparent", "00-"+traceID+"-"+spanID+"-00")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, "ctx-corr", cap.correlationID)
	assert.Equal(t, traceID, cap.traceID)
	assert.Equal(t, spanID, cap.spanID)
}

// Verifies generated middleware always sets X-Correlation-ID, X-Trace-ID, and X-Span-ID on the response.
func TestCorrelationMW_ResponseHeadersPresent(t *testing.T) {
	s := newTestServer()
	cap := &captureHandler{}
	handler := s.correlationMW(cap)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.NotEmpty(t, rec.Header().Get("X-Correlation-ID"))
	assert.NotEmpty(t, rec.Header().Get("X-Trace-ID"))
	assert.NotEmpty(t, rec.Header().Get("X-Span-ID"))
}

// Verifies many consecutive requests each get a distinct generated correlation ID.
func TestCorrelationMW_GeneratedIDsAreUnique(t *testing.T) {
	s := newTestServer()

	ids := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		cap := &captureHandler{}
		handler := s.correlationMW(cap)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		_, exists := ids[cap.correlationID]
		assert.False(t, exists, "duplicate correlation ID on iteration %d", i)
		ids[cap.correlationID] = struct{}{}
	}
}

// Verifies only X-Span-ID present still yields a generated trace ID while preserving the span ID.
func TestCorrelationMW_OnlySpanIDFromLegacy(t *testing.T) {
	s := newTestServer()
	cap := &captureHandler{}
	handler := s.correlationMW(cap)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Span-ID", "span-only")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Len(t, cap.traceID, 32, "trace ID should be generated")
	assert.Equal(t, "span-only", cap.spanID)
}

// Verifies Server.wrap applies correlation middleware so IDs propagate like correlationMW alone.
func TestCorrelationMW_IntegrationWithWrap(t *testing.T) {
	s := newTestServer()
	cap := &captureHandler{}
	handler := s.wrap(cap)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Correlation-ID", "wrap-corr")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, "wrap-corr", cap.correlationID)
	assert.Equal(t, "wrap-corr", rec.Header().Get("X-Correlation-ID"))
	assert.NotEmpty(t, cap.traceID)
	assert.NotEmpty(t, cap.spanID)
}
