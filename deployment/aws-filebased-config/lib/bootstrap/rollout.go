package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbrollout"
	"github.com/mariotoffia/gobridge/bridge"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
)

// Coordinated cluster rollout — the shipped file-based root's barrier host
// (design cluster-config-rollout-protocol.md Phase 6 "ship step").
//
// The barrier itself lives in package bridge and is runtime-agnostic: it drives
// through a ports.RolloutHost (bridge/rollout_driver.go). This file is the App's
// implementation of that host plus the composition that builds the driver, so a
// clustered file-based deployment can do coordinated live-safe config changes
// instead of the ADR 0012 whole-cohort refusal. Everything hard (the store
// protocol, the coordinator election, the joiner boot resolution, the applier
// retry/degrade) is the barrier's; this file only supplies the App's own runtime
// as the host and wires the deploy-time dependencies.

// errRolloutDeferred is returned from applyLogicalConfig when a live-safe delta is
// PROPOSED to the coordinated rollout barrier rather than applied inline. It is a
// deferral, not a failure: the running config keeps serving and the barrier's
// applier performs the local swap once the cohort commits.
//
// It wraps ports.ErrApplyInFlight ("committed, will become running — do NOT roll
// back") so the admin config-transaction layer (httpapi), which errors.Is against
// that sentinel across the module boundary, surfaces committed_not_applied instead
// of rolling the durable write BACK. Rolling back would fight the barrier: the
// cohort still commits the deferred delta, so a rollback would leave the durable
// config and the running runtime permanently split (adversarial-review Finding 1).
var errRolloutDeferred = fmt.Errorf("bootstrap: config reload deferred to the coordinated cluster "+
	"rollout barrier: %w", ports.ErrApplyInFlight)

// WithClusterRolloutStores injects the coordination stores the rollout barrier
// runs on, instead of the DynamoDB stores Start builds from the task's DynamoDB
// client. It is the seam custom composition roots and tests use to run the barrier
// on a non-DynamoDB store (e.g. an in-memory store for a component test). Wiring
// only ONE of the pair is a misconfiguration the driver rejects (it fails closed
// to the ADR 0012 refusal).
func WithClusterRolloutStores(store ports.ClusterRolloutStore, lease ports.LeaseStore) Option {
	return func(a *App) {
		a.rolloutConfig.Store = store
		a.rolloutConfig.Lease = lease
	}
}

// appRolloutHost adapts this App to ports.RolloutHost: the five operations the
// barrier drive needs from its runtime. It captures the App, so every call reads
// live App state (the applied config, the logger) at call time.
type appRolloutHost struct{ a *App }

var _ ports.RolloutHost = appRolloutHost{}

// newAppRolloutHost returns the App as a ports.RolloutHost. It returns the PORT
// interface (not the concrete type) so the composition root injects an abstraction
// into bridge — the same shape adapter factories use (NewDynamoDBStoreFactory ->
// ports.StoreFactory) — keeping bridge free of any structural dependency on the
// deployment layer.
func newAppRolloutHost(a *App) ports.RolloutHost { //nolint:ireturn // intentional: returns the ports.RolloutHost driving-port so the composition root injects an ABSTRACTION into bridge (keeping bridge free of any structural dependency on the deployment layer — the arch-lint deep scan), not the concrete host type.
	return appRolloutHost{a}
}

// Config is the running config — appliedRef holds the last successfully-applied
// logical config (installPlan sets it to plan.logical), so its content is exactly
// what the barrier compares the committed candidate against after a swap.
func (h appRolloutHost) Config() *ports.BridgeConfig { return h.a.appliedRef.Get() }

// PlanCandidate is the Ack build-proof: build the candidate's stores and runtime
// options WITHOUT opening transport sessions (bridge.Builder.Plan), then release
// it. It mirrors prepareRuntimePlan's prepare phase but always takes the cheap
// Plan path (never Build), because a vote only needs to prove the candidate builds
// on this member — the real swap runs the full apply path at commit time.
func (h appRolloutHost) PlanCandidate(ctx context.Context, cfg *ports.BridgeConfig) (func(), error) {
	inputs, err := resolveInputs(ctx, h.a.parameterResolver, h.a.cfg, h.a.pluginRegistry, cfg)
	if err != nil {
		return nil, err
	}
	if err := applyMQTTMemoryProfile(inputs.RuntimeConfig, h.a.cfg); err != nil {
		return nil, err
	}
	plan, err := h.a.newFactoryRegistry(inputs.RuntimeConfig).builder.Plan(ctx)
	if err != nil {
		return nil, err
	}
	return plan.Close, nil
}

// ApplyCommitted swaps the live runtime to a committed config through the App's own
// apply machinery (with barrierCommitted=true so the reload seam is skipped) and
// re-syncs the config manager. It is best-effort by contract: the barrier verifies
// the swap took by re-reading Config() and retries or marks this member degraded.
func (h appRolloutHost) ApplyCommitted(ctx context.Context, cfg *ports.BridgeConfig) {
	h.a.applyBarrierCommitted(ctx, cfg)
}

// MarkDegraded latches an applied-but-diverged state: a generation the cohort
// committed could not be applied here after the barrier's bounded retries, so this
// member runs an older generation than its peers. It surfaces in deep health and
// MetricConfigDegraded via the existing convergence-degraded latch.
func (h appRolloutHost) MarkDegraded(reason string) {
	h.a.markConvergenceDegraded(h.a.runtimeRef.Get(), reason)
}

func (h appRolloutHost) RolloutLogger() *slog.Logger { return h.a.logger }

// Converged reports whether this App's active runtime reached the post-swap
// readiness level (MQTT-R1 — the same signal the convergence watch uses). It is the
// confirm-window signal (design §8.1): the drive records this member's Converge once
// it is true. False when no runtime is active.
func (h appRolloutHost) Converged(ctx context.Context) bool {
	rt := h.a.runtimeRef.Get()
	if rt == nil {
		return false
	}
	return rt.ReadinessLevel(ctx) >= ports.LevelSubscribed
}

// rolloutCodec is the config <-> bytes round-trip the barrier persists the durable
// last-committed artifact through. Encode is the round-trippable JSON wire form;
// Decode parses it back through THIS App's plugin registry, so a (re)joining member
// reconstructs the committed config with a digest the joiner/reconcile paths accept.
// bridge cannot import config/parser (arch-lint), which is why the composition root
// injects these.
func (a *App) rolloutCodec() (func(*ports.BridgeConfig) ([]byte, error), func([]byte) (*ports.BridgeConfig, error)) {
	encode := func(cfg *ports.BridgeConfig) ([]byte, error) {
		return cfgparser.MarshalBridgeConfigJSON(cfg)
	}
	decode := func(b []byte) (*ports.BridgeConfig, error) {
		return cfgparser.Parse(bytes.NewReader(b), cfgparser.FormatJSON, a.pluginRegistry)
	}
	return encode, decode
}

// rolloutTableName is the DynamoDB table backing the coordination store, from the
// deployment config or the adapter default.
func (a *App) rolloutTableName() string {
	if a.cfg.DynamoDBHARolloutTableName != "" {
		return a.cfg.DynamoDBHARolloutTableName
	}
	return dynamodbrollout.DefaultTableName
}

// buildRolloutDriver constructs this App's cluster rollout barrier host. It fills
// the composition-root-owned dependencies onto rolloutConfig — the member id, the
// config codec, and (unless already injected via WithClusterRolloutStores) the
// DynamoDB coordination store and lease store built from the task's DynamoDB client
// — then builds the driver. The driver is nil when the barrier is half-wired
// (no member id or store), which fails closed to the ADR 0012 refusal; Start
// treats that as a fatal misconfiguration for a coordinated boot config.
//
// EnsureTable is best-effort: it creates the coordination table for a self-managed
// or local deployment and no-ops when the table already exists; when the task role
// lacks CreateTable but the table is pre-provisioned (least-privilege production),
// the create call fails and is logged, and the boot-resolve read against the
// existing table is the authoritative gate that a truly-missing store fails on.
func (a *App) buildRolloutDriver(ctx context.Context) error {
	rc := a.rolloutConfig
	rc.MemberID = a.cfg.MemberID
	rc.Encode, rc.Decode = a.rolloutCodec()

	if rc.Store == nil {
		store := dynamodbrollout.NewStore(a.dynamoDBClient,
			dynamodbrollout.WithTableName(a.rolloutTableName()),
			dynamodbrollout.WithLogger(a.logger),
		)
		if err := store.EnsureTable(ctx); err != nil {
			a.logger.Warn("bootstrap: could not ensure the cluster rollout coordination table exists; "+
				"if it is pre-provisioned this is expected, otherwise the boot-resolve read will fail closed",
				"table", a.rolloutTableName(), "error", err)
		}
		rc.Store = store
	}
	if rc.Lease == nil {
		rc.Lease = dynamodblease.NewStore(a.dynamoDBClient,
			dynamodblease.WithTableName(a.cfg.DynamoDBHALeaseTableName),
		)
	}

	a.rolloutConfig = rc
	a.rolloutDriver = bridge.NewClusterRolloutDriver(newAppRolloutHost(a), rc)
	return nil
}

// reconcileBootApplyResult tells the config manager the outcome of the boot apply.
// Normally bootCfg IS the emitted logical config, so NotifyApplyResult correlates
// by pointer identity as it always has. When the coordinated boot-resolve
// SUBSTITUTED the durable committed config (bootCfg != logicalCfg), the applied
// config is one the manager did not emit, so AdoptRunning re-syncs running to it
// instead — ReconfigurePending then correctly reflects that the config source still
// holds a config the cohort has not committed, which the operator must reconcile.
func (a *App) reconcileBootApplyResult(logicalCfg, bootCfg *ports.BridgeConfig) {
	if bootCfg == logicalCfg {
		a.manager.NotifyApplyResult(logicalCfg, nil)
		return
	}
	a.manager.AdoptRunning(bootCfg)
}

// clusterReloadSeam is the shipped-root counterpart to the Supervisor's apply-path
// cluster guard. It reports handled=true when it resolved a clustered live reload
// itself — refused it (ADR 0012), or proposed a live-safe delta to the barrier and
// deferred it — in which case applyLogicalConfig must return without swapping.
// handled=false means proceed with the normal single-node apply (neither side is
// clustered, or this is the initial boot apply).
func (a *App) clusterReloadSeam(ctx context.Context, logical *ports.BridgeConfig) (bool, error) {
	oldApplied := a.appliedRef.Get()
	if oldApplied == nil {
		// Initial apply: a fresh boot into a clustered config is legitimate (the
		// joiner boot-resolve already ran in Start), not a live reload.
		return false, nil
	}
	if !bridge.IsClusteredDeployment(oldApplied) && !bridge.IsClusteredDeployment(logical) {
		return false, nil
	}
	switch disp, reason := bridge.ClassifyClusterReload(oldApplied, logical); disp {
	case bridge.ClusterReloadCoordinated:
		if a.rolloutDriver == nil {
			return true, a.refuseClusteredReload(logical, "cluster.rollout: coordinated is configured but "+
				"this process has no rollout barrier wired")
		}
		// Propose to the barrier and DEFER. This node must not swap here under any
		// outcome — an uncoordinated swap splits the cohort. The applier performs the
		// local swap once the barrier commits.
		//
		// `logical` is passed as BOTH the frozen candidate and the manager source
		// pointer (unlike the Supervisor, which freezes an independent clone). That is
		// safe here because a manager-emitted config is immutable by contract and the
		// App never mutates it in place — resolveInputs clones before injecting
		// secrets — so the frozen snapshot the applier builds/digests against cannot
		// diverge from the source. (adversarial-review Finding 3: latent, safe today.)
		if err := a.rolloutDriver.Propose(ctx, oldApplied, logical, logical); err != nil {
			return true, fmt.Errorf("bootstrap: refusing the live-safe delta fail-closed; proposing it to "+
				"the coordinated cluster rollout barrier failed, so the running config keeps serving: %w", err)
		}
		a.logger.Info("bootstrap: live-safe delta proposed to the coordinated cluster rollout barrier; "+
			"this node applies it when the barrier commits", "attempted_config_version", logical.Version)
		return true, errRolloutDeferred
	case bridge.ClusterReloadRefuse:
		return true, a.refuseClusteredReload(logical, reason)
	default:
		return false, nil
	}
}

// refuseClusteredReload is the ADR 0012 fail-closed refusal. reason is non-empty
// only for a coordinated cohort whose delta is replacement-required (it then names
// the class on top of the generic refusal, pointing at the whole-cohort procedure).
func (a *App) refuseClusteredReload(logical *ports.BridgeConfig, reason string) error {
	err := fmt.Errorf("bootstrap: refusing live reload of a clustered deployment: a per-process reload has "+
		"no cluster-wide version barrier or coordinated rollback, so a rolling reload would split the cohort "+
		"across config versions. Externally coordinate a whole-cohort replacement (stage, validate every "+
		"member, quiesce ingress, drain/stop all members, commit, start all members, verify the "+
		"version/readiness barrier, then re-enable ingress); see docs/runbooks/cluster-config-rollout.md "+
		"(attempted_config_version=%d)", logical.Version)
	if reason != "" {
		err = fmt.Errorf("%w; this delta is replacement-required and keeps the whole-cohort replacement "+
			"procedure: %s", err, reason)
	}
	return err
}

// applyBarrierCommitted performs the local swap for a committed rollout generation,
// driven by the barrier's applier. It swaps through the App's own apply path with
// the reload seam skipped (barrierCommitted=true), records the fingerprint so the
// file watcher re-emitting the same config does not swap again, and re-syncs the
// config manager.
//
// The manager re-sync (Phase-6 composition obligation) is the load-bearing part:
// the barrier applies a config the manager never EMITTED (the applier's candidate,
// or the decoded committed artifact), so NotifyApplyResult would drop it as a
// foreign pointer and leave ReconfigurePending — and deep-health Degraded — latched
// true despite correct convergence. AdoptRunning re-syncs running to the applied
// config instead.
func (a *App) applyBarrierCommitted(ctx context.Context, cfg *ports.BridgeConfig) {
	a.mu.Lock()
	fp := a.parsedFingerprint(cfg, true)
	alreadyRunning := fp != "" && fp == a.lastAppliedFingerprint
	var err error
	if !alreadyRunning {
		if err = a.applyLogicalConfig(ctx, cfg, true); err == nil && fp != "" {
			a.lastAppliedFingerprint = fp
		}
	}
	a.mu.Unlock()

	if err != nil {
		a.logger.Error("bootstrap: applying the committed cluster rollout generation failed; the barrier "+
			"retries and marks this member degraded if it cannot converge", "error", err)
		return
	}
	// Re-sync the manager to the config the barrier applied (see doc above).
	a.manager.AdoptRunning(cfg)
}
