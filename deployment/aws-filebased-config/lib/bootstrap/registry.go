package bootstrap

import (
	"net/http"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

type factoryRegistry struct {
	cfg        *ports.BridgeConfig
	builder    *bridge.Builder
	transports map[string]ports.TransportFactory
	stores     map[string]ports.StoreFactory
	http       *httptransport.Factory
}

func (a *App) newFactoryRegistry(runtimeCfg *ports.BridgeConfig) *factoryRegistry {
	var opts []bridge.BuilderOption
	if a.logger != nil {
		opts = append(opts, bridge.WithLogger(a.logger))
	}
	if a.credentialStore != nil {
		// WithPolledCredentialStore registers the store BOTH as the pull
		// store (synchronous initial resolve at session construction) and as
		// the polled source of a runtime-owned PushCredentialStore, so
		// credential rotation reaches long-lived transport sessions. The
		// zero PollBasedWrapperConfig selects the default poll interval;
		// the production store (runtime.CredentialResolver) exposes
		// ResolveUncached, so every poll bypasses its TTL cache. This
		// profile has no push-capable credential backend (SSM is pull-only),
		// so no WithPushCredentialStore is registered.
		opts = append(opts, bridge.WithPolledCredentialStore(a.credentialStore, ports.PollBasedWrapperConfig{}))
	}
	if a.metricsExporter != nil {
		opts = append(opts, bridge.WithMetrics(a.metricsExporter))
	}
	// Runtime audit logger: lease transitions and DLQ mutations audit through
	// the same slog logger the App owns (Noop otherwise — audit silently lost).
	// The httpapi server wires its own SlogAuditLogger for admin-API audit;
	// this one covers the runtime side (bridge forwards it via
	// runtime.WithAuditLogger). The intermediate interface-typed variable is
	// load-bearing: the bridge layer must only ever see ports.AuditLogger,
	// and the architecture lint's dependency-injection scan rejects a
	// composition-root concrete type injected into the bridge component.
	if a.logger != nil {
		var auditLogger ports.AuditLogger = newSlogAuditLogger(a.logger)
		opts = append(opts, bridge.WithAuditLogger(auditLogger))
	}
	// Tracing is intentionally NOT wired in this profile: BootstrapConfig has
	// no traces-exporter surface and the CDK constructs provision no OTLP
	// collector, so adapters/otel/tracing would ship dead config plus its
	// full OTel dependency tree. A future traces_exporter selection would be
	// wired here via bridge.WithTracer.
	builder := bridge.NewBuilder(runtimeCfg, opts...)

	transports := map[string]ports.TransportFactory{
		"mqtt": paho.NewFactory(a.logger),
		"sqs":  sqsadapter.NewFactory(a.logger),
	}

	// The metrics exporter reaches HTTP receivers/SSE senders through the
	// factory (nil keeps the adapter's internal noop fallback). Forwarder,
	// forward token, and route locator stay unwired on purpose: they exist
	// for cluster-internal message routing, which this profile forbids
	// (validateFilesystemProfile rejects route.session / shared_outbox) and
	// BootstrapConfig carries no peer or forward-token surface.
	httpOpts := []httptransport.FactoryOption{httptransport.WithFactoryLogger(a.logger)}
	if a.metricsExporter != nil {
		httpOpts = append(httpOpts, httptransport.WithFactoryMetrics(a.metricsExporter))
	}
	httpFactory := httptransport.NewFactory(httpOpts...)
	transports["http"] = httpFactory

	for name, factory := range transports {
		builder.RegisterTransportFactory(name, factory)
	}
	stores := map[string]ports.StoreFactory{
		"memory":              nativestore.NewMemoryStoreFactory(),
		"sqlite":              nativestore.NewSQLiteStoreFactory(),
		awsstore.DynamoDBKind: awsstore.NewDynamoDBStoreFactory(a.dynamoDBClient),
	}
	for name, factory := range stores {
		builder.RegisterStoreFactory(name, factory)
	}

	return &factoryRegistry{
		cfg:        runtimeCfg,
		builder:    builder,
		transports: transports,
		stores:     stores,
		http:       httpFactory,
	}
}

func (r *factoryRegistry) detectSwapMode(cfg *ports.BridgeConfig) swapMode {
	for _, session := range cfg.Sessions {
		factory, ok := r.transports[session.Transport]
		if !ok {
			continue
		}
		for _, capability := range factory.Capabilities() {
			if capability == ports.CapExclusiveIdentity {
				return swapModePrepareCommit
			}
		}
	}
	return swapModeOverlap
}

func (r *factoryRegistry) transportHandler() http.Handler {
	if r.http == nil || !hasHTTPTransportEndpoints(r.cfg) {
		return http.NotFoundHandler()
	}
	return r.http.Handler()
}
