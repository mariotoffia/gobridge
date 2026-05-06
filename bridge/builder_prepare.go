package bridge

import (
	"context"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// PreparedBuild holds pre-validated state from the prepare phase.
// No transport sessions, receivers, or senders have been created yet,
// making it safe to call Prepare while an old runtime still holds
// exclusive transport connections.
type PreparedBuild struct {
	cfg    *ports.BridgeConfig
	stores *storeResult
	rtOpts []runtime.Option
}

// Prepare builds stores but does NOT create transport sessions,
// receivers, or senders. This is the first phase of the two-phase
// build used by the Supervisor in PrepareCommit mode.
//
// When a ports.BlueprintValidator was supplied via
// WithBlueprintValidator, it runs first; the builder does not import
// the config parser, so the composition root injects the validator
// (typically config.Validate). When no validator is set the bridge
// trusts the input.
func (b *Builder) Prepare(ctx context.Context) (*PreparedBuild, error) {
	if b.validator != nil {
		if err := b.validator(b.cfg); err != nil {
			return nil, fmt.Errorf("bridge: config validation: %w", err)
		}
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

func isDistributedFactory(sf ports.StoreFactory) bool {
	if df, ok := sf.(ports.DistributedStoreFactory); ok {
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
		s, err := sf.NewLeaseStore(ctx, sc.Config)
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
		runtimeOpts, err := b.outboxRuntimeOptions(sc)
		if err != nil {
			return nil, err
		}
		s, err := sf.NewOutboxStore(ctx, sc.Config, runtimeOpts)
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
		s, err := sf.NewDLQStore(ctx, sc.Config)
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

// outboxRuntimeOptions derives the runtime tuning passed to outbox
// store factories. StaleClaimDuration is sourced in this priority:
//
//  1. an explicit `stale_claim_duration` entry in the outbox YAML
//     options (read via StoreConfig.Raw()) — supports either a
//     duration string ("2m") or a time.Duration value;
//  2. a value derived from the maximum session step-down grace
//     across all routes, plus a buffer.
//
// The derivation keeps the outbox reclaim timeout aligned with the
// lease lifecycle without forcing every plugin config schema to
// carry the runtime knob.
func (b *Builder) outboxRuntimeOptions(sc *ports.StoreConfig) (ports.OutboxRuntimeOptions, error) {
	if sc == nil {
		return ports.OutboxRuntimeOptions{}, nil
	}

	if explicit, ok, err := explicitStaleClaimDuration(sc); err != nil {
		return ports.OutboxRuntimeOptions{}, err
	} else if ok {
		return ports.OutboxRuntimeOptions{StaleClaimDuration: explicit}, nil
	}

	maxStepDownGrace := runtime.DefaultSessionConfig("", true).StepDownGrace
	for _, r := range b.cfg.Routes {
		if r.Session == nil {
			continue
		}
		sessCfg, err := toSessionConfigE(r.Session)
		if err != nil {
			return ports.OutboxRuntimeOptions{}, fmt.Errorf("bridge: route %q: %w", r.ID, err)
		}
		if sessCfg != nil && sessCfg.StepDownGrace > maxStepDownGrace {
			maxStepDownGrace = sessCfg.StepDownGrace
		}
	}

	staleClaimBuffer := max(2*maxStepDownGrace, 15*time.Second)
	return ports.OutboxRuntimeOptions{
		StaleClaimDuration: maxStepDownGrace + staleClaimBuffer,
	}, nil
}

// explicitStaleClaimDuration looks for a user-provided override in
// the outbox blueprint's raw stage-1 options. It returns ok=false
// when the override is absent.
func explicitStaleClaimDuration(sc *ports.StoreConfig) (time.Duration, bool, error) {
	raw := sc.Raw()
	if raw == nil {
		return 0, false, nil
	}
	var probe struct {
		StaleClaimDuration any `mapstructure:"stale_claim_duration" yaml:"stale_claim_duration" json:"stale_claim_duration"`
	}
	if err := raw.Decode(&probe); err != nil {
		return 0, false, nil
	}
	switch v := probe.StaleClaimDuration.(type) {
	case nil:
		return 0, false, nil
	case time.Duration:
		return v, true, nil
	case string:
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, false, fmt.Errorf("bridge: outbox stale_claim_duration: invalid duration %q: %w", v, err)
		}
		return d, true, nil
	default:
		return 0, false, fmt.Errorf("bridge: outbox stale_claim_duration: must be a duration string or time.Duration, got %T", v)
	}
}
