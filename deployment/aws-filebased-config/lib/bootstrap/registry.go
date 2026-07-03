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
		opts = append(opts, bridge.WithCredentialStore(a.credentialStore))
	}
	if a.metricsExporter != nil {
		opts = append(opts, bridge.WithMetrics(a.metricsExporter))
	}
	builder := bridge.NewBuilder(runtimeCfg, opts...)

	transports := map[string]ports.TransportFactory{
		"mqtt": paho.NewFactory(a.logger),
		"sqs":  sqsadapter.NewFactory(a.logger),
	}

	httpFactory := httptransport.NewFactory(httptransport.WithFactoryLogger(a.logger))
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
