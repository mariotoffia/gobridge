package messaging_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// TestParseTraceparent validates ParseTraceparent for valid W3C traceparent values and common invalid inputs.
func TestParseTraceparent(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantOK bool
		wantTC messaging.TraceContext
	}{
		{
			name:   "valid sampled",
			input:  "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			wantOK: true,
			wantTC: messaging.TraceContext{
				TraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
				SpanID:  "00f067aa0ba902b7",
				Flags:   0x01,
			},
		},
		{
			name:   "valid unsampled",
			input:  "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00",
			wantOK: true,
			wantTC: messaging.TraceContext{
				TraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
				SpanID:  "00f067aa0ba902b7",
				Flags:   0x00,
			},
		},
		{
			name:   "invalid version",
			input:  "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			wantOK: false,
		},
		{
			name:   "trace ID too short",
			input:  "00-4bf92f3577b34da6a3ce929d0e0e47-00f067aa0ba902b7-01",
			wantOK: false,
		},
		{
			name:   "trace ID all zeros",
			input:  "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
			wantOK: false,
		},
		{
			name:   "span ID all zeros",
			input:  "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
			wantOK: false,
		},
		{
			name:   "wrong number of parts",
			input:  "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7",
			wantOK: false,
		},
		{
			name:   "non-hex characters in trace ID",
			input:  "00-4bf92f3577b34da6a3ce929d0e0eXXXX-00f067aa0ba902b7-01",
			wantOK: false,
		},
		{
			name:   "uppercase hex rejected",
			input:  "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01",
			wantOK: false,
		},
		{
			name:   "empty string",
			input:  "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc, ok := messaging.ParseTraceparent(tt.input)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.wantTC, tc)
			}
		})
	}
}

// TestFormatTraceparent verifies FormatTraceparent round-trips with ParseTraceparent and formats sampled and unsampled flags.
func TestFormatTraceparent(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		original := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
		tc, ok := messaging.ParseTraceparent(original)
		require.True(t, ok)
		assert.Equal(t, original, messaging.FormatTraceparent(tc))
	})

	t.Run("flags 01", func(t *testing.T) {
		tc := messaging.TraceContext{
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
			SpanID:  "00f067aa0ba902b7",
			Flags:   0x01,
		}
		assert.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", messaging.FormatTraceparent(tc))
	})

	t.Run("flags 00", func(t *testing.T) {
		tc := messaging.TraceContext{
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
			SpanID:  "00f067aa0ba902b7",
			Flags:   0x00,
		}
		assert.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00", messaging.FormatTraceparent(tc))
	})
}

// TestExtractTraceContext verifies ExtractTraceContext reads traceparent and optional tracestate and rejects nil or incomplete headers.
func TestExtractTraceContext(t *testing.T) {
	t.Run("with traceparent and tracestate", func(t *testing.T) {
		headers := map[string]any{
			"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			"tracestate":  "congo=t61rcWkgMzE",
		}
		tc, ok := messaging.ExtractTraceContext(headers)
		require.True(t, ok)
		assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", tc.TraceID)
		assert.Equal(t, "00f067aa0ba902b7", tc.SpanID)
		assert.Equal(t, byte(0x01), tc.Flags)
		assert.Equal(t, "congo=t61rcWkgMzE", tc.State)
	})

	t.Run("traceparent only", func(t *testing.T) {
		headers := map[string]any{
			"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00",
		}
		tc, ok := messaging.ExtractTraceContext(headers)
		require.True(t, ok)
		assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", tc.TraceID)
		assert.Empty(t, tc.State)
	})

	t.Run("nil headers", func(t *testing.T) {
		_, ok := messaging.ExtractTraceContext(nil)
		assert.False(t, ok)
	})

	t.Run("missing traceparent", func(t *testing.T) {
		headers := map[string]any{
			"tracestate": "congo=t61rcWkgMzE",
		}
		_, ok := messaging.ExtractTraceContext(headers)
		assert.False(t, ok)
	})
}

// TestInjectTraceContext verifies InjectTraceContext on nil and existing maps, omits empty tracestate, and round-trips with ExtractTraceContext.
func TestInjectTraceContext(t *testing.T) {
	tc := messaging.TraceContext{
		TraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:  "00f067aa0ba902b7",
		Flags:   0x01,
		State:   "congo=t61rcWkgMzE",
	}

	t.Run("nil map", func(t *testing.T) {
		h := messaging.InjectTraceContext(nil, tc)
		require.NotNil(t, h)
		assert.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", h["traceparent"])
		assert.Equal(t, "congo=t61rcWkgMzE", h["tracestate"])
	})

	t.Run("existing map", func(t *testing.T) {
		existing := map[string]any{"custom": "value"}
		h := messaging.InjectTraceContext(existing, tc)
		assert.Equal(t, "value", h["custom"])
		assert.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", h["traceparent"])
	})

	t.Run("with state", func(t *testing.T) {
		h := messaging.InjectTraceContext(nil, tc)
		assert.Equal(t, "congo=t61rcWkgMzE", h["tracestate"])
	})

	t.Run("empty state omits tracestate", func(t *testing.T) {
		noState := messaging.TraceContext{
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
			SpanID:  "00f067aa0ba902b7",
			Flags:   0x00,
		}
		h := messaging.InjectTraceContext(nil, noState)
		_, exists := h["tracestate"]
		assert.False(t, exists)
	})

	t.Run("round trip inject then extract", func(t *testing.T) {
		h := messaging.InjectTraceContext(nil, tc)
		extracted, ok := messaging.ExtractTraceContext(h)
		require.True(t, ok)
		assert.Equal(t, tc, extracted)
	})
}
