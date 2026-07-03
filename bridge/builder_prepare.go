package bridge

import (
	"context"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// preparedBuild holds pre-validated state from the prepare phase.
// No transport sessions, receivers, or senders have been created yet,
// making it safe to construct while an old runtime still holds
// exclusive transport connections.
//
// preparedBuild is unexported on purpose: the only supported public
// entry points are Builder.Build (single-shot) and Builder.Plan +
// BuildPlan.Commit (explicit two-phase). See M-3 / W-7.
type preparedBuild struct {
	cfg    *ports.BridgeConfig
	stores *storeResult
	rtOpts []runtime.Option
}

// BuildPlan is the result of Builder.Plan. It captures all
// pre-validated state (config validation, stores opened, runtime
// options assembled) WITHOUT having opened any transport sessions,
// receivers, or senders yet.
//
// A BuildPlan exists to support the prepare/commit swap pattern used
// by hot-reload code paths that must drain an OLD runtime BEFORE the
// NEW runtime opens exclusive transport resources (e.g. an MQTT
// client-id that may not be held twice). Typical usage:
//
//	plan, err := builder.Plan(ctx)
//	if err != nil { return err }
//	// ... drain / stop the previous runtime ...
//	rt, err := plan.Commit(ctx)
//
// BuildPlan.Commit is one-shot: a second call returns an error. If
// you do not need the explicit two-phase pattern, prefer the simpler
// Builder.Build(ctx).
type BuildPlan struct {
	b         *Builder
	prep      *preparedBuild
	committed bool
}

// Plan runs the prepare phase: validates the configuration, builds
// stores, assembles runtime options. It does NOT open transport
// sessions, receivers, or senders — those are deferred to
// BuildPlan.Commit. This separation lets callers stop and drain a
// previously running runtime between the two phases without holding
// duplicate exclusive transport resources.
//
// When a ports.BlueprintValidator was supplied via
// WithBlueprintValidator, it runs first; the builder does not import
// the config parser, so the composition root injects the validator
// (typically config.Validate). When no validator is set the bridge
// trusts the input.
//
// Most callers should use Build(ctx) instead; Plan is reserved for
// supervisor-style hot-reload orchestration.
func (b *Builder) Plan(ctx context.Context) (*BuildPlan, error) {
	prep, err := b.prepare(ctx)
	if err != nil {
		return nil, err
	}
	return &BuildPlan{b: b, prep: prep}, nil
}

// Commit runs the commit phase: opens transport sessions, receivers,
// and senders, wires routes, and returns a ready-to-start
// *runtime.Runtime (callers must invoke Start themselves).
//
// Commit is one-shot — calling it twice on the same BuildPlan
// returns an error. This guards against accidental double-commit
// when the hot-reload state machine retries.
func (p *BuildPlan) Commit(ctx context.Context) (*runtime.Runtime, error) {
	if p == nil {
		return nil, fmt.Errorf("bridge: BuildPlan.Commit called on nil plan")
	}
	if p.committed {
		return nil, fmt.Errorf("bridge: BuildPlan already committed")
	}
	rt, err := p.b.complete(ctx, p.prep)
	if err != nil {
		return nil, err
	}
	p.committed = true
	return rt, nil
}

// prepare is the internal first phase used by both Build and Plan.
// It is unexported to enforce that callers cannot construct an
// invalid prepare/complete sequence — the public surface is Build
// (single-shot) or Plan/Commit (explicit two-phase).
func (b *Builder) prepare(ctx context.Context) (*preparedBuild, error) {
	if err := runtime.CheckRandSource(); err != nil {
		return nil, fmt.Errorf("bridge: entropy source unavailable: %w", err)
	}

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
	// Forward observability into the runtime so a config-driven deployment
	// exports real metrics/traces/audit instead of the Noop defaults
	// (Finding 15). The Builder/Supervisor previously had no seam to pass
	// these through despite runtime.WithMetrics/WithTracer/WithAuditLogger
	// existing.
	if b.metrics != nil {
		rtOpts = append(rtOpts, runtime.WithMetrics(b.metrics))
	}
	if b.tracer != nil {
		rtOpts = append(rtOpts, runtime.WithTracer(b.tracer))
	}
	if b.auditLogger != nil {
		rtOpts = append(rtOpts, runtime.WithAuditLogger(b.auditLogger))
	}

	endpoints := b.resolveClusterEndpoints(ctx)
	if len(endpoints) > 0 {
		rtOpts = append(rtOpts, runtime.WithClusterEndpoints(endpoints))
	}

	return &preparedBuild{
		cfg:    b.cfg,
		stores: stores,
		rtOpts: rtOpts,
	}, nil
}

// Build validates the configuration, creates all adapters via
// registered factories, and wires them into a runtime.Runtime in a
// single call. The returned runtime is not yet started; call Start
// on it separately. If any step fails, previously created sessions
// are closed to prevent resource leaks.
//
// Build is the recommended entry point for almost all callers.
// Supervisor / hot-reload code that must drain an old runtime
// between the prepare and commit phases should use Plan + Commit
// instead.
func (b *Builder) Build(ctx context.Context) (*runtime.Runtime, error) {
	prep, err := b.prepare(ctx)
	if err != nil {
		return nil, err
	}
	return b.complete(ctx, prep)
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

	// Clustered posture is implied by configured cluster endpoints even when
	// deployment_mode is unset (cluster finding 11): forwarding between
	// instances with a process-local lease/outbox/DLQ store silently breaks
	// exclusivity and durability, so the store-distribution guard keys on
	// either signal.
	clustered := b.cfg.Bridge.DeploymentMode == "clustered" ||
		(b.cfg.Bridge.Cluster != nil && len(b.cfg.Bridge.Cluster.Endpoints) > 0)
	if clustered {
		if res.lease != nil && !res.leaseDist {
			return nil, fmt.Errorf("bridge: clustered deployment (deployment_mode or cluster.endpoints set) requires a distributed LeaseStore; the configured store is process-local")
		}
		if res.outbox != nil && !res.outboxDist {
			return nil, fmt.Errorf("bridge: clustered deployment (deployment_mode or cluster.endpoints set) requires a distributed OutboxStore; the configured store is process-local")
		}
		if res.dlq != nil && !res.dlqDist {
			return nil, fmt.Errorf("bridge: clustered deployment (deployment_mode or cluster.endpoints set) requires a distributed DLQStore; the configured store is process-local")
		}
	}

	// Wrap stores with metrics decorators when an exporter is configured so
	// lease/outbox latency and failure metrics are actually emitted for
	// config-driven deployments (Finding 15). Wrapping happens AFTER the
	// distributed-store validation so leaseDist/outboxDist reflect the real
	// backing factory, not the decorator. A nil clock defaults to the system
	// clock inside the decorator. No DLQ decorator exists in the runtime, so
	// the DLQ store is left unwrapped.
	//
	// runtime.NewInstrumentedOutboxStoreCapabilityPreserving re-exports the
	// inner store's optional io.Closer/OutboxReleaser capabilities dynamically,
	// so its result is used directly — the bare NewInstrumentedOutboxStore would
	// mask OutboxReleaser and silently degrade the drainer's fast-release path
	// to stale reclaim. NewInstrumentedLeaseStore does NOT forward io.Closer,
	// yet runtime.Stop releases durable (file-backed) lease handles via an
	// io.Closer type assertion on the store it holds; wrapping a closable lease
	// store with the bare decorator would mask its Close and leak the OS handle
	// on every reconfiguration, so it is wrapped in instrumentedClosableLeaseStore
	// to keep Close visible to teardown.
	if b.metrics != nil {
		if res.lease != nil {
			res.lease = instrumentedClosableLeaseStore{
				InstrumentedLeaseStore: runtime.NewInstrumentedLeaseStore(res.lease, b.metrics, nil),
				inner:                  res.lease,
			}
		}
		if res.outbox != nil {
			res.outbox = runtime.NewInstrumentedOutboxStoreCapabilityPreserving(res.outbox, b.metrics, nil)
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
		return ports.OutboxRuntimeOptions{Metrics: b.metrics}, nil
	}

	if explicit, ok, err := explicitStaleClaimDuration(sc); err != nil {
		return ports.OutboxRuntimeOptions{}, err
	} else if ok {
		return ports.OutboxRuntimeOptions{StaleClaimDuration: explicit, Metrics: b.metrics}, nil
	}

	maxStepDownGrace := session.DefaultConfig("", true).StepDownGrace
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
		// Thread the builder's exporter (the same one handed to routes) so
		// the DynamoDB outbox store emits shared.MetricOutboxClaimConflicts in
		// production; nil when no exporter is configured (factory treats nil as
		// no-op).
		Metrics: b.metrics,
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
