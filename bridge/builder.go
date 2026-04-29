package bridge

import (
	"log/slog"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// Builder constructs a runtime.Runtime from a declarative BridgeConfig.
type Builder struct {
	cfg              *config.BridgeConfig
	transports       map[string]ports.TransportFactory
	storeFactories   map[string]ports.StoreFactory
	processors       map[string]ports.Processor
	logger           *slog.Logger
	credStore        ports.CredentialStore
	pushCredStore    ports.PushCredentialStore
	endpointResolver ports.EndpointResolver
	hook             ports.DeliveryHook
}

// BuilderOption configures a Builder.
type BuilderOption func(*Builder)

// WithLogger sets the logger for the builder and the resulting runtime.
func WithLogger(l *slog.Logger) BuilderOption {
	return func(b *Builder) { b.logger = l }
}

// WithCredentialStore sets the pull-style credential store used to
// resolve credentials_uri references in session, receiver, and sender
// options at Build time. This is the historical entry point and is
// equivalent to WithPullCredentialStore.
func WithCredentialStore(cs ports.CredentialStore) BuilderOption {
	return func(b *Builder) { b.credStore = cs }
}

// WithPullCredentialStore is an explicit name for WithCredentialStore.
// Provided alongside WithPushCredentialStore so callers can be explicit
// about intent when both stores are in play.
func WithPullCredentialStore(cs ports.PullCredentialStore) BuilderOption {
	return func(b *Builder) { b.credStore = cs }
}

// WithPushCredentialStore registers an observable credential store that
// the runtime can subscribe to for rotation events. When set, the
// composition root will spawn a watcher per credentials_uri found in
// session options and apply new credentials to the owning transport
// session (currently MQTT).
//
// A push store does NOT replace the pull store — the pull store is
// still required for the initial resolve that happens synchronously
// during session construction. Typically callers register the same
// backing store for both, or wrap a pull store via
// WithPolledCredentialStore.
func WithPushCredentialStore(cs ports.PushCredentialStore) BuilderOption {
	return func(b *Builder) { b.pushCredStore = cs }
}

// WithPolledCredentialStore wraps a pull store in a runtime-owned
// poll-based PushCredentialStore and registers BOTH the pull and push
// sides on the builder. This is the easy-mode option for users whose
// credential backend only supports pull semantics (file, SSM, Vault KV v1).
//
// Why register both: the builder still needs a pull store for the
// synchronous initial resolve during session construction, and the
// runtime needs a push store to observe rotations. A single call
// configures them consistently.
func WithPolledCredentialStore(cs ports.PullCredentialStore, cfg ports.PollBasedWrapperConfig) BuilderOption {
	return func(b *Builder) {
		b.credStore = cs
		b.pushCredStore = runtime.NewPollBasedWrapper(cs, cfg, runtime.WithPollLogger(b.logger))
	}
}

// NewBuilder creates a builder from the given configuration.
func NewBuilder(cfg *config.BridgeConfig, opts ...BuilderOption) *Builder {
	b := &Builder{
		cfg:            cfg,
		transports:     make(map[string]ports.TransportFactory),
		storeFactories: make(map[string]ports.StoreFactory),
		processors:     make(map[string]ports.Processor),
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// RegisterTransport registers a transport factory under the given name
// (e.g. "mqtt", "sqs"). Returns the builder for chaining.
func (b *Builder) RegisterTransport(name string, factory ports.TransportFactory) *Builder {
	b.transports[name] = factory
	return b
}

// RegisterStoreFactory registers a store factory under the given name
// (e.g. "dynamodb", "memory", "sqlite"). Returns the builder for chaining.
func (b *Builder) RegisterStoreFactory(name string, factory ports.StoreFactory) *Builder {
	b.storeFactories[name] = factory
	return b
}

// RegisterProcessor registers a named processor that can be referenced
// from route definitions. Returns the builder for chaining.
func (b *Builder) RegisterProcessor(name string, proc ports.Processor) *Builder {
	b.processors[name] = proc
	return b
}

// RegisterEndpointResolver sets a custom EndpointResolver for cluster
// endpoint discovery. When not set, the builder auto-detects the resolver
// based on the runtime environment.
func (b *Builder) RegisterEndpointResolver(r ports.EndpointResolver) *Builder {
	b.endpointResolver = r
	return b
}

// RegisterDeliveryHook sets a hook that observes message delivery lifecycle
// events (ingress receive, egress send attempts, and final outcomes).
// Returns the builder for chaining.
func (b *Builder) RegisterDeliveryHook(h ports.DeliveryHook) *Builder {
	b.hook = h
	return b
}
