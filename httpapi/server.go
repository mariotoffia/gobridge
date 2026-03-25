package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/runtime"
)

// Config holds HTTP server configuration.
type Config struct {
	AdminAddr   string `json:"admin_addr"`
	MonitorAddr string `json:"monitor_addr"`
	APIKey      string `json:"-"`
	CORSOrigins string `json:"cors_origins"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		AdminAddr:   ":8080",
		MonitorAddr: ":8081",
	}
}

// Server manages the admin and monitor HTTP endpoints.
type Server struct {
	rt     *runtime.Runtime
	cfg    Config
	logger *slog.Logger

	admin   *http.Server
	monitor *http.Server

	mu      sync.Mutex
	running bool
}

// Option configures a Server.
type Option func(*Server)

// WithServerLogger sets the logger.
func WithServerLogger(l *slog.Logger) Option {
	return func(s *Server) { s.logger = l }
}

// New creates an HTTP Server bound to the given runtime.
func New(rt *runtime.Runtime, cfg Config, opts ...Option) *Server {
	s := &Server{rt: rt, cfg: cfg}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Start starts both HTTP servers. It binds listeners synchronously so
// port conflicts are detected immediately, then serves in background.
func (s *Server) Start(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("httpapi: already running")
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

func (s *Server) wrap(h http.Handler) http.Handler {
	h = s.recoverMW(h)
	if s.cfg.CORSOrigins != "" {
		h = s.corsMW(h)
	}
	return h
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

func (s *Server) corsMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.isAllowedOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Authorization")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) isAllowedOrigin(origin string) bool {
	if s.cfg.CORSOrigins == "*" {
		return true
	}
	for _, allowed := range strings.Split(s.cfg.CORSOrigins, ",") {
		if strings.TrimSpace(allowed) == origin {
			return true
		}
	}
	return false
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIKey == "" {
			next(w, r)
			return
		}
		expected := []byte(s.cfg.APIKey)
		if k := r.Header.Get("X-API-Key"); subtle.ConstantTimeCompare([]byte(k), expected) == 1 {
			next(w, r)
			return
		}
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token := strings.TrimPrefix(auth, "Bearer ")
			if subtle.ConstantTimeCompare([]byte(token), expected) == 1 {
				next(w, r)
				return
			}
		}
		writeErr(w, http.StatusUnauthorized, "invalid or missing API key")
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if data != nil {
		_ = json.NewEncoder(w).Encode(data)
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
