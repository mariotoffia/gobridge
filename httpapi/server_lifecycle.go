package httpapi

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Server start/stop and the listener endpoints they publish. Split out of
// server.go, which holds the configuration and construction of the same type.
func (s *Server) Start(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("httpapi: already running")
	}

	if err := s.validateConfig(); err != nil {
		return err
	}

	// TLS is opt-in via a cert/key pair. The reloader loads the pair here so
	// startup fails fast on a bad or unreadable pair, then serves it through
	// GetCertificate with an mtime-checked lazy reload so a cert-manager renewal
	// (atomic file replace) is picked up without a process restart.
	var tlsConf *tls.Config
	if s.tlsEnabled() {
		cr, err := newCertReloader(s.cfg.TLSCertFile, s.cfg.TLSKeyFile, s.serverLogger())
		if err != nil {
			return fmt.Errorf("httpapi: load TLS keypair: %w", err)
		}
		tlsConf = &tls.Config{
			GetCertificate: cr.getCertificate,
			MinVersion:     tls.VersionTLS12,
		}
	}

	adminMux := http.NewServeMux()
	s.registerAdminRoutes(adminMux)

	monitorMux := http.NewServeMux()
	s.registerMonitorRoutes(monitorMux)

	// The admin WriteTimeout is derived from the longest response path this
	// server can serve (see adminWriteTimeout): a request that legitimately runs
	// that long server-side would otherwise have its response connection reset,
	// leaving the operator retrying against an ambiguous state.
	writeTimeout := s.adminWriteTimeout()

	s.admin = &http.Server{
		Addr:         s.cfg.AdminAddr,
		Handler:      s.wrap(adminMux),
		TLSConfig:    tlsConf,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: writeTimeout,
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
			s.serverLogger().Error("admin server error", "error", err)
		}
	}()
	go func() {
		if err := s.monitor.Serve(monitorLn); err != nil && err != http.ErrServerClosed {
			s.serverLogger().Error("monitor server error", "error", err)
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

// serverLogger returns the configured logger, or the package default when none
// was injected, so a background listener that dies (Serve returning a non-
// ErrServerClosed error) is never silently swallowed. Mirrors writeJSON's use
// of the package-global slog for last-resort diagnostics.
func (s *Server) serverLogger() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
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
