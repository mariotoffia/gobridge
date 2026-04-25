package bridge

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// PreparedBuild holds pre-validated state from the prepare phase.
// No transport sessions, receivers, or senders have been created yet,
// making it safe to call Prepare while an old runtime still holds
// exclusive transport connections.
type PreparedBuild struct {
	cfg    *config.BridgeConfig
	stores *storeResult
	rtOpts []runtime.Option
}

// Prepare validates config and builds stores but does NOT create
// transport sessions, receivers, or senders. This is the first phase
// of the two-phase build used by the Supervisor in PrepareCommit mode.
func (b *Builder) Prepare(ctx context.Context) (*PreparedBuild, error) {
	if err := config.Validate(b.cfg); err != nil {
		return nil, fmt.Errorf("bridge: config validation: %w", err)
	}

	stores, err := b.buildStores(ctx)
	if err != nil {
		return nil, err
	}

	rtOpts := []runtime.Option{
		runtime.WithLeaseStore(stores.lease),
		runtime.WithOutboxStore(stores.outbox),
		runtime.WithDLQStore(stores.dlq),
	}
	if b.cfg.Bridge.InstanceID != "" {
		rtOpts = append(rtOpts, runtime.WithInstanceID(b.cfg.Bridge.InstanceID))
	}
	if b.logger != nil {
		rtOpts = append(rtOpts, runtime.WithLogger(b.logger))
	}
	if b.hook != nil {
		rtOpts = append(rtOpts, runtime.WithDeliveryHook(b.hook))
	}

	endpoints := b.resolveClusterEndpoints(ctx)
	if len(endpoints) > 0 {
		rtOpts = append(rtOpts, runtime.WithClusterEndpoints(endpoints))
	}

	return &PreparedBuild{
		cfg:    b.cfg,
		stores: stores,
		rtOpts: rtOpts,
	}, nil
}

// Build validates the configuration, creates all adapters via registered
// factories, and wires them into a runtime.Runtime. The returned runtime
// is not yet started; call Start on it separately. If any step fails,
// previously created sessions are closed to prevent resource leaks.
func (b *Builder) Build(ctx context.Context) (*runtime.Runtime, error) {
	prep, err := b.Prepare(ctx)
	if err != nil {
		return nil, err
	}
	return b.Complete(ctx, prep)
}

type storeResult struct {
	lease      ports.LeaseStore
	outbox     ports.OutboxStore
	dlq        ports.DLQStore
	leaseDist  bool
	outboxDist bool
	dlqDist    bool
}

func isDistributedFactory(sf StoreFactory) bool {
	if df, ok := sf.(DistributedStoreFactory); ok {
		return df.IsDistributed()
	}
	return false
}

func (b *Builder) buildStores(ctx context.Context) (*storeResult, error) {
	res := &storeResult{}

	if sc := b.cfg.Stores.Lease; sc != nil {
		sf, ok := b.storeFactories[sc.Type]
		if !ok {
			return nil, fmt.Errorf("bridge: no store factory registered for lease type %q", sc.Type)
		}
		s, err := sf.NewLeaseStore(ctx, *sc)
		if err != nil {
			return nil, fmt.Errorf("bridge: create lease store: %w", err)
		}
		if s == nil {
			return nil, fmt.Errorf("bridge: store factory %q returned nil lease store without error", sc.Type)
		}
		res.lease = s
		res.leaseDist = isDistributedFactory(sf)
	}
	if sc := b.cfg.Stores.Outbox; sc != nil {
		sf, ok := b.storeFactories[sc.Type]
		if !ok {
			return nil, fmt.Errorf("bridge: no store factory registered for outbox type %q", sc.Type)
		}
		if err := b.injectStaleClaimDuration(sc); err != nil {
			return nil, err
		}
		s, err := sf.NewOutboxStore(ctx, *sc)
		if err != nil {
			return nil, fmt.Errorf("bridge: create outbox store: %w", err)
		}
		if s == nil {
			return nil, fmt.Errorf("bridge: store factory %q returned nil outbox store without error", sc.Type)
		}
		res.outbox = s
		res.outboxDist = isDistributedFactory(sf)
	}
	if sc := b.cfg.Stores.DLQ; sc != nil {
		sf, ok := b.storeFactories[sc.Type]
		if !ok {
			return nil, fmt.Errorf("bridge: no store factory registered for dlq type %q", sc.Type)
		}
		s, err := sf.NewDLQStore(ctx, *sc)
		if err != nil {
			return nil, fmt.Errorf("bridge: create dlq store: %w", err)
		}
		if s == nil {
			return nil, fmt.Errorf("bridge: store factory %q returned nil dlq store without error", sc.Type)
		}
		res.dlq = s
		res.dlqDist = isDistributedFactory(sf)
	}

	if b.cfg.Bridge.DeploymentMode == "clustered" {
		if res.lease != nil && !res.leaseDist {
			return nil, fmt.Errorf("bridge: clustered deployment requires a distributed LeaseStore; the configured store is process-local")
		}
		if res.outbox != nil && !res.outboxDist {
			return nil, fmt.Errorf("bridge: clustered deployment requires a distributed OutboxStore; the configured store is process-local")
		}
		if res.dlq != nil && !res.dlqDist {
			return nil, fmt.Errorf("bridge: clustered deployment requires a distributed DLQStore; the configured store is process-local")
		}
	}

	return res, nil
}

func (b *Builder) resolveClusterEndpoints(ctx context.Context) map[string]string {
	if b.cfg.Bridge.Cluster != nil && len(b.cfg.Bridge.Cluster.Endpoints) > 0 {
		return b.cfg.Bridge.Cluster.Endpoints
	}

	if b.endpointResolver != nil {
		listenAddr := ":8080"
		if b.cfg.HTTP != nil && b.cfg.HTTP.AdminAddr != "" {
			listenAddr = b.cfg.HTTP.AdminAddr
		}
		endpoints, err := b.endpointResolver.Resolve(ctx, listenAddr)
		if err != nil {
			if b.logger != nil {
				b.logger.Warn("endpoint resolution failed, cluster routing may be unavailable", "error", err)
			}
			return nil
		}
		return endpoints
	}

	return nil
}

// injectStaleClaimDuration derives stale_claim_duration from the
// session StepDownGrace values across all routes and injects it into
// the outbox store config options. This keeps the outbox reclaim
// timeout aligned with the lease lifecycle rather than being an
// independent hardcoded value. The derivation is skipped when the
// user has explicitly set stale_claim_duration in YAML.
//
// The method works on a shallow copy of sc.Options so that the
// original config is not mutated, allowing safe re-derivation on
// subsequent Build() calls with the same config.
func (b *Builder) injectStaleClaimDuration(sc *config.StoreConfig) error {
	if sc.Options != nil {
		if _, explicit := sc.Options["stale_claim_duration"]; explicit {
			return nil
		}
	}

	maxStepDownGrace := runtime.DefaultSessionConfig("", true).StepDownGrace
	for _, r := range b.cfg.Routes {
		if r.Session == nil {
			continue
		}
		sessCfg, err := toSessionConfigE(r.Session)
		if err != nil {
			return fmt.Errorf("bridge: route %q: %w", r.ID, err)
		}
		if sessCfg != nil && sessCfg.StepDownGrace > maxStepDownGrace {
			maxStepDownGrace = sessCfg.StepDownGrace
		}
	}

	staleClaimBuffer := max(2*maxStepDownGrace, 15*time.Second)

	opts := make(map[string]any, len(sc.Options)+1)
	maps.Copy(opts, sc.Options)
	opts["stale_claim_duration"] = maxStepDownGrace + staleClaimBuffer
	sc.Options = opts
	return nil
}
