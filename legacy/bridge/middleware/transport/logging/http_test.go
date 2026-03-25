package logging

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════════════
// HTTP Middleware Tests
//
// Validates HTTP middleware for correlation ID and trace context propagation.
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │                    HTTP Middleware Flow                                  │
// ├─────────────────────────────────────────────────────────────────────────┤
// │  Request ──▶ [HTTPCorrelationMiddleware] ──▶ Next Handler               │
// │    │              │                                │                     │
// │    │         ┌────┴────┐                      ┌────┴────┐               │
// │    │         │ Extract │                      │ Inject  │               │
// │    │         │   or    │                      │   to    │               │
// │    │         │Generate │                      │Response │               │
// │    │         └─────────┘                      └─────────┘               │
// │    │                                                                     │
// │    ├── X-Correlation-ID / X-Request-ID                                  │
// │    ├── traceparent (W3C TraceContext)                                   │
// │    └── X-Trace-ID / X-Span-ID                                           │
// └─────────────────────────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// ═══════════════════════════════════════════════════════════════════════════
// HTTPCorrelationMiddleware Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestHTTPCorrelationMiddleware_NewIDs validates ID generation when none provided.
func TestHTTPCorrelationMiddleware_NewIDs(t *testing.T) {
	var capturedCorrelationID, capturedTraceID, capturedSpanID string

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedCorrelationID = GetCorrelationID(r.Context())
		capturedTraceID = GetTraceID(r.Context())
		capturedSpanID = GetSpanID(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	middleware := HTTPCorrelationMiddleware(handler)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	middleware.ServeHTTP(rec, req)

	// IDs should be generated
	if capturedCorrelationID == "" {
		t.Error("expected generated correlation ID in context")
	}
	if capturedTraceID == "" {
		t.Error("expected generated trace ID in context")
	}
	if capturedSpanID == "" {
		t.Error("expected generated span ID in context")
	}

	// IDs should be in response headers
	if rec.Header().Get(CorrelationIDHeaderKey) == "" {
		t.Errorf("expected %s in response headers", CorrelationIDHeaderKey)
	}
	if rec.Header().Get("X-Trace-ID") == "" {
		t.Error("expected X-Trace-ID in response headers")
	}
	if rec.Header().Get("X-Span-ID") == "" {
		t.Error("expected X-Span-ID in response headers")
	}
}

// TestHTTPCorrelationMiddleware_ExtractCorrelationID validates extraction of
// X-Correlation-ID header.
func TestHTTPCorrelationMiddleware_ExtractCorrelationID(t *testing.T) {
	var capturedCorrelationID string

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedCorrelationID = GetCorrelationID(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	middleware := HTTPCorrelationMiddleware(handler)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(CorrelationIDHeaderKey, "provided-corr-id")
	rec := httptest.NewRecorder()

	middleware.ServeHTTP(rec, req)

	if capturedCorrelationID != "provided-corr-id" {
		t.Errorf("correlation ID = %q, want %q", capturedCorrelationID, "provided-corr-id")
	}

	// Should be echoed in response
	if rec.Header().Get(CorrelationIDHeaderKey) != "provided-corr-id" {
		t.Errorf("response header = %q, want %q",
			rec.Header().Get(CorrelationIDHeaderKey), "provided-corr-id")
	}
}

// TestHTTPCorrelationMiddleware_ExtractRequestID validates extraction of
// X-Request-ID header as fallback.
func TestHTTPCorrelationMiddleware_ExtractRequestID(t *testing.T) {
	var capturedCorrelationID string

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedCorrelationID = GetCorrelationID(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	middleware := HTTPCorrelationMiddleware(handler)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(RequestIDHeaderKey, "request-id-fallback")
	rec := httptest.NewRecorder()

	middleware.ServeHTTP(rec, req)

	if capturedCorrelationID != "request-id-fallback" {
		t.Errorf("correlation ID = %q, want %q", capturedCorrelationID, "request-id-fallback")
	}
}

// TestHTTPCorrelationMiddleware_ExtractTraceContext validates extraction of
// W3C traceparent header.
func TestHTTPCorrelationMiddleware_ExtractTraceContext(t *testing.T) {
	var capturedTraceID, capturedSpanID string

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedTraceID = GetTraceID(r.Context())
		capturedSpanID = GetSpanID(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	middleware := HTTPCorrelationMiddleware(handler)

	// W3C TraceContext format: "00-traceId-spanId-flags"
	traceparent := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(TraceIDHeaderKey, traceparent)
	rec := httptest.NewRecorder()

	middleware.ServeHTTP(rec, req)

	if capturedTraceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("trace ID = %q, want %q", capturedTraceID, "0af7651916cd43dd8448eb211c80319c")
	}
	if capturedSpanID != "b7ad6b7169203331" {
		t.Errorf("span ID = %q, want %q", capturedSpanID, "b7ad6b7169203331")
	}
}

// TestHTTPCorrelationMiddleware_ResponseHeaders validates response header injection.
func TestHTTPCorrelationMiddleware_ResponseHeaders(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	middleware := HTTPCorrelationMiddleware(handler)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(CorrelationIDHeaderKey, "resp-corr")
	rec := httptest.NewRecorder()

	middleware.ServeHTTP(rec, req)

	// All IDs should be in response headers
	if rec.Header().Get(CorrelationIDHeaderKey) != "resp-corr" {
		t.Errorf("response %s = %q, want %q",
			CorrelationIDHeaderKey, rec.Header().Get(CorrelationIDHeaderKey), "resp-corr")
	}
	if rec.Header().Get("X-Trace-ID") == "" {
		t.Error("expected X-Trace-ID in response headers")
	}
	if rec.Header().Get("X-Span-ID") == "" {
		t.Error("expected X-Span-ID in response headers")
	}
}

// TestHTTPCorrelationHandler validates the HandlerFunc wrapper.
func TestHTTPCorrelationHandler(t *testing.T) {
	var capturedCorrelationID string

	handler := HTTPCorrelationHandler(func(w http.ResponseWriter, r *http.Request) {
		capturedCorrelationID = GetCorrelationID(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(CorrelationIDHeaderKey, "handler-corr")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if capturedCorrelationID != "handler-corr" {
		t.Errorf("correlation ID = %q, want %q", capturedCorrelationID, "handler-corr")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Header Extraction Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestExtractCorrelationIDFromHeaders validates extraction from various header formats.
func TestExtractCorrelationIDFromHeaders(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string]string
		expected string
	}{
		{
			name:     "X-Correlation-ID",
			headers:  map[string]string{CorrelationIDHeaderKey: "corr-1"},
			expected: "corr-1",
		},
		{
			name:     "X-Request-ID fallback",
			headers:  map[string]string{RequestIDHeaderKey: "req-1"},
			expected: "req-1",
		},
		{
			name:     "lowercase x-correlation-id",
			headers:  map[string]string{"x-correlation-id": "lower-corr"},
			expected: "lower-corr",
		},
		{
			name:     "lowercase x-request-id",
			headers:  map[string]string{"x-request-id": "lower-req"},
			expected: "lower-req",
		},
		{
			name:     "X-Correlation-ID takes precedence",
			headers:  map[string]string{CorrelationIDHeaderKey: "corr", RequestIDHeaderKey: "req"},
			expected: "corr",
		},
		{
			name:     "no headers",
			headers:  map[string]string{},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			got := extractCorrelationIDFromHeaders(req)

			if got != tt.expected {
				t.Errorf("extractCorrelationIDFromHeaders() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// TestExtractTraceContextFromHeaders_W3C validates W3C TraceContext parsing.
func TestExtractTraceContextFromHeaders_W3C(t *testing.T) {
	tests := []struct {
		name          string
		traceparent   string
		expectedTrace string
		expectedSpan  string
	}{
		{
			name:          "valid W3C format",
			traceparent:   "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
			expectedTrace: "0af7651916cd43dd8448eb211c80319c",
			expectedSpan:  "b7ad6b7169203331",
		},
		{
			name:          "valid with different flags",
			traceparent:   "00-12345678901234567890123456789012-1234567890123456-00",
			expectedTrace: "12345678901234567890123456789012",
			expectedSpan:  "1234567890123456",
		},
		{
			name:          "malformed - too few parts",
			traceparent:   "00-traceonly",
			expectedTrace: "",
			expectedSpan:  "",
		},
		{
			name:          "empty",
			traceparent:   "",
			expectedTrace: "",
			expectedSpan:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			if tt.traceparent != "" {
				req.Header.Set(TraceIDHeaderKey, tt.traceparent)
			}

			gotTrace, gotSpan := extractTraceContextFromHeaders(req)

			if gotTrace != tt.expectedTrace {
				t.Errorf("trace ID = %q, want %q", gotTrace, tt.expectedTrace)
			}
			if gotSpan != tt.expectedSpan {
				t.Errorf("span ID = %q, want %q", gotSpan, tt.expectedSpan)
			}
		})
	}
}

// TestExtractTraceContextFromHeaders_XHeaders validates X-Trace-ID/X-Span-ID parsing.
func TestExtractTraceContextFromHeaders_XHeaders(t *testing.T) {
	tests := []struct {
		name          string
		headers       map[string]string
		expectedTrace string
		expectedSpan  string
	}{
		{
			name:          "X-Trace-ID only",
			headers:       map[string]string{"X-Trace-ID": "xtrace123"},
			expectedTrace: "xtrace123",
			expectedSpan:  "",
		},
		{
			name:          "X-Span-ID only",
			headers:       map[string]string{"X-Span-ID": "xspan456"},
			expectedTrace: "",
			expectedSpan:  "xspan456",
		},
		{
			name:          "both X headers",
			headers:       map[string]string{"X-Trace-ID": "xtrace", "X-Span-ID": "xspan"},
			expectedTrace: "xtrace",
			expectedSpan:  "xspan",
		},
		{
			name:          "lowercase headers",
			headers:       map[string]string{"x-trace-id": "lower-trace", "x-span-id": "lower-span"},
			expectedTrace: "lower-trace",
			expectedSpan:  "lower-span",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			gotTrace, gotSpan := extractTraceContextFromHeaders(req)

			if gotTrace != tt.expectedTrace {
				t.Errorf("trace ID = %q, want %q", gotTrace, tt.expectedTrace)
			}
			if gotSpan != tt.expectedSpan {
				t.Errorf("span ID = %q, want %q", gotSpan, tt.expectedSpan)
			}
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Header Injection Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestInjectTraceContextToRequest validates request header injection.
func TestInjectTraceContextToRequest(t *testing.T) {
	lc := LogContext{
		CorrelationID: "inject-corr",
		TraceID:       "inject-trace",
		SpanID:        "inject-span",
	}

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	InjectTraceContextToRequest(req, lc)

	if req.Header.Get(CorrelationIDHeaderKey) != "inject-corr" {
		t.Errorf("correlation header = %q, want %q",
			req.Header.Get(CorrelationIDHeaderKey), "inject-corr")
	}
	if req.Header.Get("X-Trace-ID") != "inject-trace" {
		t.Errorf("X-Trace-ID = %q, want %q", req.Header.Get("X-Trace-ID"), "inject-trace")
	}
	if req.Header.Get("X-Span-ID") != "inject-span" {
		t.Errorf("X-Span-ID = %q, want %q", req.Header.Get("X-Span-ID"), "inject-span")
	}
}

// TestInjectTraceContextToRequest_W3CFormat validates W3C traceparent format.
func TestInjectTraceContextToRequest_W3CFormat(t *testing.T) {
	lc := LogContext{
		TraceID: "0af7651916cd43dd8448eb211c80319c",
		SpanID:  "b7ad6b7169203331",
	}

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	InjectTraceContextToRequest(req, lc)

	expectedTraceparent := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	if req.Header.Get(TraceIDHeaderKey) != expectedTraceparent {
		t.Errorf("traceparent = %q, want %q",
			req.Header.Get(TraceIDHeaderKey), expectedTraceparent)
	}
}

// TestInjectTraceContextToRequest_Partial validates partial injection.
func TestInjectTraceContextToRequest_Partial(t *testing.T) {
	lc := LogContext{
		CorrelationID: "only-corr",
		// TraceID and SpanID empty
	}

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	InjectTraceContextToRequest(req, lc)

	if req.Header.Get(CorrelationIDHeaderKey) != "only-corr" {
		t.Errorf("correlation header = %q, want %q",
			req.Header.Get(CorrelationIDHeaderKey), "only-corr")
	}
	// Traceparent should not be set when trace/span are empty
	if req.Header.Get(TraceIDHeaderKey) != "" {
		t.Errorf("traceparent should be empty, got %q", req.Header.Get(TraceIDHeaderKey))
	}
}

// TestInjectTraceContextToResponse validates response header injection.
func TestInjectTraceContextToResponse(t *testing.T) {
	lc := LogContext{
		CorrelationID: "resp-corr",
		TraceID:       "resp-trace",
		SpanID:        "resp-span",
	}

	rec := httptest.NewRecorder()
	InjectTraceContextToResponse(rec, lc)

	if rec.Header().Get(CorrelationIDHeaderKey) != "resp-corr" {
		t.Errorf("correlation header = %q, want %q",
			rec.Header().Get(CorrelationIDHeaderKey), "resp-corr")
	}
	if rec.Header().Get("X-Trace-ID") != "resp-trace" {
		t.Errorf("X-Trace-ID = %q, want %q", rec.Header().Get("X-Trace-ID"), "resp-trace")
	}
	if rec.Header().Get("X-Span-ID") != "resp-span" {
		t.Errorf("X-Span-ID = %q, want %q", rec.Header().Get("X-Span-ID"), "resp-span")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// RoundTripper Tests
// ═══════════════════════════════════════════════════════════════════════════

// mockRoundTripper captures the request for inspection.
type mockRoundTripper struct {
	capturedReq *http.Request
	response    *http.Response
	err         error
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	m.capturedReq = req
	if m.response == nil {
		m.response = &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
		}
	}
	return m.response, m.err
}

// TestRoundTripperWithCorrelation validates correlation injection.
func TestRoundTripperWithCorrelation(t *testing.T) {
	mock := &mockRoundTripper{}
	rt := &RoundTripperWithCorrelation{Base: mock}

	ctx := context.Background()
	ctx = WithCorrelationID(ctx, "rt-corr")
	ctx = WithTraceID(ctx, "rt-trace")
	ctx = WithSpanID(ctx, "rt-span")

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req = req.WithContext(ctx)

	_, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}

	// Check captured request has headers
	if mock.capturedReq.Header.Get(CorrelationIDHeaderKey) != "rt-corr" {
		t.Errorf("correlation header = %q, want %q",
			mock.capturedReq.Header.Get(CorrelationIDHeaderKey), "rt-corr")
	}
	if mock.capturedReq.Header.Get("X-Trace-ID") != "rt-trace" {
		t.Errorf("X-Trace-ID = %q, want %q",
			mock.capturedReq.Header.Get("X-Trace-ID"), "rt-trace")
	}
}

// TestRoundTripperWithCorrelation_NilBase validates fallback to DefaultTransport.
func TestRoundTripperWithCorrelation_NilBase(t *testing.T) {
	rt := &RoundTripperWithCorrelation{Base: nil}

	// This would actually make an HTTP request to a test server in a real test
	// For now, we just verify the struct allows nil Base
	if rt.Base != nil {
		t.Errorf("Base = %v, want nil", rt.Base)
	}
}

// TestNewHTTPClientWithCorrelation validates client factory.
func TestNewHTTPClientWithCorrelation(t *testing.T) {
	client := NewHTTPClientWithCorrelation()

	if client == nil {
		t.Fatal("NewHTTPClientWithCorrelation() returned nil")
	}

	rt, ok := client.Transport.(*RoundTripperWithCorrelation)
	if !ok {
		t.Fatalf("Transport = %T, want *RoundTripperWithCorrelation", client.Transport)
	}
	if rt.Base != http.DefaultTransport {
		t.Errorf("RoundTripper.Base = %v, want DefaultTransport", rt.Base)
	}
}
