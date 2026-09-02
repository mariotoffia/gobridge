package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/observability"
)

// correlationMW injects correlation, trace, and span IDs into the request
// context and echoes them on the response. It prefers incoming headers when
// present and generates cryptographically random IDs otherwise.
//
// Header priority:
//
//	Correlation ID: X-Correlation-ID > X-Request-ID > generate
//	Trace/Span: traceparent (W3C) > X-Trace-ID / X-Span-ID > generate
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

func (s *Server) wrap(h http.Handler) http.Handler {
	h = s.correlationMW(h)
	h = s.recoverMW(h)
	if s.cfg.CORSOrigins != "" {
		h = s.corsMW(h)
	}
	h = s.securityHeadersMW(h)
	h = s.requestLogMW(h)
	return h
}

func (s *Server) securityHeadersMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				if s.logger != nil {
					s.logger.Error("panic recovered", "error", err, "path", r.URL.Path)
				}
				writeErr(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestLogMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.clk.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(r.Context(), logging.LevelDebug, "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rw.status,
				"duration_ms", s.clk.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
			)
		}
	})
}

// corsAllowedMethods is the union of HTTP verbs served across the admin and
// monitor APIs, advertised uniformly to browsers on CORS preflight. The
// config transaction routes use PATCH (apply overlay) and DELETE (rollback),
// so both must be listed or browser-based admin clients fail preflight for
// those operations. The monitor mux serves only GET, so it over-advertises
// harmlessly (an unsupported verb still 405s at the mux). Keep in sync with
// the route registrations in admin.go, admin_config.go, and monitor.go.
const corsAllowedMethods = "GET, POST, PATCH, DELETE, OPTIONS"

func (s *Server) corsMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := origin != "" && s.isAllowedOrigin(origin)
		// The response body/headers depend on Origin whether or not the origin
		// is allowed, so Vary: Origin MUST be set unconditionally to keep
		// intermediary caches from serving an allow-listed response to a
		// disallowed origin (or vice versa).
		w.Header().Add("Vary", "Origin")
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", corsAllowedMethods)
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Authorization")
		}
		if r.Method == http.MethodOptions {
			if !allowed {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) isAllowedOrigin(origin string) bool {
	for _, allowed := range strings.Split(s.cfg.CORSOrigins, ",") {
		if strings.TrimSpace(allowed) == origin {
			return true
		}
	}
	return false
}

func (s *Server) requireAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Check the credential FIRST so a valid operator key ALWAYS passes,
		// even from a peer whose window is throttled by someone else's failed
		// attempts (a shared LB/NAT peer must not lock out a valid operator).
		// Only a FAILED credential consults and feeds the throttle.
		//
		// TRADEOFF: because the key is compared on EVERY request before the
		// throttle is consulted, the throttle shapes the failure RESPONSE (429
		// vs 401) but no longer caps the rate at which guesses are TESTED — a
		// throttled attacker's keys are still evaluated at line rate. Online
		// brute-force resistance therefore rests entirely on key entropy (the
		// 16-char floor enforces length, not randomness), so deployments MUST
		// use high-entropy admin keys. Restoring a test-rate cap would
		// reintroduce the shared-NAT lockout this ordering exists to prevent.
		if name, ok := s.matchAPIKey(r, s.currentAdminAPIKeys()); ok {
			s.adminThrottle.recordSuccess(throttleKeyFromRequest(r))
			next(w, r.WithContext(context.WithValue(r.Context(), ctxKeyActor{}, name)))
			return
		}
		// Throttle key is the transport peer (RemoteAddr host), never the
		// client-controlled X-Forwarded-For; see throttleKeyFromRequest.
		client := throttleKeyFromRequest(r)
		if s.adminThrottle.throttled(client) {
			// Audit only the transition into throttling (first reject per
			// window), not every rejected request — a brute-forcer must not be
			// able to write audit records at request line-rate.
			if s.adminThrottle.shouldAuditThrottle(client) {
				s.emitAudit(r, "auth.throttled", "admin", "", "failure", nil)
			}
			w.Header().Set("Retry-After", strconv.Itoa(s.adminThrottle.retryAfterSeconds()))
			writeErr(w, http.StatusTooManyRequests, "too many failed authentication attempts")
			return
		}
		s.adminThrottle.recordFailure(client)
		s.emitAudit(r, "auth.failure", "admin", "", "failure", nil)
		w.Header().Set("WWW-Authenticate", `Bearer realm="gobridge-admin"`)
		writeErr(w, http.StatusUnauthorized, "invalid or missing API key")
	}
}

func (s *Server) requireMonitorAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Check the credential FIRST (monitor key, or admin key as a superset)
		// so a valid key ALWAYS passes regardless of the peer's throttle window;
		// only a FAILED credential consults and feeds the monitor throttle. The
		// monitor throttle is a SEPARATE scope from the admin throttle so a
		// monitor-plane attacker cannot lock out the admin plane. Same tradeoff
		// as requireAdminAuth: credential-first means the throttle no longer
		// rate-caps guess EVALUATION, so monitor-key entropy is the sole online
		// brute-force control.
		monitorKey := s.currentMonitorAPIKey()
		if !monitorKey.IsZero() && s.checkAPIKey(r, monitorKey) {
			s.monitorThrottle.recordSuccess(throttleKeyFromRequest(r))
			next(w, r)
			return
		}
		if name, matched := s.matchAPIKey(r, s.currentAdminAPIKeys()); matched {
			// Admin is a superset of monitor access.
			s.monitorThrottle.recordSuccess(throttleKeyFromRequest(r))
			next(w, r.WithContext(context.WithValue(r.Context(), ctxKeyActor{}, name)))
			return
		}
		// Throttle key is the transport peer (RemoteAddr host), never the
		// client-controlled X-Forwarded-For; see throttleKeyFromRequest.
		client := throttleKeyFromRequest(r)
		if s.monitorThrottle.throttled(client) {
			// Audit only the transition into throttling (first reject per
			// window), not every rejected request — a brute-forcer must not be
			// able to write audit records at request line-rate.
			if s.monitorThrottle.shouldAuditThrottle(client) {
				s.emitAudit(r, "auth.throttled", "monitor", "", "failure", nil)
			}
			w.Header().Set("Retry-After", strconv.Itoa(s.monitorThrottle.retryAfterSeconds()))
			writeErr(w, http.StatusTooManyRequests, "too many failed authentication attempts")
			return
		}
		s.monitorThrottle.recordFailure(client)
		s.emitAudit(r, "auth.failure", "monitor", "", "failure", nil)
		w.Header().Set("WWW-Authenticate", `Bearer realm="gobridge-monitor"`)
		writeErr(w, http.StatusUnauthorized, "invalid or missing API key")
	}
}

// ctxKeyActor is the context key under which requireAdminAuth (and the admin
// fallback in requireMonitorAuth) stashes the matched admin-key NAME after a
// successful authentication. emitAudit reads it so the audit Actor is a stable,
// possession-based identity (the key name) instead of the spoofable network
// address. It is absent for unauthenticated / failed requests, so auth.failure
// events keep the network-address actor by definition.
type ctxKeyActor struct{}

// presentedCredentials extracts candidate API-key credentials from a request:
// the X-API-Key header value (when non-empty) and the Bearer token from the
// Authorization header (when it carries the "Bearer " prefix and is non-empty).
// The returned slice holds only non-empty credentials and may be empty.
func presentedCredentials(r *http.Request) []string {
	creds := make([]string, 0, 2)
	if k := r.Header.Get("X-API-Key"); k != "" {
		creds = append(creds, k)
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		if token := strings.TrimPrefix(auth, "Bearer "); token != "" {
			creds = append(creds, token)
		}
	}
	return creds
}

// matchAPIKey returns the name of the first admin key whose SHA-256 matches a
// presented credential (X-API-Key or Bearer). Each presented credential is
// hashed once into a fixed 32-byte digest, then every stored key is compared
// with subtle.ConstantTimeCompare over those digests. The comparison timing is
// therefore independent of the presented credential's length and content (the
// variable-length secret is absorbed into a fixed-width digest before any
// compare). The only data-dependent branch is the early return on a match,
// whose iteration count leaks at most the number of configured keys and which
// of the (≤2) credential slots matched — never any key material.
func (s *Server) matchAPIKey(r *http.Request, keys map[string]shared.Secret) (string, bool) {
	presented := presentedCredentials(r)
	if len(presented) == 0 || len(keys) == 0 {
		return "", false
	}
	hashes := make([][32]byte, len(presented))
	for i, c := range presented {
		hashes[i] = sha256.Sum256([]byte(c))
	}
	for name, key := range keys {
		if key.IsZero() {
			continue
		}
		kHash := sha256.Sum256([]byte(key.Reveal()))
		for i := range hashes {
			if subtle.ConstantTimeCompare(hashes[i][:], kHash[:]) == 1 {
				return name, true
			}
		}
	}
	return "", false
}

// checkAPIKey reports whether a request presents a credential matching the
// single expected key. It backs the monitor single-key path; the admin path
// uses matchAPIKey to also recover the matched key name.
func (s *Server) checkAPIKey(r *http.Request, expected shared.Secret) bool {
	if expected.IsZero() {
		return false
	}
	expHash := sha256.Sum256([]byte(expected.Reveal()))
	for _, c := range presentedCredentials(r) {
		cHash := sha256.Sum256([]byte(c))
		if subtle.ConstantTimeCompare(cHash[:], expHash[:]) == 1 {
			return true
		}
	}
	return false
}

// statusRecorder captures the HTTP status code for logging while
// preserving optional http.ResponseWriter interfaces.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if data != nil {
		if err := json.NewEncoder(w).Encode(data); err != nil {
			slog.Error("failed to encode JSON response", "error", err)
		}
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
