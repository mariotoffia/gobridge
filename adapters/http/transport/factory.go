package transport

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

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
	return func(f *Factory) { f.clock = clk }
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
	return f
}

// NewSession returns (nil, nil) because HTTP transport is stateless.
func (f *Factory) NewSession(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
	return nil, nil
}

// NewReceiver creates an HTTP POST handler that converts requests to deliveries.
func (f *Factory) NewReceiver(_ context.Context, spec ports.ReceiverSpec, _ ports.Session) (ports.Receiver, error) {
	path := optStr(spec.Options, "path", "")
	if path == "" {
		path = f.pathPrefix + "/receivers/" + spec.ID + "/messages"
	}
	apiKey := optStr(spec.Options, "api_key", "")
	maxBody := optInt64(spec.Options, "max_body_size", 1<<20)

	recv := newReceiver(receiverConfig{
		id:          spec.ID,
		path:        path,
		maxBodySize: maxBody,
		apiKey:      apiKey,
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
	mode := optStr(spec.Options, "mode", "sse")
	if mode != "sse" {
		return nil, fmt.Errorf("http transport: unsupported sender mode %q (only \"sse\" supported)", mode)
	}

	path := optStr(spec.Options, "path", "")
	if path == "" {
		path = f.pathPrefix + "/senders/" + spec.ID + "/events"
	}

	heartbeat := optDuration(spec.Options, "heartbeat_interval", 30*time.Second)

	apiKey := optStr(spec.Options, "api_key", "")
	maxClients := int(optInt64(spec.Options, "max_clients", 0))

	sender := newSSESender(sseSenderConfig{
		id:                spec.ID,
		path:              path,
		heartbeatInterval: heartbeat,
		maxClients:        maxClients,
		apiKey:            apiKey,
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

// Handler returns the HTTP handler for all registered transport endpoints.
func (f *Factory) Handler() http.Handler {
	return f.mux
}

// PathPrefix returns the URL prefix for this transport.
func (f *Factory) PathPrefix() string {
	return f.pathPrefix
}

func optStr(opts map[string]any, key, fallback string) string {
	if opts == nil {
		return fallback
	}
	if v, ok := opts[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return fallback
}

func optInt64(opts map[string]any, key string, fallback int64) int64 {
	if opts == nil {
		return fallback
	}
	v, ok := opts[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	default:
		return fallback
	}
}

func optDuration(opts map[string]any, key string, fallback time.Duration) time.Duration {
	if opts == nil {
		return fallback
	}
	v, ok := opts[key]
	if !ok {
		return fallback
	}
	switch d := v.(type) {
	case time.Duration:
		if d < 0 {
			return fallback
		}
		return d
	case string:
		parsed, err := time.ParseDuration(d)
		if err != nil || parsed < 0 {
			return fallback
		}
		return parsed
	case int:
		if d < 0 {
			return fallback
		}
		return time.Duration(d) * time.Second
	case int64:
		if d < 0 {
			return fallback
		}
		return time.Duration(d) * time.Second
	case float64:
		if d < 0 || math.IsNaN(d) || math.IsInf(d, 0) {
			return fallback
		}
		return time.Duration(d * float64(time.Second))
	default:
		return fallback
	}
}
