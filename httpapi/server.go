package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// adminWriteTimeoutMargin is added to AdminOperationTimeout to derive the admin
// server WriteTimeout so a slow-but-successful admin operation can flush its
// response before the write deadline fires.
const adminWriteTimeoutMargin = 15 * time.Second

// Config holds HTTP server configuration.
type Config struct {
	AdminAddr     string        `json:"admin_addr"`
	MonitorAddr   string        `json:"monitor_addr"`
	AdminAPIKey   shared.Secret `json:"-"`
	MonitorAPIKey shared.Secret `json:"-"`
	CORSOrigins   string        `json:"cors_origins"`

	// AdminAPIKeyProvider returns the current admin API key as a raw
	// string (e.g. fetched from a secret manager on rotation). When nil,
	// the static AdminAPIKey is used. The server wraps whatever this
	// returns in a redacting shared.Secret before any comparison.
	AdminAPIKeyProvider func() string `json:"-"`

	// AdminAPIKeys maps operator-facing key NAMES to admin API keys. Each name
	// becomes the audit Actor when that key authenticates. The legacy single
	// AdminAPIKey folds in under the name "admin"; an explicit "admin" entry
	// here overrides it. At least one admin key must exist after folding.
	AdminAPIKeys map[string]shared.Secret `json:"-"`

	// AdminAPIKeysProvider returns the current named admin keys as raw strings
	// (e.g. fetched/rotated from a secret manager). When set it replaces the
	// static AdminAPIKeys per request; each value is wrapped in a redacting
	// shared.Secret before comparison. Mirrors AdminAPIKeyProvider.
	AdminAPIKeysProvider func() map[string]string `json:"-"`

	// MonitorAPIKeyProvider returns the current monitor API key as a raw
	// string. When nil, the static MonitorAPIKey is used. The server
	// wraps the returned value in a redacting shared.Secret before use.
	MonitorAPIKeyProvider func() string `json:"-"`

	// RuntimeProvider returns the current runtime backing the admin/monitor
	// APIs. When nil, the Server uses the runtime passed to New().
	RuntimeProvider func() ports.Runtime `json:"-"`

	// ConfigStore is the persistence boundary used by the admin
	// transactions API: validate / merge / save / load. The
	// composition root supplies an implementation (typically backed
	// by config.Manager). When set together with ConfigProvider,
	// the config management endpoints are enabled on the admin server.
	ConfigStore ports.ConfigStore `json:"-"`

	// ConfigProvider returns the current effective BridgeConfig.
	// Typically wired to bridge.Supervisor.Config().
	ConfigProvider func() *ports.BridgeConfig `json:"-"`

	// AdminOperationTimeout is the context timeout applied to admin
	// start/stop operations. Defaults to 30s when zero.
	AdminOperationTimeout time.Duration `json:"admin_operation_timeout,omitempty"`

	// TLSCertFile and TLSKeyFile are filesystem paths to the PEM server
	// certificate (with chain) and private key. When BOTH are set the admin
	// and monitor listeners serve HTTPS and AdminURL/MonitorURL report the
	// https scheme. When either is empty the servers stay plaintext (the
	// default) and rely on an external TLS terminator. Setting only one is a
	// startup error.
	TLSCertFile string `json:"tls_cert_file,omitempty"`
	TLSKeyFile  string `json:"tls_key_file,omitempty"`

	// ConfigApplier, when set, is invoked by a successful config-transaction
	// Commit AFTER the new blueprint is persisted, so the running runtime is
	// reconfigured in-band rather than relying solely on the file watcher. A
	// non-nil error fails the commit response (the durable write already
	// happened; the operator must reconcile) instead of falsely reporting
	// "committed" while the runtime diverges. When nil, application is
	// delegated to the config watcher (historical behavior).
	ConfigApplier func(ctx context.Context, cfg *ports.BridgeConfig) error `json:"-"`

	// AuthFailureLimit bounds failed authentication attempts per client within
	// AuthFailureWindow before further attempts from that client are rejected
	// with 429. Zero uses defaultAuthFailureLimit. AuthFailureWindow zero uses
	// defaultAuthFailureWindow.
	AuthFailureLimit  int           `json:"auth_failure_limit,omitempty"`
	AuthFailureWindow time.Duration `json:"auth_failure_window,omitempty"`
}

// DefaultConfig returns a Config with security-first defaults.
// CORS is disabled (empty origins) and must be explicitly configured.
// API keys must be set before starting; the server rejects startup
// without an AdminAPIKey.
func DefaultConfig() Config {
	return Config{
		AdminAddr:   ":8080",
		MonitorAddr: ":8081",
	}
}

// Server manages the admin and monitor HTTP endpoints.
type Server struct {
	rt                 ports.Runtime
	rtProvider         func() ports.Runtime
	adminKeyProvider   func() shared.Secret
	adminKeysProvider  func() map[string]shared.Secret
	monitorKeyProvider func() shared.Secret
	cfg                Config
	logger             *slog.Logger
	audit              ports.AuditLogger
	clk                clock.Clock
	idGen              idGenFn
	configTxn          *configTxnManager // nil when config management is disabled
	authThrottle       *authThrottle

	admin    *http.Server
	monitor  *http.Server
	adminURL string // actual bound address (e.g. "http://127.0.0.1:54321")
	monURL   string // actual bound monitor address

	mu      sync.Mutex
	running bool
}

// Option configures a Server.
type Option func(*Server)

// WithServerLogger sets the logger.
func WithServerLogger(l *slog.Logger) Option {
	return func(s *Server) { s.logger = l }
}

// WithAuditLogger sets the audit logger for security-relevant operations.
func WithAuditLogger(a ports.AuditLogger) Option {
	return func(s *Server) { s.audit = a }
}

// WithClock sets the clock used for request timestamps, durations, and admin transaction time.
func WithClock(c clock.Clock) Option {
	return func(s *Server) {
		if c != nil {
			s.clk = c
		}
	}
}

// idGenFn produces a fresh envelope ID. It is the seam used by the
// admin Inject endpoint when the request body omits an "id" field. A
// function (rather than a clock.Clock dependency) keeps tests
// deterministic without pulling crypto/rand into every spec.
type idGenFn func() string

// WithIDGenerator overrides the envelope-ID generator used by the
// admin Inject endpoint. Tests inject a deterministic generator;
// production callers leave it unset so the default crypto/rand UUID
// generator is used.
func WithIDGenerator(fn idGenFn) Option {
	return func(s *Server) {
		if fn != nil {
			s.idGen = fn
		}
	}
}

// New creates an HTTP Server bound to the given runtime.
func New(rt ports.Runtime, cfg Config, opts ...Option) *Server {
	s := &Server{rt: rt, cfg: cfg}
	for _, o := range opts {
		o(s)
	}
	if s.audit == nil {
		s.audit = ports.NoopAuditLogger{}
	}
	if s.clk == nil {
		s.clk = clock.System
	}
	if s.idGen == nil {
		s.idGen = defaultIDGen
	}
	if cfg.RuntimeProvider != nil {
		s.rtProvider = cfg.RuntimeProvider
	} else {
		s.rtProvider = func() ports.Runtime { return rt }
	}
	if cfg.AdminAPIKeyProvider != nil {
		s.adminKeyProvider = func() shared.Secret { return shared.NewSecret(cfg.AdminAPIKeyProvider()) }
	} else {
		s.adminKeyProvider = func() shared.Secret { return cfg.AdminAPIKey }
	}
	if cfg.AdminAPIKeysProvider != nil {
		s.adminKeysProvider = func() map[string]shared.Secret {
			raw := cfg.AdminAPIKeysProvider()
			out := make(map[string]shared.Secret, len(raw))
			for name, k := range raw {
				out[name] = shared.NewSecret(k)
			}
			return out
		}
	}
	if cfg.MonitorAPIKeyProvider != nil {
		s.monitorKeyProvider = func() shared.Secret { return shared.NewSecret(cfg.MonitorAPIKeyProvider()) }
	} else {
		s.monitorKeyProvider = func() shared.Secret { return cfg.MonitorAPIKey }
	}
	if s.cfg.AdminOperationTimeout <= 0 {
		s.cfg.AdminOperationTimeout = 30 * time.Second
	}
	if cfg.ConfigStore != nil && cfg.ConfigProvider != nil {
		s.configTxn = newTxnManager(cfg.ConfigStore, cfg.ConfigProvider, cfg.ConfigApplier, s.logger, s.clk)
	}
	s.authThrottle = newAuthThrottle(s.clk, cfg.AuthFailureLimit, cfg.AuthFailureWindow)
	return s
}

func (s *Server) currentRuntime() ports.Runtime { //nolint:ireturn // intentional: the server depends on the ports.Runtime driving-port interface, not the concrete runtime type
	if s.rtProvider != nil {
		if rt := s.rtProvider(); rt != nil {
			return rt
		}
	}
	return s.rt
}

func (s *Server) currentAdminAPIKey() shared.Secret {
	if s.adminKeyProvider != nil {
		key := s.adminKeyProvider()
		// Warn only when an explicit single-key provider was configured and
		// returned empty (a rotation failure). The default wrapper returns the
		// static AdminAPIKey, which is legitimately empty when only the named
		// AdminAPIKeys map is configured — that path must not warn per request.
		if key.IsZero() && s.cfg.AdminAPIKeyProvider != nil && s.logger != nil {
			s.logger.Warn("admin API key provider returned empty key; all admin requests will be rejected")
		}
		return key
	}
	return s.cfg.AdminAPIKey
}

// currentAdminAPIKeys folds the legacy single admin key (as name "admin")
// with the named keys (static or from the provider). Named keys win on a
// name collision. Zero-value secrets are skipped.
func (s *Server) currentAdminAPIKeys() map[string]shared.Secret {
	out := make(map[string]shared.Secret)
	if k := s.currentAdminAPIKey(); !k.IsZero() {
		out["admin"] = k
	}
	named := s.cfg.AdminAPIKeys
	if s.adminKeysProvider != nil {
		named = s.adminKeysProvider()
	}
	for name, k := range named {
		if !k.IsZero() {
			out[name] = k
		}
	}
	return out
}

func (s *Server) currentMonitorAPIKey() shared.Secret {
	if s.monitorKeyProvider != nil {
		return s.monitorKeyProvider()
	}
	return s.cfg.MonitorAPIKey
}

// Start starts both HTTP servers. It validates configuration, binds
// listeners synchronously so port conflicts are detected immediately,
// then serves in background.
func (s *Server) Start(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("httpapi: already running")
	}

	if err := s.validateConfig(); err != nil {
		return err
	}

	// TLS is opt-in via a cert/key pair. Loading here fails startup fast on a
	// bad or unreadable pair rather than on the first request.
	var tlsConf *tls.Config
	if s.tlsEnabled() {
		cert, err := tls.LoadX509KeyPair(s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
		if err != nil {
			return fmt.Errorf("httpapi: load TLS keypair: %w", err)
		}
		tlsConf = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	adminMux := http.NewServeMux()
	s.registerAdminRoutes(adminMux)

	monitorMux := http.NewServeMux()
	s.registerMonitorRoutes(monitorMux)

	// The admin WriteTimeout must exceed AdminOperationTimeout: a start/stop
	// that legitimately runs up to AdminOperationTimeout server-side would
	// otherwise have its response connection reset by an equal WriteTimeout,
	// leaving the operator retrying against an ambiguous state.
	adminWriteTimeout := s.cfg.AdminOperationTimeout + adminWriteTimeoutMargin

	s.admin = &http.Server{
		Addr:         s.cfg.AdminAddr,
		Handler:      s.wrap(adminMux),
		TLSConfig:    tlsConf,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: adminWriteTimeout,
		IdleTimeout:  120 * time.Second,
	}
	s.monitor = &http.Server{
		Addr:         s.cfg.MonitorAddr,
		Handler:      s.wrap(monitorMux),
		TLSConfig:    tlsConf,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	adminLn, err := net.Listen("tcp", s.cfg.AdminAddr)
	if err != nil {
		return fmt.Errorf("httpapi: admin listen %s: %w", s.cfg.AdminAddr, err)
	}
	monitorLn, err := net.Listen("tcp", s.cfg.MonitorAddr)
	if err != nil {
		_ = adminLn.Close()
		return fmt.Errorf("httpapi: monitor listen %s: %w", s.cfg.MonitorAddr, err)
	}

	scheme := "http://"
	if tlsConf != nil {
		scheme = "https://"
		adminLn = tls.NewListener(adminLn, tlsConf)
		monitorLn = tls.NewListener(monitorLn, tlsConf)
	}
	s.adminURL = scheme + adminLn.Addr().String()
	s.monURL = scheme + monitorLn.Addr().String()

	go func() {
		if err := s.admin.Serve(adminLn); err != nil && err != http.ErrServerClosed {
			if s.logger != nil {
				s.logger.Error("admin server error", "error", err)
			}
		}
	}()
	go func() {
		if err := s.monitor.Serve(monitorLn); err != nil && err != http.ErrServerClosed {
			if s.logger != nil {
				s.logger.Error("monitor server error", "error", err)
			}
		}
	}()

	s.running = true
	return nil
}

// AdminURL returns the actual bound admin URL (e.g. "http://127.0.0.1:54321",
// or "https://..." when TLS is enabled). Only valid after Start returns
// successfully. Reads are synchronised against Start's write.
func (s *Server) AdminURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adminURL
}

// MonitorURL returns the actual bound monitor URL. Only valid after Start
// returns successfully. Reads are synchronised against Start's write.
func (s *Server) MonitorURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.monURL
}

// tlsEnabled reports whether an in-process TLS keypair is configured.
func (s *Server) tlsEnabled() bool {
	return s.cfg.TLSCertFile != "" && s.cfg.TLSKeyFile != ""
}

// Stop gracefully shuts down both HTTP servers.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}
	s.running = false

	var errs []error
	if s.admin != nil {
		if err := s.admin.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if s.monitor != nil {
		if err := s.monitor.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("httpapi: shutdown: %v", errs)
	}
	return nil
}

const minAPIKeyLen = 16

func (s *Server) validateConfig() error {
	adminKeys := s.currentAdminAPIKeys()
	if len(adminKeys) == 0 {
		return fmt.Errorf("httpapi: admin API key is required; set AdminAPIKey or AdminAPIKeys in Config")
	}
	for name, key := range adminKeys {
		if err := validateAdminKeyEntry(name, len(key.Reveal())); err != nil {
			return err
		}
	}
	monitorKey := s.currentMonitorAPIKey()
	if !monitorKey.IsZero() && len(monitorKey.Reveal()) < minAPIKeyLen {
		return fmt.Errorf("httpapi: monitor API key must be at least %d characters when set", minAPIKeyLen)
	}
	if s.cfg.CORSOrigins == "*" {
		return fmt.Errorf("httpapi: wildcard CORS origin '*' is not allowed; specify explicit origins or leave empty to disable CORS")
	}
	for _, o := range strings.Split(s.cfg.CORSOrigins, ",") {
		if strings.TrimSpace(o) == "*" {
			return fmt.Errorf("httpapi: wildcard CORS origin '*' is not allowed; specify explicit origins or leave empty to disable CORS")
		}
	}
	if (s.cfg.TLSCertFile == "") != (s.cfg.TLSKeyFile == "") {
		return fmt.Errorf("httpapi: TLS requires both tls_cert_file and tls_key_file; set both to enable HTTPS or neither to stay plaintext")
	}
	return nil
}

// validateAdminKeyEntry checks one folded admin key's NAME (tag-safe) and its
// length (>= minAPIKeyLen). Shared by validateConfig (startup, over
// shared.Secret values) and ValidateAdminKeys (reload, over raw strings) so
// both boundaries enforce identical rules. It never returns key material — only
// the name and the length bound appear in the error text.
func validateAdminKeyEntry(name string, keyLen int) error {
	if !validAdminKeyName(name) {
		return fmt.Errorf("httpapi: invalid admin key name %q; must match [a-z0-9._-]+ and be 1-64 chars", name)
	}
	if keyLen < minAPIKeyLen {
		return fmt.Errorf("httpapi: admin API key %q must be at least %d characters", name, minAPIKeyLen)
	}
	return nil
}

// ValidateAdminKeys validates a raw name->key admin map against the same rules
// validateConfig enforces at startup (tag-safe names, per-key minAPIKeyLen
// floor). The composition root MUST call this on every resolved/rotated
// admin-key set so a hot reload cannot install a below-floor key or an unsafe
// name that startup would have rejected. It never logs or returns key material
// (only the name and the length bound). An empty map is allowed here (the
// startup "at least one key" guard belongs to validateConfig); callers that
// require a non-empty set check that separately.
func ValidateAdminKeys(keys map[string]string) error {
	for name, k := range keys {
		if err := validateAdminKeyEntry(name, len(k)); err != nil {
			return err
		}
	}
	return nil
}

// validAdminKeyName reports whether name is safe to use as an audit Actor and
// potential metric tag: non-empty, at most 64 bytes, and composed only of
// bytes in the set [a-z0-9._-]. Uppercase, whitespace, slashes, and other
// punctuation are rejected so a key name can never inject structure into a log
// line or a metric tag. All allowed bytes are single-byte ASCII, so a byte
// scan is equivalent to the documented per-rune rule.
func validAdminKeyName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
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
		// Throttle key is the transport peer (RemoteAddr host), never the
		// client-controlled X-Forwarded-For; see throttleKeyFromRequest.
		client := throttleKeyFromRequest(r)
		if s.authThrottle.throttled(client) {
			// Audit only the transition into throttling (first reject per
			// window), not every rejected request — a brute-forcer must not be
			// able to write audit records at request line-rate.
			if s.authThrottle.shouldAuditThrottle(client) {
				s.emitAudit(r, "auth.throttled", "admin", "", "failure", nil)
			}
			w.Header().Set("Retry-After", strconv.Itoa(s.authThrottle.retryAfterSeconds()))
			writeErr(w, http.StatusTooManyRequests, "too many failed authentication attempts")
			return
		}
		name, ok := s.matchAPIKey(r, s.currentAdminAPIKeys())
		if !ok {
			s.authThrottle.recordFailure(client)
			s.emitAudit(r, "auth.failure", "admin", "", "failure", nil)
			w.Header().Set("WWW-Authenticate", `Bearer realm="gobridge-admin"`)
			writeErr(w, http.StatusUnauthorized, "invalid or missing API key")
			return
		}
		s.authThrottle.recordSuccess(client)
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKeyActor{}, name)))
	}
}

func (s *Server) requireMonitorAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Throttle key is the transport peer (RemoteAddr host), never the
		// client-controlled X-Forwarded-For; see throttleKeyFromRequest.
		client := throttleKeyFromRequest(r)
		if s.authThrottle.throttled(client) {
			// Audit only the transition into throttling (first reject per
			// window), not every rejected request — a brute-forcer must not be
			// able to write audit records at request line-rate.
			if s.authThrottle.shouldAuditThrottle(client) {
				s.emitAudit(r, "auth.throttled", "monitor", "", "failure", nil)
			}
			w.Header().Set("Retry-After", strconv.Itoa(s.authThrottle.retryAfterSeconds()))
			writeErr(w, http.StatusTooManyRequests, "too many failed authentication attempts")
			return
		}
		// Accept monitor key, or fall back to admin key (admin is a
		// superset of monitor access).
		ok := false
		monitorKey := s.currentMonitorAPIKey()
		if !monitorKey.IsZero() {
			ok = s.checkAPIKey(r, monitorKey)
		}
		if !ok {
			if name, matched := s.matchAPIKey(r, s.currentAdminAPIKeys()); matched {
				ok = true
				r = r.WithContext(context.WithValue(r.Context(), ctxKeyActor{}, name))
			}
		}
		if !ok {
			s.authThrottle.recordFailure(client)
			s.emitAudit(r, "auth.failure", "monitor", "", "failure", nil)
			w.Header().Set("WWW-Authenticate", `Bearer realm="gobridge-monitor"`)
			writeErr(w, http.StatusUnauthorized, "invalid or missing API key")
			return
		}
		s.authThrottle.recordSuccess(client)
		next(w, r)
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
