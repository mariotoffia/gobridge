// Package server provides the HTTP server for Admin and Monitor APIs.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// Server is the HTTP server for Admin and Monitor APIs.
type Server struct {
	adminServer   *http.Server
	monitorServer *http.Server

	adminMux   *http.ServeMux
	monitorMux *http.ServeMux

	bridge         BridgeController
	config         Config
	logger         SimpleLogger
	metrics        types.MetricsCollector
	dlqManager     types.DLQManager
	configReloader ConfigReloaderInterface

	mu      sync.RWMutex
	running bool
}

// ConfigReloaderInterface defines the interface for config reloading.
type ConfigReloaderInterface interface {
	Reload(ctx context.Context) (*ConfigReloadResult, error)
	StartWatching(ctx context.Context) error
	StopWatching()
}

// ConfigReloadResult contains the result of a configuration reload.
type ConfigReloadResult struct {
	Timestamp      time.Time     `json:"timestamp"`
	Source         string        `json:"source,omitempty"`
	ChangesApplied int           `json:"changesApplied"`
	Added          []string      `json:"added,omitempty"`
	Updated        []string      `json:"updated,omitempty"`
	Deleted        []string      `json:"deleted,omitempty"`
	Errors         []string      `json:"errors,omitempty"`
	Duration       time.Duration `json:"duration"`
}

// Config holds the HTTP server configuration.
type Config struct {
	// AdminAddr is the address for the admin API (e.g., ":8080")
	AdminAddr string `json:"adminAddr"`
	// MonitorAddr is the address for the monitor API (e.g., ":8081")
	MonitorAddr string `json:"monitorAddr"`

	// EnableAdmin enables the admin API
	EnableAdmin bool `json:"enableAdmin"`
	// EnableMonitor enables the monitor API
	EnableMonitor bool `json:"enableMonitor"`

	// AdminAPIPrefix is the prefix for admin API routes
	AdminAPIPrefix string `json:"adminApiPrefix"`
	// MonitorAPIPrefix is the prefix for monitor API routes
	MonitorAPIPrefix string `json:"monitorApiPrefix"`

	// ReadTimeout is the read timeout for HTTP requests
	ReadTimeout time.Duration `json:"readTimeout"`
	// WriteTimeout is the write timeout for HTTP responses
	WriteTimeout time.Duration `json:"writeTimeout"`
	// IdleTimeout is the idle timeout for keep-alive connections
	IdleTimeout time.Duration `json:"idleTimeout"`

	// EnableCORS enables CORS headers
	EnableCORS bool `json:"enableCors"`
	// CORSOrigins are allowed CORS origins
	CORSOrigins []string `json:"corsOrigins"`

	// APIKey is the required API key for admin operations (optional)
	APIKey string `json:"-"`
}

// DefaultConfig returns the default server configuration.
func DefaultConfig() Config {
	return Config{
		AdminAddr:        ":8080",
		MonitorAddr:      ":8081",
		EnableAdmin:      true,
		EnableMonitor:    true,
		AdminAPIPrefix:   "/api/v1/admin",
		MonitorAPIPrefix: "/api/v1/monitor",
		ReadTimeout:      30 * time.Second,
		WriteTimeout:     30 * time.Second,
		IdleTimeout:      60 * time.Second,
		EnableCORS:       true,
		CORSOrigins:      []string{"*"},
	}
}

// BridgeController is the interface for controlling the bridge from the API.
type BridgeController interface {
	// Lifecycle
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	IsReady() bool
	IsLive() bool

	// Health and Metrics
	Health(ctx context.Context) *types.HealthCheck
	Metrics() *types.BridgeMetrics

	// Pipelines
	GetPipelines() []types.Pipeline
	GetPipeline(id string) (types.Pipeline, bool)
	AddPipelineRunning(ctx context.Context, pipeline types.Pipeline) error
	RemovePipelineRunning(ctx context.Context, id string) error

	// Connections
	GetConnection(id string) (types.Connection, bool)
	ListConnections() []string
	AddConnection(ctx context.Context, id string, conn types.Connection) error

	// Config
	Validate() error

	// Status
	GetID() string
	GetClusterID() string
}

// New creates a new HTTP server.
func New(bridge BridgeController, config Config, opts ...Option) *Server {
	s := &Server{
		bridge:     bridge,
		config:     config,
		adminMux:   http.NewServeMux(),
		monitorMux: http.NewServeMux(),
		logger:     &noopLogger{},
		metrics:    &types.NoopMetricsCollector{},
	}

	for _, opt := range opts {
		opt(s)
	}

	s.setupRoutes()

	return s
}

// Option is a functional option for the server.
type Option func(*Server)

// WithLogger sets the logger.
func WithLogger(logger SimpleLogger) Option {
	return func(s *Server) {
		s.logger = logger
	}
}

// WithMetrics sets the metrics collector.
func WithMetrics(metrics types.MetricsCollector) Option {
	return func(s *Server) {
		s.metrics = metrics
	}
}

// WithDLQManager sets the DLQ manager.
func WithDLQManager(dlq types.DLQManager) Option {
	return func(s *Server) {
		s.dlqManager = dlq
	}
}

// WithConfigReloader sets the config reloader.
func WithConfigReloader(reloader ConfigReloaderInterface) Option {
	return func(s *Server) {
		s.configReloader = reloader
	}
}

// Start starts the HTTP servers.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("server already running")
	}

	errCh := make(chan error, 2)

	if s.config.EnableAdmin {
		s.adminServer = &http.Server{
			Addr:         s.config.AdminAddr,
			Handler:      s.wrapHandler(s.adminMux),
			ReadTimeout:  s.config.ReadTimeout,
			WriteTimeout: s.config.WriteTimeout,
			IdleTimeout:  s.config.IdleTimeout,
		}

		go func() {
			s.logger.Info("starting admin API", "addr", s.config.AdminAddr)
			if err := s.adminServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errCh <- fmt.Errorf("admin server: %w", err)
			}
		}()
	}

	if s.config.EnableMonitor {
		s.monitorServer = &http.Server{
			Addr:         s.config.MonitorAddr,
			Handler:      s.wrapHandler(s.monitorMux),
			ReadTimeout:  s.config.ReadTimeout,
			WriteTimeout: s.config.WriteTimeout,
			IdleTimeout:  s.config.IdleTimeout,
		}

		go func() {
			s.logger.Info("starting monitor API", "addr", s.config.MonitorAddr)
			if err := s.monitorServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errCh <- fmt.Errorf("monitor server: %w", err)
			}
		}()
	}

	// Brief wait to catch immediate startup errors
	select {
	case err := <-errCh:
		return err
	case <-time.After(100 * time.Millisecond):
		s.running = true
		return nil
	}
}

// Stop stops the HTTP servers gracefully.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}

	var errs []error

	if s.adminServer != nil {
		if err := s.adminServer.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("admin server shutdown: %w", err))
		}
	}

	if s.monitorServer != nil {
		if err := s.monitorServer.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("monitor server shutdown: %w", err))
		}
	}

	s.running = false

	if len(errs) > 0 {
		return fmt.Errorf("shutdown errors: %v", errs)
	}
	return nil
}

// wrapHandler wraps the handler with middleware.
func (s *Server) wrapHandler(handler http.Handler) http.Handler {
	// Apply middleware chain
	h := handler

	// Recovery middleware
	h = s.recoveryMiddleware(h)

	// CORS middleware
	if s.config.EnableCORS {
		h = s.corsMiddleware(h)
	}

	// Logging middleware
	h = s.loggingMiddleware(h)

	return h
}

func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				s.logger.Error("panic recovered", "error", err, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := false

		for _, o := range s.config.CORSOrigins {
			if o == "*" || o == origin {
				allowed = true
				break
			}
		}

		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrap response writer to capture status
		wrapped := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		s.logger.Debug("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.status,
			"duration", time.Since(start).String(),
		)
	})
}

func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.config.APIKey == "" {
			next(w, r)
			return
		}

		// Check X-API-Key header
		apiKey := r.Header.Get("X-API-Key")
		if apiKey == s.config.APIKey {
			next(w, r)
			return
		}

		// Check Bearer token
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			token := strings.TrimPrefix(auth, "Bearer ")
			if token == s.config.APIKey {
				next(w, r)
				return
			}
		}

		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid or missing API key")
	}
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Helper functions for JSON responses.

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if data != nil {
		json.NewEncoder(w).Encode(data)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]interface{}{
		"code":    code,
		"message": message,
	})
}

func writeErrorWithDetails(w http.ResponseWriter, status int, code, message string, details map[string]interface{}) {
	writeJSON(w, status, map[string]interface{}{
		"code":    code,
		"message": message,
		"details": details,
	})
}

// SimpleLogger is a simple logging interface for the HTTP server.
// This is separate from types.Logger to avoid circular dependencies
// and provide a simpler API for HTTP logging.
type SimpleLogger interface {
	Debug(msg string, args ...interface{})
	Info(msg string, args ...interface{})
	Warn(msg string, args ...interface{})
	Error(msg string, args ...interface{})
}

type noopLogger struct{}

func (l *noopLogger) Debug(msg string, args ...interface{}) {}
func (l *noopLogger) Info(msg string, args ...interface{})  {}
func (l *noopLogger) Warn(msg string, args ...interface{})  {}
func (l *noopLogger) Error(msg string, args ...interface{}) {}
