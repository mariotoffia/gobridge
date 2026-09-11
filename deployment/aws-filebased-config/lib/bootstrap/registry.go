package bootstrap

import (
	"context"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"

	ecscluster "github.com/mariotoffia/gobridge/adapters/aws/cluster/ecs"
	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
)

// ensureDynamoDBClient shares the existing AWS construction path between config
// loading and HA stores, preserving any client the embedding process injected.
func (a *App) ensureDynamoDBClient(ctx context.Context) error {
	if a.dynamoDBClient != nil {
		return nil
	}
	client, err := newDynamoDBClient(ctx, a.cfg)
	if err != nil {
		return err
	}
	a.dynamoDBClient = client
	return nil
}

// Streams has its own service endpoint in AWS. Copy only the shared client
// settings; a supplied BaseEndpoint keeps local emulation on the same backend.
func newDynamoDBStreamsClient(client *dynamodb.Client) *dynamodbstreams.Client {
	opts := client.Options()
	return dynamodbstreams.New(dynamodbstreams.Options{
		Region: opts.Region, Credentials: opts.Credentials,
		HTTPClient: opts.HTTPClient, Retryer: opts.Retryer,
		BaseEndpoint: opts.BaseEndpoint,
	})
}

type factoryRegistry struct {
	cfg        *ports.BridgeConfig
	builder    *bridge.Builder
	transports map[string]ports.TransportFactory
	stores     map[string]ports.StoreFactory
	http       *httptransport.Factory
}

func (a *App) newFactoryRegistry(runtimeCfg *ports.BridgeConfig) *factoryRegistry {
	// Blueprint validation on EVERY build this root performs. The config manager
	// validates what it emits, but the coordinated rollout paths build configs the
	// manager never emitted — the vote's candidate and the bytes decoded from the
	// durable committed artifact — and those reached the builder unvalidated. A
	// dangling reference in a candidate must Nack at the vote; discovering it after
	// the cohort commits fails every member at once.
	opts := []bridge.BuilderOption{bridge.WithBlueprintValidator(config.Validate)}
	if a.logger != nil {
		opts = append(opts, bridge.WithLogger(a.logger))
	}
	if a.credentialStore != nil {
		// WithPolledCredentialStore registers the store BOTH as the pull
		// store (synchronous initial resolve at session construction) and as
		// the polled source of a runtime-owned PushCredentialStore, so
		// credential rotation reaches long-lived transport sessions. The
		// production store (runtime.CredentialResolver) exposes
		// ResolveUncached, so every poll bypasses its TTL cache.
		//
		// The poll config is now driven by BootstrapConfig instead
		// of a zero-value: EmitOnStart defaults true so a rotation that landed
		// in the build->watch window is surfaced on the first tick rather than
		// silently baselined; PollInterval is operator-tunable to
		// shrink hard-rotation auth-failure downtime; Jitter defaults to ~10%
		// of the interval so a fleet does not stampede the credential backend
		// on the same tick (LOW-severity finding).
		credPollCfg := ports.PollBasedWrapperConfig{
			PollInterval: a.cfg.EffectiveCredentialPollInterval(),
			Jitter:       a.cfg.EffectiveCredentialPollJitter(),
			EmitOnStart:  a.cfg.EffectiveCredentialEmitOnStart(),
		}
		opts = append(opts, bridge.WithPolledCredentialStore(a.credentialStore, credPollCfg))
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
	if runtimeCfg != nil && runtimeCfg.Bridge.DeploymentMode == "clustered" {
		builder.RegisterEndpointResolver(ecscluster.NewEcsEndpointResolver(ecscluster.WithLogger(a.logger)))
	}

	// The metrics exporter is threaded into the MQTT and SQS transport
	// factories (nil keeps each adapter's internal Noop fallback) so their
	// self-instrumented metrics actually emit on this config-driven path —
	// previously both factories were constructed logger-only, leaving every
	// SQS and MQTT adapter metric dead (the paho factory carried the same
	// dead-metrics wiring bug). Mirrors the HTTP factory wiring
	// below.
	mqttFactory := paho.NewFactory(a.logger, a.metricsExporter)
	sqsFactory := sqsadapter.NewFactory(a.logger, a.metricsExporter)
	// Keep factory keys symmetric with the canonical decoder aliases registered
	// by paho.Register and sqsadapter.Register. Both names resolve to the same
	// factory instance; no duplicate transport/session state is created.
	transports := map[string]ports.TransportFactory{
		paho.ShortKind: mqttFactory, paho.QualifiedKind: mqttFactory,
		sqsadapter.ShortKind: sqsFactory, sqsadapter.QualifiedKind: sqsFactory,
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

	// Optional families (AMQP 0-9-1, AMQP 1.0, Azure Service Bus) are selected
	// at build time and extend the same map before it is registered, so an
	// alias they add reaches the builder through the single loop below and is
	// visible to detectSwapMode like any base-set transport.
	wireOptionalTransports(transports, a.logger, a.metricsExporter)

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

// detectSwapMode asks the same question the Supervisor asks, through the same
// predicate: this root drives its own swap, so a probe added there must not
// have to be re-added here. current is the running config and next the one
// replacing it: an exclusive identity the running config holds on a transport
// next still uses conflicts with next as surely as one next claims itself.
func (r *factoryRegistry) detectSwapMode(current, next *ports.BridgeConfig) swapMode {
	if bridge.RequiresSerializedSwap(current, next, r.transports) {
		return swapModePrepareCommit
	}
	return swapModeOverlap
}

func (r *factoryRegistry) transportHandler() http.Handler {
	if r.http == nil || !hasHTTPTransportEndpoints(r.cfg) {
		return http.NotFoundHandler()
	}
	return r.http.Handler()
}
