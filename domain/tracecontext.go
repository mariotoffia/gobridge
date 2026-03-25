package domain

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// TraceContext holds W3C Trace Context fields extracted from traceparent/tracestate headers.
type TraceContext struct {
	TraceID string
	SpanID  string
	Flags   byte
	State   string
}

// ParseTraceparent parses a W3C traceparent header value of the form
// "00-<32-hex-trace-id>-<16-hex-span-id>-<2-hex-flags>".
// It returns false if the value is malformed or violates the spec.
func ParseTraceparent(s string) (TraceContext, bool) {
	parts := strings.Split(s, "-")
	if len(parts) != 4 {
		return TraceContext{}, false
	}

	version, traceID, spanID, flagsHex := parts[0], parts[1], parts[2], parts[3]

	if version != "00" {
		return TraceContext{}, false
	}

	if len(traceID) != 32 || !isLowHex(traceID) || isAllZeros(traceID) {
		return TraceContext{}, false
	}

	if len(spanID) != 16 || !isLowHex(spanID) || isAllZeros(spanID) {
		return TraceContext{}, false
	}

	if len(flagsHex) != 2 || !isLowHex(flagsHex) {
		return TraceContext{}, false
	}

	flagBytes, err := hex.DecodeString(flagsHex)
	if err != nil {
		return TraceContext{}, false
	}

	return TraceContext{
		TraceID: traceID,
		SpanID:  spanID,
		Flags:   flagBytes[0],
	}, true
}

// FormatTraceparent formats a TraceContext into a W3C traceparent header value.
func FormatTraceparent(tc TraceContext) string {
	return fmt.Sprintf("00-%s-%s-%02x", tc.TraceID, tc.SpanID, tc.Flags)
}

// ExtractTraceContext reads traceparent (and optionally tracestate) from
// the given headers map. Returns false if traceparent is missing or invalid.
func ExtractTraceContext(headers map[string]any) (TraceContext, bool) {
	raw, ok := GetHeaderString(headers, HeaderTraceParent)
	if !ok {
		return TraceContext{}, false
	}

	tc, ok := ParseTraceparent(raw)
	if !ok {
		return TraceContext{}, false
	}

	if state, ok := GetHeaderString(headers, HeaderTraceState); ok {
		tc.State = state
	}

	return tc, true
}

// InjectTraceContext writes traceparent (and optionally tracestate) into
// the headers map, initialising it if nil. Returns the (possibly new) map.
func InjectTraceContext(headers map[string]any, tc TraceContext) map[string]any {
	headers = SetHeader(headers, HeaderTraceParent, FormatTraceparent(tc))

	if tc.State != "" {
		headers = SetHeader(headers, HeaderTraceState, tc.State)
	}

	return headers
}

func isLowHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func isAllZeros(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}
