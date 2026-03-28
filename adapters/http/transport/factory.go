package transport

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ bridge.TransportFactory = (*BridgeFactory)(nil)
	_ bridge.HTTPMountable    = (*BridgeFactory)(nil)
)

// BridgeFactory implements bridge.TransportFactory for the HTTP transport.
// It creates HTTP source receivers and SSE target senders, exposing them
// via an internal http.ServeMux accessible through Handler().
type BridgeFactory struct {
	mux             *http.ServeMux
	pathPrefix      string
	locator         ports.RouteLocator
	forwarder       ports.MessageForwarder
	metrics         ports.MetricsExporter
	logger          *slog.Logger
	mu              sync.Mutex
	registeredPaths map[string]bool
}

// FactoryOption configures a BridgeFactory.
type FactoryOption func(*BridgeFactory)

// WithRouteLocator sets the cluster-aware route locator.
func WithRouteLocator(l ports.RouteLocator) FactoryOption {
	return func(f *BridgeFactory) { f.locator = l }
}

// WithMessageForwarder sets the forwarder for cluster message routing.
func WithMessageForwarder(fw ports.MessageForwarder) FactoryOption {
	return func(f *BridgeFactory) { f.forwarder = fw }
}

// WithFactoryMetrics sets the metrics exporter.
func WithFactoryMetrics(m ports.MetricsExporter) FactoryOption {
	return func(f *BridgeFactory) { f.metrics = m }
}

// WithFactoryLogger sets the structured logger.
func WithFactoryLogger(l *slog.Logger) FactoryOption {
	return func(f *BridgeFactory) { f.logger = l }
}

// WithPathPrefix overrides the default URL prefix (default: "/transport/http").
func WithPathPrefix(prefix string) FactoryOption {
	return func(f *BridgeFactory) { f.pathPrefix = prefix }
}

// NewBridgeFactory creates an HTTP transport factory.
func NewBridgeFactory(opts ...FactoryOption) *BridgeFactory {
	f := &BridgeFactory{
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
func (f *BridgeFactory) NewSession(_ context.Context, _ config.SessionDef) (ports.Session, error) {
	return nil, nil
}

// NewReceiver creates an HTTP POST handler that converts requests to deliveries.
func (f *BridgeFactory) NewReceiver(_ context.Context, def config.ReceiverDef, _ ports.Session) (ports.Receiver, error) {
	path := optStr(def.Options, "path", "")
	if path == "" {
		path = f.pathPrefix + "/receivers/" + def.ID + "/messages"
	}
	apiKey := optStr(def.Options, "api_key", "")
	maxBody := optInt64(def.Options, "max_body_size", 1<<20)

	recv := newReceiver(receiverConfig{
		id:          def.ID,
		path:        path,
		maxBodySize: maxBody,
		apiKey:      apiKey,
		locator:     f.locator,
		forwarder:   f.forwarder,
		metrics:     f.metrics,
		logger:      f.logger,
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
func (f *BridgeFactory) NewSender(_ context.Context, def config.SenderDef, _ ports.Session) (ports.Sender, error) {
	mode := optStr(def.Options, "mode", "sse")
	if mode != "sse" {
		return nil, fmt.Errorf("http transport: unsupported sender mode %q (only \"sse\" supported)", mode)
	}

	path := optStr(def.Options, "path", "")
	if path == "" {
		path = f.pathPrefix + "/senders/" + def.ID + "/events"
	}

	heartbeat := optDuration(def.Options, "heartbeat_interval", 30*time.Second)

	apiKey := optStr(def.Options, "api_key", "")
	maxClients := int(optInt64(def.Options, "max_clients", 0))

	sender := newSSESender(sseSenderConfig{
		id:                def.ID,
		path:              path,
		heartbeatInterval: heartbeat,
		maxClients:        maxClients,
		apiKey:            apiKey,
		locator:           f.locator,
		metrics:           f.metrics,
		logger:            f.logger,
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
func (f *BridgeFactory) Capabilities() []ports.Capability {
	return []ports.Capability{ports.CapHTTPEndpoint}
}

// Handler returns the HTTP handler for all registered transport endpoints.
func (f *BridgeFactory) Handler() http.Handler {
	return f.mux
}

// PathPrefix returns the URL prefix for this transport.
func (f *BridgeFactory) PathPrefix() string {
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
	if s, ok := v.(string); ok {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	return fallback
}
