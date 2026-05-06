package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/observability"
)

// correlationMW injects correlation, trace, and span IDs into the request
// context and echoes them on the response. It prefers incoming headers when
// present and generates cryptographically random IDs otherwise.
//
// Header priority:
//
//	Correlation ID: X-Correlation-ID > X-Request-ID > generate
//	Trace/Span:     traceparent (W3C) > X-Trace-ID / X-Span-ID > generate
func (s *Server) correlationMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		correlationID := firstNonEmpty(
			r.Header.Get("X-Correlation-ID"),
			r.Header.Get("X-Request-ID"),
		)
		if correlationID == "" {
			correlationID = generateHexID(16)
		}
		ctx = observability.WithCorrelationID(ctx, correlationID)

		var traceID, spanID string
		if tp := r.Header.Get("Traceparent"); tp != "" {
			if tc, ok := messaging.ParseTraceparent(tp); ok {
				traceID = tc.TraceID
				spanID = tc.SpanID
			}
		}

		if traceID == "" {
			traceID = sanitizePropagatedID(r.Header.Get("X-Trace-ID"))
		}
		if traceID == "" {
			traceID = generateHexID(16)
		}
		ctx = observability.WithTraceID(ctx, traceID)

		if spanID == "" {
			spanID = sanitizePropagatedID(r.Header.Get("X-Span-ID"))
		}
		if spanID == "" {
			spanID = generateHexID(8)
		}
		ctx = observability.WithSpanID(ctx, spanID)

		w.Header().Set("X-Correlation-ID", correlationID)
		w.Header().Set("X-Trace-ID", traceID)
		w.Header().Set("X-Span-ID", spanID)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// generateHexID returns n random bytes encoded as lowercase hex (2*n chars).
func generateHexID(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("httpapi: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

const maxPropagatedIDLen = 256

func sanitizePropagatedID(s string) string {
	if len(s) > maxPropagatedIDLen {
		return s[:maxPropagatedIDLen]
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return sanitizePropagatedID(v)
		}
	}
	return ""
}
