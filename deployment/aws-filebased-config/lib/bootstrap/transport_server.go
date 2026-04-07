package bootstrap

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

type transportHandlerRef struct {
	mu      sync.RWMutex
	handler http.Handler
}

func newTransportHandlerRef() *transportHandlerRef {
	return &transportHandlerRef{handler: http.NotFoundHandler()}
}

func (r *transportHandlerRef) Get() http.Handler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.handler
}

func (r *transportHandlerRef) Set(handler http.Handler) {
	if handler == nil {
		handler = http.NotFoundHandler()
	}
	r.mu.Lock()
	r.handler = handler
	r.mu.Unlock()
}

type transportServer struct {
	handlerRef *transportHandlerRef
	logger     *slog.Logger

	server *http.Server
	url    string
}

func newTransportServer(handlerRef *transportHandlerRef, logger *slog.Logger) *transportServer {
	return &transportServer{
		handlerRef: handlerRef,
		logger:     logger,
	}
}

func (s *transportServer) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	s.server = &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handlerRef.Get().ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	s.url = "http://" + ln.Addr().String()

	go func() {
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed && s.logger != nil {
			s.logger.Error("transport http server error", "error", err)
		}
	}()
	return nil
}

func (s *transportServer) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

func (s *transportServer) URL() string {
	return s.url
}
