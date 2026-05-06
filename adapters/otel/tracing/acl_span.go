package oteltracing

import (
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var _ ports.Span = (*otelSpan)(nil)

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

func (s *otelSpan) SetError(err error) {
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
