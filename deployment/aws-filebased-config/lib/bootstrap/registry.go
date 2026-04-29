package bootstrap

import (
	"net/http"

	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
)

type factoryRegistry struct {
	cfg        *config.BridgeConfig
	builder    *bridge.Builder
	transports map[string]ports.TransportFactory
	http       *httptransport.Factory
}

func (a *App) newFactoryRegistry(runtimeCfg *config.BridgeConfig) *factoryRegistry {
	var opts []bridge.BuilderOption
	if a.logger != nil {
		opts = append(opts, bridge.WithLogger(a.logger))
	}
	if a.credentialStore != nil {
		opts = append(opts, bridge.WithCredentialStore(a.credentialStore))
	}
	builder := bridge.NewBuilder(runtimeCfg, opts...)

	transports := map[string]ports.TransportFactory{
		"mqtt": paho.NewFactory(a.logger),
		"sqs":  sqsadapter.NewFactory(a.logger),
	}

	httpFactory := httptransport.NewFactory(httptransport.WithFactoryLogger(a.logger))
	transports["http"] = httpFactory

	for name, factory := range transports {
		builder.RegisterTransport(name, factory)
	}
	builder.RegisterStoreFactory("memory", nativestore.NewMemoryStoreFactory())
	builder.RegisterStoreFactory("sqlite", nativestore.NewSQLiteStoreFactory())

	return &factoryRegistry{
		cfg:        runtimeCfg,
		builder:    builder,
		transports: transports,
		http:       httpFactory,
	}
}

func (r *factoryRegistry) detectSwapMode(cfg *config.BridgeConfig) swapMode {
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
