package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

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
	monitorKeyProvider func() shared.Secret
	cfg                Config
	logger             *slog.Logger
	audit              ports.AuditLogger
	clk                clock.Clock
	idGen              idGenFn
	configTxn          *configTxnManager // nil when config management is disabled

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
	if cfg.MonitorAPIKeyProvider != nil {
		s.monitorKeyProvider = func() shared.Secret { return shared.NewSecret(cfg.MonitorAPIKeyProvider()) }
	} else {
		s.monitorKeyProvider = func() shared.Secret { return cfg.MonitorAPIKey }
	}
	if s.cfg.AdminOperationTimeout <= 0 {
		s.cfg.AdminOperationTimeout = 30 * time.Second
	}
	if cfg.ConfigStore != nil && cfg.ConfigProvider != nil {
		s.configTxn = newTxnManager(cfg.ConfigStore, cfg.ConfigProvider, s.logger, s.clk)
	}
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
		if key.IsZero() && s.logger != nil {
			s.logger.Warn("admin API key provider returned empty key; all admin requests will be rejected")
		}
		return key
	}
	return s.cfg.AdminAPIKey
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

	adminMux := http.NewServeMux()
	s.registerAdminRoutes(adminMux)

	monitorMux := http.NewServeMux()
	s.registerMonitorRoutes(monitorMux)

	s.admin = &http.Server{
		Addr:         s.cfg.AdminAddr,
		Handler:      s.wrap(adminMux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	s.monitor = &http.Server{
		Addr:         s.cfg.MonitorAddr,
		Handler:      s.wrap(monitorMux),
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

	s.adminURL = "http://" + adminLn.Addr().String()
	s.monURL = "http://" + monitorLn.Addr().String()

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

// AdminURL returns the actual bound admin URL (e.g. "http://127.0.0.1:54321").
// Only valid after Start returns successfully.
func (s *Server) AdminURL() string { return s.adminURL }

// MonitorURL returns the actual bound monitor URL.
// Only valid after Start returns successfully.
func (s *Server) MonitorURL() string { return s.monURL }

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
	adminKey := s.currentAdminAPIKey()
	if adminKey.IsZero() {
		return fmt.Errorf("httpapi: admin API key is required; set AdminAPIKey in Config")
	}
	if len(adminKey.Reveal()) < minAPIKeyLen {
		return fmt.Errorf("httpapi: admin API key must be at least %d characters", minAPIKeyLen)
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
	return nil
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
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", corsAllowedMethods)
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Authorization")
			w.Header().Set("Vary", "Origin")
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
		if !s.checkAPIKey(r, s.currentAdminAPIKey()) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gobridge-admin"`)
			writeErr(w, http.StatusUnauthorized, "invalid or missing API key")
			return
		}
		next(w, r)
	}
}

func (s *Server) requireMonitorAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Accept monitor key, or fall back to admin key (admin is a
		// superset of monitor access).
		ok := false
		monitorKey := s.currentMonitorAPIKey()
		if !monitorKey.IsZero() {
			ok = s.checkAPIKey(r, monitorKey)
		}
		if !ok {
			ok = s.checkAPIKey(r, s.currentAdminAPIKey())
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gobridge-monitor"`)
			writeErr(w, http.StatusUnauthorized, "invalid or missing API key")
			return
		}
		next(w, r)
	}
}

func (s *Server) checkAPIKey(r *http.Request, expected shared.Secret) bool {
	if expected.IsZero() {
		return false
	}
	expHash := sha256.Sum256([]byte(expected.Reveal()))
	if k := r.Header.Get("X-API-Key"); k != "" {
		kHash := sha256.Sum256([]byte(k))
		if subtle.ConstantTimeCompare(kHash[:], expHash[:]) == 1 {
			return true
		}
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		token := strings.TrimPrefix(auth, "Bearer ")
		tHash := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(tHash[:], expHash[:]) == 1 {
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
