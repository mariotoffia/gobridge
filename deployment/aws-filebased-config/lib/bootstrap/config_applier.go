package bootstrap

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// Applying a logical config to the live runtime: the swap itself.
//
// Two swap modes exist because some transports own an EXCLUSIVE broker identity
// that two runtimes cannot hold at once (prepare/commit: stop the old runtime
// before starting the new one), while everything else can overlap (build and
// start the new runtime, then stop the old). The overlap mode keeps serving
// across the swap; the prepare/commit mode accepts a gap to keep the identity
// single-owner, and carries the recovery path for a commit that failed.

type swapMode int

const (
	swapModeOverlap swapMode = iota
	swapModePrepareCommit
)

type runtimePlan struct {
	logical  *ports.BridgeConfig
	resolved *ports.BridgeConfig
	inputs   *resolvedInputs
	mode     swapMode

	registry *factoryRegistry
	plan     *bridge.BuildPlan
	runtime  *goruntime.Runtime
}

func (a *App) applyLogicalConfig(ctx context.Context, logical *ports.BridgeConfig, barrierCommitted bool) error {
	// The cluster reload seam (design cluster-config-rollout-protocol.md §6). A
	// per-process live reload of a clustered deployment has no cluster-wide version
	// barrier or coordinated rollback, so by default it is refused (ADR 0012): a
	// rolling reload would split the cohort across config versions. When the
	// deployment opts into cluster.rollout: coordinated AND this App has the barrier
	// wired, a live-safe delta is instead PROPOSED to the barrier and DEFERRED here
	// (clusterReloadSeam) — the local swap happens later, driven by the barrier's
	// applier once the cohort commits, which re-enters with barrierCommitted=true to
	// SKIP the seam and perform the actual swap.
	//
	// The initial apply (oldApplied == nil) and a genuine no-op re-emit never reach
	// the seam: the seam requires an applied config, and applyLogicalIfChanged
	// short-circuits a no-op on the content fingerprint before calling this.

	// Deployment admission runs BEFORE the cluster reload seam. Proposing first meant
	// a config this deployment forbids could be acknowledged and committed by the
	// whole cohort, and only then refused by every member's apply — a committed
	// generation nobody runs, which is not a state the barrier can resolve.
	if err := a.admitDeploymentProfile(ctx, logical, "apply"); err != nil {
		return err
	}

	if !barrierCommitted {
		if handled, err := a.clusterReloadSeam(ctx, logical); handled {
			return err
		}
	}

	plan, err := a.prepareRuntimePlan(ctx, logical)
	if err != nil {
		return err
	}

	oldRuntime := a.runtimeRef.Get()
	oldApplied := a.appliedRef.Get()
	oldRegistry := a.registryRef.Load()

	switch plan.mode {
	case swapModePrepareCommit:
		return a.applyPrepareCommit(ctx, plan, oldRuntime, oldApplied, oldRegistry)
	default:
		return a.applyOverlap(ctx, plan, oldRuntime, oldApplied, oldRegistry)
	}
}

func (a *App) prepareRuntimePlan(ctx context.Context, logical *ports.BridgeConfig) (*runtimePlan, error) {
	inputs, err := resolveInputs(ctx, a.parameterResolver, a.cfg, a.pluginRegistry, logical)
	if err != nil {
		return nil, err
	}
	if err := applyMQTTMemoryProfile(inputs.RuntimeConfig, a.cfg); err != nil {
		return nil, err
	}

	registry := a.newFactoryRegistry(inputs.RuntimeConfig)
	mode := registry.detectSwapMode(inputs.RuntimeConfig)

	plan := &runtimePlan{
		logical:  logical,
		resolved: inputs.RuntimeConfig,
		inputs:   inputs,
		mode:     mode,
		registry: registry,
	}

	switch mode {
	case swapModePrepareCommit:
		bp, err := registry.builder.Plan(ctx)
		if err != nil {
			return nil, err
		}
		plan.plan = bp
	default:
		rt, err := registry.builder.Build(ctx)
		if err != nil {
			return nil, err
		}
		plan.runtime = rt
	}

	return plan, nil
}

func (a *App) applyOverlap(
	ctx context.Context,
	plan *runtimePlan,
	oldRuntime *goruntime.Runtime,
	oldApplied *ports.BridgeConfig,
	oldRegistry *factoryRegistry,
) error {
	if err := plan.runtime.Start(a.runtimeStartCtx(ctx)); err != nil {
		// RECONFIG-2: a candidate whose late Start fails still opened stores,
		// sessions, and adapter resources during Build. Stop it before returning so
		// repeated failing applies do not leak store handles and background state.
		// Bounded teardown (context.Background) like every other reload-path stop.
		_ = stopRuntime(context.Background(), plan.runtime, plan.logical)
		return fmt.Errorf("bootstrap: start runtime: %w", err)
	}

	// If anything below panics, ensure the started runtime is cleaned up.
	// Reload-path drain: NOT bounded by process shutdown (context.Background).
	installed := false
	defer func() {
		if !installed {
			_ = stopRuntime(context.Background(), plan.runtime, plan.logical)
		}
	}()

	a.installPlan(plan)
	installed = true

	if oldRuntime != nil {
		if err := stopRuntime(context.Background(), oldRuntime, oldApplied); err != nil {
			a.logger.Warn("bootstrap: stop old runtime after overlap swap", "error", err)
		}
	}
	// Drain the superseded registry's SSE senders ONLY on this success tail:
	// installPlan has swapped handlerRef to the new mux, so clients still
	// pinned to the OLD sender must be released to reconnect to the new one
	// (otherwise they sit on the orphaned sender receiving heartbeats but no
	// events). This is deliberately AFTER installPlan and NOT on the early
	// "start runtime" error return above — that path leaves the old
	// runtime/handler live, so draining there would disconnect healthy clients
	// from a still-serving sender.
	a.closeSupersededHTTP(ctx, oldRegistry)
	return nil
}

func (a *App) applyPrepareCommit(
	ctx context.Context,
	plan *runtimePlan,
	oldRuntime *goruntime.Runtime,
	oldApplied *ports.BridgeConfig,
	oldRegistry *factoryRegistry,
) error {
	// Every return path here supersedes the old registry: success installs the
	// new mux via installPlan, and each failure path routes through
	// recoverPrevious — which installs a freshly-rebuilt registry or wedges to
	// http.NotFoundHandler — never the old one. So drain the old registry's SSE
	// senders on the way out regardless of outcome. Unlike applyOverlap there
	// is no "old still live" early return to protect: the old runtime is
	// stopped up front (below), so its handler is already being torn down.
	defer a.closeSupersededHTTP(ctx, oldRegistry)

	if oldRuntime != nil {
		if err := stopRuntime(context.Background(), oldRuntime, oldApplied); err != nil {
			// prepare/commit stops the old runtime BEFORE committing the new one,
			// so a failed stop leaves ownership uncertain (its exclusive broker
			// session / lease may still be held). Committing a replacement onto
			// the same identity would risk two owners, so abort.
			//
			// The old runtime cannot be kept installed either: Runtime.Stop has no
			// early error return, so it has already cancelled its work context and
			// closed managers, sessions and stores, and a stopped runtime is
			// single-use. Leaving it in runtimeRef bridged nothing behind a green
			// /live — and because appliedRef and the fingerprint still named its
			// config, the admin transaction's disk rollback re-emitted that same
			// config and applyLogicalIfChanged SKIPPED the rebuild, so nothing
			// ever recovered. Release the never-committed plan's store handles,
			// clear the fingerprint so a re-emit does rebuild, and wedge
			// (ADR-0004) so the backstop restarts the task.
			plan.plan.Close()
			a.lastAppliedFingerprint = ""
			a.enterWedgedState()
			return fmt.Errorf("bootstrap: abort prepare/commit swap; old runtime did not stop cleanly "+
				"(uncertain ownership, refusing to commit a replacement): %w", err)
		}
	}
	a.runtimeRef.Set(nil)

	newRuntime, err := plan.plan.Commit(ctx)
	if err != nil {
		// RECONFIG-2: a partially-built candidate from a failed Commit still holds
		// resources; stop it (nil-safe) before rebuilding the previous runtime.
		if newRuntime != nil {
			_ = stopRuntime(context.Background(), newRuntime, plan.logical)
		}
		a.recoverPrevious(ctx, oldApplied)
		return fmt.Errorf("bootstrap: complete runtime: %w", err)
	}
	if err := newRuntime.Start(a.runtimeStartCtx(ctx)); err != nil {
		// RECONFIG-2: stop the committed-but-unstarted candidate before recovering,
		// so its opened stores/sessions are released rather than leaked.
		_ = stopRuntime(context.Background(), newRuntime, plan.logical)
		a.recoverPrevious(ctx, oldApplied)
		return fmt.Errorf("bootstrap: start runtime: %w", err)
	}
	plan.runtime = newRuntime
	a.installPlan(plan)
	return nil
}

func (a *App) recoverPrevious(ctx context.Context, logical *ports.BridgeConfig) {
	if logical == nil {
		a.enterWedgedState()
		return
	}

	plan, err := a.prepareRuntimePlan(ctx, logical)
	if err != nil {
		a.logger.Error("bootstrap: failed to rebuild previous runtime after prepare/commit failure", "error", err)
		a.enterWedgedState()
		return
	}

	switch plan.mode {
	case swapModePrepareCommit:
		plan.runtime, err = plan.plan.Commit(ctx)
	default:
		// Overlap mode: plan.runtime was already built by prepareRuntimePlan.
	}
	if err == nil {
		err = plan.runtime.Start(a.runtimeStartCtx(ctx))
	}
	if err != nil {
		// RECONFIG-2: the recovery candidate itself failed to commit/start. Stop it
		// (nil-safe) so it does not leak resources on top of the failed swap before
		// entering the wedged state that the orchestrator restarts out of.
		if plan.runtime != nil {
			_ = stopRuntime(context.Background(), plan.runtime, logical)
		}
		a.logger.Error("bootstrap: failed to restart previous runtime after prepare/commit failure", "error", err)
		a.enterWedgedState()
		return
	}

	a.installPlan(plan)
}

func (a *App) installPlan(plan *runtimePlan) {
	// bridge.log_level is committed WITH the runtime, never ahead of it. Applied
	// at the top of the reload path it changed live process verbosity even for a
	// candidate that deployment-profile validation or the build then rejected,
	// leaving the running system matching no config an operator could read back.
	// The cost is that a reload raising the level to debug does not log its own
	// build at debug; the state being truthful is worth more.
	a.applyLogLevel(plan.logical)
	// A successful apply/recovery clears any prior wedged latch: the App now
	// has an active runtime again and can self-heal.
	a.wedged.Store(false)
	a.runtimeRef.Set(plan.runtime)
	a.appliedRef.Set(plan.logical)
	a.apiKeysRef.Set(plan.inputs.AdminAPIKey, plan.inputs.MonitorAPIKey)
	a.handlerRef.Set(plan.registry.transportHandler())
	// Retain the installed registry so its HTTP transport's SSE senders can be
	// drained when this registry is later superseded or on shutdown (see
	// closeSupersededHTTP and Stop). Stored last, after handlerRef already
	// points at this registry's mux.
	a.registryRef.Store(plan.registry)
	// RECONFIG-1: begin (supersede) the post-swap convergence watch for the
	// freshly installed runtime. Skipped during the pre-rootCtx initial apply
	// (Start begins the initial watch explicitly once rootCtx exists).
	if a.rootCtx != nil {
		a.startConvergenceWatch(a.rootCtx, plan.runtime, plan.logical)
	}
	if a.onRuntimeInstalled != nil {
		a.onRuntimeInstalled()
	}
}

// closeSupersededHTTP drains the SSE senders of a superseded (or shutting-down)
// factory registry's HTTP transport so clients pinned to its mux disconnect and
// reconnect to the newly installed one, and so a fronting server.Shutdown does
// not hang on long-lived SSE handlers. nil-safe; idempotent (SSESender.Close
// uses sync.Once).
func (a *App) closeSupersededHTTP(ctx context.Context, reg *factoryRegistry) {
	if reg == nil || reg.http == nil {
		return
	}
	if err := reg.http.Close(ctx); err != nil {
		a.logger.Warn("bootstrap: drain superseded HTTP SSE senders", "error", err)
	}
}

// runtimeStartCtx returns the context a newly installed runtime must live under:
// the process-scoped watch context, never the caller's apply context.
//
// An admin config commit applies IN-BAND on a context the httpapi transaction
// detaches from the request but still bounds with its apply deadline and
// cancels the moment Commit returns. A runtime started on it therefore had its
// lifetime tied to the apply: the start-context watcher observed the cancel
// seconds later and stopped the freshly installed runtime, leaving the process
// installed-but-stopped — /live still 200, because a clean stop is not terminal
// — until some unrelated config arrived. Runtimes outlive the apply that built
// them; only shutdown ends them.
//
// Before Start has published rootCtx (the initial boot apply) the caller's
// context IS the process context, so it is the right one.
func (a *App) runtimeStartCtx(ctx context.Context) context.Context {
	if a.rootCtx != nil {
		return a.rootCtx
	}
	return ctx
}
