package oteltracing

import (
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var (
	_ ports.Span         = (*otelSpan)(nil)
	_ ports.SpanIdentity = (*otelSpan)(nil)
)

// otelSpan adapts an OpenTelemetry [trace.Span] to [ports.Span]. It
// lives in the ACL because the embedded SDK Span is the boundary
// between OTel's vocabulary (codes, attributes, events) and the
// project's domain types.
type otelSpan struct {
	span trace.Span
}

func (s *otelSpan) End() {
	s.span.End()
}

// SetError records err on the span and marks it failed. A nil err is a
// no-op, matching the noop span (ports.Span carries no non-nil
// precondition): the OTel span must tolerate it too rather than panic in
// otel's RecordError/err.Error() on a nil interface.
func (s *otelSpan) SetError(err error) {
	if err == nil {
		return
	}
	s.span.RecordError(err)
	s.span.SetStatus(codes.Error, err.Error())
}

func (s *otelSpan) AddEvent(name string, attrs ...shared.Tag) {
	if len(attrs) > 0 {
		s.span.AddEvent(name, trace.WithAttributes(tagsToAttributes(attrs)...))
		return
	}
	s.span.AddEvent(name)
}

func (s *otelSpan) SetAttributes(attrs ...shared.Tag) {
	if len(attrs) > 0 {
		s.span.SetAttributes(tagsToAttributes(attrs)...)
	}
}

// TraceID implements the OPTIONAL [ports.SpanIdentity] capability: the
// runtime stamps log-correlation fields from the ACTIVE span so logs
// carry this hop's identity (and root deliveries a trace_id at all).
// Returns "" when the span context is invalid or the trace is unsampled
// — an unsampled trace exports no spans, so the logged id would dangle.
func (s *otelSpan) TraceID() string {
	sc := s.span.SpanContext()
	if !sc.IsValid() || !sc.IsSampled() {
		return ""
	}
	return sc.TraceID().String()
}

// SpanID implements [ports.SpanIdentity]; see [otelSpan.TraceID].
func (s *otelSpan) SpanID() string {
	sc := s.span.SpanContext()
	if !sc.IsValid() || !sc.IsSampled() {
		return ""
	}
	return sc.SpanID().String()
}
