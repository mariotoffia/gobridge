package transport

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.TransportFactory = (*Factory)(nil)

// Factory implements ports.TransportFactory for the HTTP transport. It
// creates HTTP source receivers and SSE target senders, exposing them
// via an internal http.ServeMux accessible through Handler().
type Factory struct {
	mux             *http.ServeMux
	pathPrefix      string
	locator         ports.RouteLocator
	forwarder       ports.MessageForwarder
	metrics         ports.MetricsExporter
	logger          *slog.Logger
	clock           clock.Clock
	mu              sync.Mutex
	registeredPaths map[string]bool
}

// FactoryOption configures a Factory.
type FactoryOption func(*Factory)

// WithRouteLocator sets the cluster-aware route locator.
func WithRouteLocator(l ports.RouteLocator) FactoryOption {
	return func(f *Factory) { f.locator = l }
}

// WithMessageForwarder sets the forwarder for cluster message routing.
func WithMessageForwarder(fw ports.MessageForwarder) FactoryOption {
	return func(f *Factory) { f.forwarder = fw }
}

// WithFactoryMetrics sets the metrics exporter.
func WithFactoryMetrics(m ports.MetricsExporter) FactoryOption {
	return func(f *Factory) { f.metrics = m }
}

// WithFactoryLogger sets the structured logger.
func WithFactoryLogger(l *slog.Logger) FactoryOption {
	return func(f *Factory) { f.logger = l }
}

// WithPathPrefix overrides the default URL prefix (default: "/transport/http").
func WithPathPrefix(prefix string) FactoryOption {
	return func(f *Factory) { f.pathPrefix = prefix }
}

func WithClock(clk clock.Clock) FactoryOption {
	return func(f *Factory) {
		if clk != nil {
			f.clock = clk
		}
	}
}

// NewFactory creates an HTTP transport factory.
func NewFactory(opts ...FactoryOption) *Factory {
	f := &Factory{
		mux:             http.NewServeMux(),
		pathPrefix:      "/transport/http",
		registeredPaths: make(map[string]bool),
	}
	for _, o := range opts {
		o(f)
	}
	if f.clock == nil {
		f.clock = clock.System
	}
	return f
}

// NewSession returns (nil, nil) because HTTP transport is stateless.
func (f *Factory) NewSession(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
	return nil, nil
}

func httpConfig(c ports.PluginConfig) (Config, error) {
	if c == nil {
		return Config{}, nil
	}
	switch v := c.(type) {
	case *Config:
		if v == nil {
			return Config{}, nil
		}
		return *v, nil
	case Config:
		return v, nil
	default:
		return Config{}, fmt.Errorf("http transport: expected Config, got %T", c)
	}
}

// NewReceiver creates an HTTP POST handler that converts requests to deliveries.
func (f *Factory) NewReceiver(_ context.Context, spec ports.ReceiverSpec, _ ports.Session) (ports.Receiver, error) {
	cfg, err := httpConfig(spec.Config)
	if err != nil {
		return nil, err
	}
	path := cfg.Path
	if path == "" {
		path = f.pathPrefix + "/receivers/" + spec.ID + "/messages"
	}

	recv := newReceiver(receiverConfig{
		id:          spec.ID,
		path:        path,
		maxBodySize: cfg.effectiveMaxBody(),
		apiKey:      cfg.APIKey.Reveal(),
		locator:     f.locator,
		forwarder:   f.forwarder,
		metrics:     f.metrics,
		logger:      f.logger,
		clock:       f.clock,
	})

	pattern := "POST " + path
	f.mu.Lock()
	if f.registeredPaths[pattern] {
		f.mu.Unlock()
		return nil, fmt.Errorf("http transport: duplicate receiver path %q", path)
	}
	f.registeredPaths[pattern] = true
	f.mux.HandleFunc(pattern, recv.ServeHTTP)
	f.mu.Unlock()

	return recv, nil
}

// NewSender creates an SSE sender that streams events to connected clients.
func (f *Factory) NewSender(_ context.Context, spec ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	cfg, err := httpConfig(spec.Config)
	if err != nil {
		return nil, err
	}
	if mode := cfg.effectiveMode(); mode != "sse" {
		return nil, fmt.Errorf("http transport: unsupported sender mode %q (only \"sse\" supported)", mode)
	}

	path := cfg.Path
	if path == "" {
		path = f.pathPrefix + "/senders/" + spec.ID + "/events"
	}

	sender := newSSESender(sseSenderConfig{
		id:                spec.ID,
		path:              path,
		heartbeatInterval: cfg.effectiveHeartbeat(),
		maxClients:        cfg.MaxClients,
		apiKey:            cfg.APIKey.Reveal(),
		locator:           f.locator,
		metrics:           f.metrics,
		logger:            f.logger,
		clock:             f.clock,
	})

	pattern := "GET " + path
	f.mu.Lock()
	if f.registeredPaths[pattern] {
		f.mu.Unlock()
		return nil, fmt.Errorf("http transport: duplicate sender path %q", path)
	}
	f.registeredPaths[pattern] = true
	f.mux.HandleFunc(pattern, sender.ServeHTTP)
	f.mu.Unlock()

	return sender, nil
}

// Capabilities returns the transport capabilities.
func (f *Factory) Capabilities() []ports.Capability {
	return []ports.Capability{ports.CapHTTPEndpoint}
}

// AddressValidator returns nil — HTTP egress targets are plain URL
// strings and are validated by the underlying http.Client when the
// request is built.
func (f *Factory) AddressValidator() ports.AddressValidator { return nil }

// Handler returns the HTTP handler for all registered transport endpoints.
func (f *Factory) Handler() http.Handler {
	return f.mux
}

// PathPrefix returns the URL prefix for this transport.
func (f *Factory) PathPrefix() string {
	return f.pathPrefix
}
