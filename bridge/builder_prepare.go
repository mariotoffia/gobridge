package bridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
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
// BuildPlan.Commit (explicit two-phase). See.
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
//
// A BuildPlan that is prepared but never committed MUST be released via
// BuildPlan.Close (or its alias Abort) so the transport-independent stores the
// prepare phase opened (SQLite files, DynamoDB clients) are not leaked.
type BuildPlan struct {
	b    *Builder
	prep *preparedBuild

	mu sync.Mutex
	// consumed is set the instant Commit is invoked — BEFORE complete runs — so
	// the plan is one-shot regardless of outcome. complete()'s failure defers
	// close the prep-opened store handles, so a retried Commit would build a
	// runtime over already-closed handles; marking consumed up front
	// makes a second Commit fail instead.
	consumed bool
	// closed records that Close/Abort has released the prep-opened stores of a
	// never-committed plan, so Close is idempotent and never double-closes.
	closed bool
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
// supervisor-style hot-reload orchestration. A plan that is not
// committed must be released with Close/Abort.
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
// returns an error, whether the FIRST call succeeded OR failed. The
// plan is marked consumed BEFORE complete runs: complete()'s failure
// path closes the prep-opened store handles, so a retried Commit would
// otherwise build a runtime over already-closed stores. A
// caller that wants to retry a failed reload must Plan again.
func (p *BuildPlan) Commit(ctx context.Context) (*runtime.Runtime, error) {
	if p == nil {
		return nil, fmt.Errorf("bridge: BuildPlan.Commit called on nil plan")
	}
	p.mu.Lock()
	if p.consumed {
		p.mu.Unlock()
		return nil, fmt.Errorf("bridge: BuildPlan already committed")
	}
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("bridge: BuildPlan.Commit called after Close/Abort")
	}
	// Consume the plan up front so neither a success nor a failure leaves it
	// retryable (see the doc comment above).
	p.consumed = true
	p.mu.Unlock()

	rt, err := p.b.complete(ctx, p.prep)
	if err != nil {
		return nil, err
	}
	return rt, nil
}

// Close releases the transport-independent store handles a Plan opened but never
// committed into a runtime. It is the abort path for a caller that prepares a
// plan and then decides not to Commit it: without it the opened SQLite/DynamoDB
// store handles leak for the plan's lifetime. Close is idempotent and
// safe on a nil plan.
//
// Close is a deliberate NO-OP once Commit has been invoked: on Commit success
// the runtime owns the store handles and closes them on Stop; on Commit failure
// complete()'s own defers already closed them. Close therefore only acts on the
// never-committed path and can never double-close a handle.
func (p *BuildPlan) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.consumed || p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	prep := p.prep
	p.mu.Unlock()
	if prep != nil {
		p.b.closeStoreHandles(prep.stores)
	}
}

// Abort is an alias for Close, provided for callers that read the two-phase
// lifecycle as prepare/commit-or-abort. It releases a prepared-but-uncommitted
// plan's store handles exactly once.
func (p *BuildPlan) Abort() { p.Close() }

// prepare is the internal first phase used by both Build and Plan.
// It is unexported to enforce that callers cannot construct an
// invalid prepare/complete sequence — the public surface is Build
// (single-shot) or Plan/Commit (explicit two-phase).
func (b *Builder) prepare(ctx context.Context) (*preparedBuild, error) {
	// Surface deferred registration errors (e.g. a duplicate processor name)
	// before doing any work, so a name collision fails the Build loudly rather
	// than silently dropping a processor referenced by a route.
	if len(b.regErrs) > 0 {
		return nil, errors.Join(b.regErrs...)
	}

	// Build against a bridge-owned structural copy. Plugin configs advertising
	// ports.FreezableConfig provide their adapter-owned isolated snapshot, so
	// credential application and construction cannot mutate the Supervisor's
	// rollback/restart config. Unknown opaque plugin configs are never reflect-
	// copied or falsely treated as deep-frozen.
	var err error
	b.cfg, err = cloneConfigForBuild(b.cfg)
	if err != nil {
		return nil, fmt.Errorf("bridge: freeze config for build: %w", err)
	}

	if err := runtime.CheckRandSource(); err != nil {
		return nil, fmt.Errorf("bridge: entropy source unavailable: %w", err)
	}

	if b.validator != nil {
		if err := b.validator(b.cfg); err != nil {
			return nil, fmt.Errorf("bridge: config validation: %w", err)
		}
	}
	if err := b.validatePostAcquireActivationTimings(); err != nil {
		return nil, err
	}
	if err := b.validateFailoverBudgets(); err != nil {
		return nil, err
	}
	// Cardinality is a pure capability-based preflight. It must run before
	// buildStores or complete creates any store, session, receiver, sender, or
	// runtime resource: a rejected topology must leave the live system untouched.
	if err := b.validateDedicatedIngressSessions(); err != nil {
		return nil, err
	}
	if err := b.validateIngressMemory(); err != nil {
		return nil, err
	}

	stores, err := b.buildStores(ctx)
	if err != nil {
		return nil, err
	}

	rtOpts := []runtime.Option{
		runtime.WithLeaseStore(stores.lease),
		runtime.WithOutboxStore(stores.outbox),
		runtime.WithDLQStore(stores.dlq),
		runtime.WithManagedSubscriptionStore(stores.managedSubscriptions),
		// bridge.drain_timeout is the ceiling the supervisor puts on
		// Runtime.Stop (stopCurrent / stopAbandoned / every swap). Give the
		// runtime the SAME ceiling so the two agree: without it the runtime fell
		// back to an internal 5s budget, so whichever Stop won the SIGTERM race
		// clamped the close phase to 5s while the supervisor still held a 30s
		// drain open — the configured budget governed nothing, and the
		// store-close grace clamped to zero mid-drain.
		//
		// The pre-cancel settle phase keeps its own default ceiling
		// (WithStopQuiesce unset): it is already bounded by this same Stop
		// context, and pinning it to the full drain would leave nothing of the
		// budget for closing sessions, stores and telemetry.
		runtime.WithShutdownTimeout(b.cfg.Bridge.DrainTimeoutDuration()),
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

	endpoints, err := b.resolveClusterEndpoints(ctx)
	if err != nil {
		// buildStores already opened handles; a failure here (e.g. clustered
		// endpoint resolution) must release them rather than leak on every
		// failed reload (builder_prepare.go:229, Chunk 3).
		b.closeStoreHandles(stores)
		return nil, err
	}
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
	lease                    ports.LeaseStore
	outbox                   ports.OutboxStore
	dlq                      ports.DLQStore
	leaseDist                bool
	outboxDist               bool
	dlqDist                  bool
	leaseDurable             bool
	outboxDurable            bool
	dlqDurable               bool
	managedSubscriptions     ports.ManagedSubscriptionStore
	managedSubscriptionsDist bool
}

func isDistributedFactory(sf ports.StoreFactory) bool {
	if df, ok := sf.(ports.DistributedStoreFactory); ok {
		return df.IsDistributed()
	}
	return false
}

// hasExclusiveSessions reports whether the blueprint configures any exclusive
// (single-owner, lease-managed) session: either a session declared with
// session_mode: exclusive, or a route carrying an inline session block (which
// is always a lease-managed single-owner session). Such sessions rely on the
// LeaseStore to arbitrate ownership; a process-local lease store cannot do so
// across replicas.
func hasExclusiveSessions(cfg *ports.BridgeConfig) bool {
	if cfg == nil {
		return false
	}
	for i := range cfg.Sessions {
		if cfg.Sessions[i].SessionMode == string(connectivity.SessionExclusive) {
			return true
		}
	}
	for i := range cfg.Routes {
		if cfg.Routes[i].Session != nil {
			return true
		}
	}
	return false
}

func (b *Builder) buildStores(ctx context.Context) (_ *storeResult, retErr error) {
	res := &storeResult{}

	// Any failure AFTER a store was opened (a later store's factory error, the
	// nil-store guard, or the clustered-distribution rejection below) must not
	// leak the handles already created — a watcher-driven reload that keeps
	// failing would otherwise leak a SQLite handle / network client every cycle
	// (builder_prepare.go:229, Chunk 3). closeStoreHandles is best-effort and
	// skips in-memory stores (no io.Closer), mirroring runtime.Stop teardown.
	defer func() {
		if retErr != nil {
			b.closeStoreHandles(res)
		}
	}()

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
		res.leaseDurable = isCrashDurableFactory(sf)
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
		res.outboxDurable = isCrashDurableFactory(sf)
	}
	if sc := b.cfg.Stores.ManagedSubscriptions; sc != nil {
		sf, ok := b.storeFactories[sc.Type]
		if !ok {
			return nil, fmt.Errorf("bridge: no store factory registered for managed_subscriptions type %q", sc.Type)
		}
		mf, ok := sf.(ports.ManagedSubscriptionStoreFactory)
		if !ok {
			return nil, fmt.Errorf("bridge: store factory %q does not support managed subscriptions", sc.Type)
		}
		store, err := mf.NewManagedSubscriptionStore(ctx, sc.Config)
		if err != nil {
			return nil, fmt.Errorf("bridge: create managed subscription store: %w", err)
		}
		if store == nil {
			return nil, fmt.Errorf("bridge: store factory %q returned nil managed subscription store without error", sc.Type)
		}
		res.managedSubscriptions = store
		res.managedSubscriptionsDist = isDistributedFactory(sf)
	}
	if requiresManagedSubscriptionStore(b.cfg) && res.managedSubscriptions == nil {
		return nil, fmt.Errorf("bridge: persistent/exclusive MQTT sessions with desired subscriptions require stores.managed_subscriptions")
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
		res.dlqDurable = isCrashDurableFactory(sf)
	}

	// Clustered posture is implied by configured cluster endpoints even when
	// deployment_mode is unset (cluster finding 11): forwarding between
	// instances with a process-local lease/outbox/DLQ store silently breaks
	// exclusivity and durability, so the store-distribution guard keys on
	// either signal.
	// IsClusteredDeployment is the SHARED predicate (bridge/convert.go): the same
	// deployment_mode-or-static-endpoints definition used by the reload guard, so
	// the store-distribution guard and the fail-closed reload guard never disagree
	// on which deployments are clustered.
	if IsClusteredDeployment(b.cfg) {
		if res.lease != nil && !res.leaseDist {
			return nil, fmt.Errorf("bridge: clustered deployment (deployment_mode or cluster.endpoints set) requires a distributed LeaseStore; the configured store is process-local")
		}
		if res.outbox != nil && !res.outboxDist {
			return nil, fmt.Errorf("bridge: clustered deployment (deployment_mode or cluster.endpoints set) requires a distributed OutboxStore; the configured store is process-local")
		}
		if res.dlq != nil && !res.dlqDist {
			return nil, fmt.Errorf("bridge: clustered deployment (deployment_mode or cluster.endpoints set) requires a distributed DLQStore; the configured store is process-local")
		}
		if res.managedSubscriptions != nil && !res.managedSubscriptionsDist {
			return nil, fmt.Errorf("bridge: clustered deployment requires a distributed ManagedSubscriptionStore; the configured store is process-local")
		}
	}

	// Split-brain-by-misconfiguration guard (LOW): a process-local (e.g. memory)
	// lease store cannot arbitrate exclusive-session ownership ACROSS replicas.
	// Clustered mode already hard-fails above, but two replicas EACH deployed as
	// `standalone` with a memory lease will EACH believe they own every exclusive
	// session and drive it concurrently (split brain) — a posture NOT detectable
	// from any single process's config. deployment_mode cannot gate this warning:
	// it is a gobridge-config assertion decoupled from the orchestrator's actual
	// replica count (a pod set to `standalone` can still be scaled to replicas>1
	// in k8s), so `standalone` does NOT prove single-replica and suppressing on
	// it would blind the exact two-replica case this catches. So warn PROMINENTLY
	// whenever exclusive sessions ride on a non-distributed lease store,
	// regardless of deployment_mode, and spell out the safe remediation (run
	// exactly one replica, or adopt a distributed lease store). Follows the same
	// b.logger-nil-guarded warning idiom as the resolver-degradation path below.
	if res.lease != nil && !res.leaseDist && hasExclusiveSessions(b.cfg) && b.logger != nil {
		b.logger.Warn("SPLIT-BRAIN RISK: exclusive sessions are configured on a process-local (non-distributed) "+
			"lease store; if more than one replica runs, each replica's lease grants ownership of every exclusive "+
			"session independently and drives it concurrently. Run EXACTLY ONE replica (replicas=1) for this "+
			"configuration, or switch to a distributed lease store (e.g. dynamodb) for high availability.",
			"lease_store_type", b.cfg.Stores.Lease.Type,
			"deployment_mode", b.cfg.Bridge.DeploymentMode,
			"remediation", "set replicas=1, or use a distributed lease store")
	}

	if err := b.enforceStoreDurability(res); err != nil {
		return nil, err
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

// resolveClusterEndpoints returns the endpoints that identify this instance
// to its cluster peers. Explicitly configured cluster.endpoints win; otherwise
// a registered EndpointResolver is consulted.
//
// Posture on resolution failure (cluster): in a clustered
// deployment (deployment_mode: clustered) a failed resolution is a STARTUP
// ERROR — continuing with nil endpoints would silently disable cluster
// forwarding for the whole process lifetime, leaving peers unable to forward
// exclusive-route traffic to this instance. Outside clustered mode the
// failure is logged as a warning and forwarding is simply unavailable
// (single-instance deployments do not need it).
func (b *Builder) resolveClusterEndpoints(ctx context.Context) (map[string]string, error) {
	if b.cfg.Bridge.Cluster != nil && len(b.cfg.Bridge.Cluster.Endpoints) > 0 {
		return b.cfg.Bridge.Cluster.Endpoints, nil
	}

	if b.endpointResolver != nil {
		listenAddr := ":8080"
		if b.cfg.HTTP != nil && b.cfg.HTTP.AdminAddr != "" {
			listenAddr = b.cfg.HTTP.AdminAddr
		}
		endpoints, err := b.endpointResolver.Resolve(ctx, listenAddr)
		if err != nil {
			if b.cfg.Bridge.DeploymentMode == "clustered" {
				return nil, fmt.Errorf("bridge: clustered deployment: endpoint resolution failed; "+
					"refusing to start with cluster forwarding silently disabled for the process lifetime "+
					"(peers could not reach this instance): %w", err)
			}
			if b.logger != nil {
				b.logger.Warn("endpoint resolution failed, cluster routing may be unavailable", "error", err)
			}
			return nil, nil
		}
		return endpoints, nil
	}

	return nil, nil
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
		sessCfg, err := toSessionConfigE(r.Session, IsClusteredDeployment(b.cfg))
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

// validateDedicatedIngressSessions enforces transport-declared session
// cardinality without naming a transport or importing an adapter. The
// capability belongs to the factory that creates the session, so every logical
// receiver bound to that session counts even when receiver factories are
// registered under different aliases. Sender definitions deliberately do not
// participate: egress may continue sharing the connection.
func (b *Builder) validateDedicatedIngressSessions() error {
	if b.cfg == nil {
		return nil
	}

	receiversBySession := make(map[string][]string, len(b.cfg.Sessions))
	for i := range b.cfg.Receivers {
		receiver := &b.cfg.Receivers[i]
		if receiver.SessionID != "" {
			receiversBySession[receiver.SessionID] = append(receiversBySession[receiver.SessionID], receiver.ID)
		}
	}
	routesByReceiver := make(map[string][]string, len(b.cfg.Receivers))
	for i := range b.cfg.Routes {
		route := &b.cfg.Routes[i]
		routesByReceiver[route.ReceiverID] = append(routesByReceiver[route.ReceiverID], route.ID)
	}

	for i := range b.cfg.Sessions {
		sessionDef := &b.cfg.Sessions[i]
		factory, ok := b.transports[sessionDef.Transport]
		if !ok || !hasTransportCapability(factory, ports.CapDedicatedIngressSession) {
			continue
		}
		receiverIDs := receiversBySession[sessionDef.ID]
		if len(receiverIDs) > 1 {
			return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
				"bridge: session %q (transport %q) requires dedicated ingress but has %d logical receivers %q; configure one session per ingress receiver (senders may still share a session)",
				sessionDef.ID, sessionDef.Transport, len(receiverIDs), receiverIDs,
			))
		}
		if len(receiverIDs) == 0 {
			// Sender-only sessions do not create an ingress failure domain.
			continue
		}
		receiverID := receiverIDs[0]
		routeIDs := routesByReceiver[receiverID]
		if len(routeIDs) > 1 {
			return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
				"bridge: session %q (transport %q) requires dedicated ingress but receiver/source %q is consumed by conflicting route runners %q; configure a distinct session and receiver for each ingress route",
				sessionDef.ID, sessionDef.Transport, receiverID, routeIDs,
			))
		}
	}
	return nil
}

func hasTransportCapability(factory ports.TransportFactory, want ports.Capability) bool {
	if factory == nil {
		return false
	}
	for _, capability := range factory.Capabilities() {
		if capability == want {
			return true
		}
	}
	return false
}

// validateIngressMemory invokes the optional config capability once for every
// started session that can own inbound state. A ReceiverDef creates possible
// ingress even when no route currently consumes it. Referenced Persistent and
// Exclusive sessions are also included because resumed stale broker backlog can
// arrive before durable subscription cleanup. Ephemeral sender-only and wholly
// unreferenced sessions are excluded.
func (b *Builder) validateIngressMemory() error {
	if b.cfg == nil {
		return nil
	}

	receiverSession := make(map[string]string, len(b.cfg.Receivers))
	includedSessions := make(map[string]struct{}, len(b.cfg.Sessions))
	for i := range b.cfg.Receivers {
		receiver := &b.cfg.Receivers[i]
		if receiver.SessionID != "" {
			receiverSession[receiver.ID] = receiver.SessionID
			includedSessions[receiver.SessionID] = struct{}{}
		}
	}
	routeConcurrency := make(map[string]uint64, len(b.cfg.Sessions))
	for i := range b.cfg.Routes {
		route := &b.cfg.Routes[i]
		sessionID := receiverSession[route.ReceiverID]
		if sessionID == "" {
			continue
		}
		if route.Policy.MaxInFlight < 0 {
			return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
				"bridge: route %q: policy.max_in_flight must not be negative", route.ID,
			))
		}
		maxInFlight := route.Policy.MaxInFlight
		if maxInFlight == 0 {
			maxInFlight = routing.DefaultMaxInFlight
		}
		current := routeConcurrency[sessionID]
		add := uint64(maxInFlight)
		if add > math.MaxUint64-current {
			return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
				"bridge: session %q route concurrency overflows ingress memory calculation", sessionID,
			))
		}
		routeConcurrency[sessionID] = current + add
	}

	referenced := referencedSessionIDs(b.cfg)
	for i := range b.cfg.Sessions {
		sessionDef := &b.cfg.Sessions[i]
		mode := normalizedSessionMode(sessionDef.SessionMode)
		if !referenced[sessionDef.ID] ||
			(mode != connectivity.SessionPersistent && mode != connectivity.SessionExclusive) {
			continue
		}
		includedSessions[sessionDef.ID] = struct{}{}
	}

	for i := range b.cfg.Sessions {
		sessionDef := &b.cfg.Sessions[i]
		if _, included := includedSessions[sessionDef.ID]; !included {
			continue
		}
		maxInFlight := routeConcurrency[sessionDef.ID]
		memoryConfig, ok := sessionDef.Config.(ports.IngressMemoryConfig)
		if !ok {
			continue
		}
		if err := memoryConfig.ValidateIngressMemory(maxInFlight); err != nil {
			return shared.ErrInvalidConfig.Wrap(err).WithMessage(fmt.Sprintf(
				"bridge: session %q ingress memory validation failed", sessionDef.ID,
			))
		}
	}
	return nil
}
