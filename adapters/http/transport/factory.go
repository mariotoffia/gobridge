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
var _ ports.ContextCloser = (*Factory)(nil)

// Factory implements ports.TransportFactory for the HTTP transport. It
// creates HTTP source receivers and SSE target senders, exposing them
// via an internal http.ServeMux accessible through Handler().
type Factory struct {
	mux             *http.ServeMux
	pathPrefix      string
	locator         ports.RouteLocator
	forwarder       ports.MessageForwarder
	forwardToken    string
	metrics         ports.MetricsExporter
	logger          *slog.Logger
	clock           clock.Clock
	mu              sync.Mutex
	registeredPaths map[string]bool
	sseSenders      []*SSESender
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

// WithForwardToken sets the shared internal forwarding secret that
// receivers require before trusting an X-Bridge-Forwarded marker. It
// MUST match the ForwardToken configured on the peer HTTPForwarder.
// Empty (the default) means receivers never trust forwarded state, so a
// spoofed marker cannot force local processing on a non-owner node.
func WithForwardToken(token string) FactoryOption {
	return func(f *Factory) { f.forwardToken = token }
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
	// Validate the EFFECTIVE path (configured or generated) so a bad
	// operator path — or a spec ID carrying ServeMux metacharacters —
	// fails the build with an error instead of panicking the process
	// inside mux.HandleFunc (HTTP-M4).
	if err := validateMountPath(path); err != nil {
		return nil, err
	}

	recv := newReceiver(receiverConfig{
		id:           spec.ID,
		path:         path,
		maxBodySize:  cfg.effectiveMaxBody(),
		apiKey:       cfg.APIKey.Reveal(),
		forwardToken: f.forwardToken,
		dedupWindow:  cfg.effectiveDedupWindow(),
		locator:      f.locator,
		forwarder:    f.forwarder,
		metrics:      f.metrics,
		logger:       f.logger,
		clock:        f.clock,
	})

	if err := f.registerHandler("POST "+path, path, recv.ServeHTTP); err != nil {
		return nil, err
	}

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
	// See NewReceiver: fail the build instead of panicking ServeMux.
	if err := validateMountPath(path); err != nil {
		return nil, err
	}

	sender := newSSESender(sseSenderConfig{
		id:                spec.ID,
		path:              path,
		heartbeatInterval: cfg.effectiveHeartbeat(),
		writeTimeout:      cfg.effectiveWriteTimeout(),
		maxClients:        cfg.MaxClients,
		apiKey:            cfg.APIKey.Reveal(),
		redirectEndpoint:  cfg.RedirectEndpoint,
		locator:           f.locator,
		metrics:           f.metrics,
		logger:            f.logger,
		clock:             f.clock,
	})

	if err := f.registerHandler("GET "+path, path, sender.ServeHTTP); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.sseSenders = append(f.sseSenders, sender)
	f.mu.Unlock()

	return sender, nil
}

// registerHandler mounts a handler on the internal mux, mapping the two
// build-time failure modes to errors: duplicate registration and a
// ServeMux pattern panic. The recover is defense in depth behind
// validateMountPath — ServeMux panics on malformed or conflicting
// patterns, and an operator-supplied path must fail the BUILD, never
// crash the process.
func (f *Factory) registerHandler(pattern, path string, h http.HandlerFunc) (err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registeredPaths[pattern] {
		return fmt.Errorf("http transport: duplicate path %q", path)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("http transport: cannot register path %q: %v", path, r)
		}
	}()
	f.mux.HandleFunc(pattern, h)
	f.registeredPaths[pattern] = true
	return nil
}

// Close drains every SSE sender this factory created (unblocking their
// long-lived client handlers) so a fronting http.Server.Shutdown can
// complete instead of hanging on open event streams. It satisfies
// ports.ContextCloser; the composition root must call it BEFORE
// http.Server.Shutdown. Safe to call multiple times.
func (f *Factory) Close(ctx context.Context) error {
	f.mu.Lock()
	senders := make([]*SSESender, len(f.sseSenders))
	copy(senders, f.sseSenders)
	f.mu.Unlock()
	var firstErr error
	for _, s := range senders {
		if err := s.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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
