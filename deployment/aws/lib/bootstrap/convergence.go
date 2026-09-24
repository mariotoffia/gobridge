package bootstrap

import (
	"context"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// Post-swap convergence watch for the shipped AWS bootstrap.
//
// A committed swap proves only that the new runtime BUILT and its Start returned;
// MQTT dials and reconciles in background goroutines, so a syntactically-valid
// but broker-rejected config (denied credentials, an ACL-rejected topic) is
// acknowledged as applied while the transport never reaches broker truth. This
// watch mirrors the generic bridge.Supervisor's convergence watch: after each
// install it observes the new runtime's readiness and, if it does not reach
// LevelSubscribed within the transport's declared activation budget, latches an
// applied-but-not-converged degraded state (deep health reason + the same
// MetricConfigDegraded signal the Supervisor emits). It never reverts —
// per-session supervision keeps retrying and an operator revert is the
// remediation — it makes the false success OBSERVABLE.

const (
	bootstrapConvergencePollInterval = 2 * time.Second
	// bootstrapConvergenceBudgetFloor covers one default connect (30s) plus one
	// default reconcile (30s) for transports that declare no activation timing.
	bootstrapConvergenceBudgetFloor = 60 * time.Second
	// bootstrapConvergenceReadyLevel is the readiness level that counts as
	// converged. LevelSubscribed (not LevelFull) because a healthy standby is
	// capped at LevelSubscribed by design; both failure classes still trip
	// it (rotated credentials never connect; a rejected SUBACK never subscribes).
	bootstrapConvergenceReadyLevel = ports.LevelSubscribed
)

// startConvergenceWatch begins (or supersedes) the post-swap convergence watch
// for the freshly installed runtime rt. A prior watch is cancelled so at most one
// runs, and the freshly installed runtime resets any predecessor's convergence
// mark (a new swap is a fresh convergence attempt). No-op for a nil runtime/config
// (a swap recorded while nothing is running).
func (a *App) startConvergenceWatch(parent context.Context, rt *goruntime.Runtime, cfg *ports.BridgeConfig) {
	if rt == nil || cfg == nil || parent == nil {
		return
	}
	// Shutdown race guard: Stop cancels rootCtx (== parent) under a.mu, then
	// releases a.mu and blocks on watchWg.Wait(). startConvergenceWatch is only
	// ever reached under a.mu (installPlan, applyInPlace), so once Stop has cancelled rootCtx an
	// admin-commit-driven install racing shutdown observes parent.Err() != nil here
	// and does NOT call watchWg.Go — which would otherwise Add concurrently with
	// Stop's Wait and panic the shutdown goroutine, stranding the lease/cleanup.
	if parent.Err() != nil {
		return
	}
	a.convergenceMu.Lock()
	if a.convergenceWatchCancel != nil {
		a.convergenceWatchCancel()
	}
	ctx, cancel := context.WithCancel(parent)
	a.convergenceWatchCancel = cancel
	a.convergenceRt = rt
	a.convergenceGen++
	gen := a.convergenceGen
	wasDegraded := a.convergenceDegraded
	a.convergenceDegraded = false
	a.convergenceReason = ""
	a.convergenceMu.Unlock()
	if wasDegraded {
		a.emitConfigDegradedGauge(false)
	}

	budget := a.convergenceBudget(cfg)
	a.watchWg.Go(func() {
		a.runConvergenceWatch(ctx, rt, gen, budget)
	})
}

// convergenceBudget derives the watch budget from the committed config: the
// largest post-takeover activation any session's transport declares
// (ports.TransportFailoverTimingConfig), floored at the default. Using the
// transport's own declared worst case keeps the watch honest — it never cries
// wolf on a config whose legitimate activation is genuinely slow.
func (a *App) convergenceBudget(cfg *ports.BridgeConfig) time.Duration {
	budget := bootstrapConvergenceBudgetFloor
	for i := range cfg.Sessions {
		def := &cfg.Sessions[i]
		if ports.IsNilPluginConfig(def.Config) {
			continue
		}
		capability, ok := def.Config.(ports.TransportFailoverTimingConfig)
		if !ok {
			continue
		}
		mode := connectivity.SessionMode(def.SessionMode)
		if mode == "" {
			mode = connectivity.SessionEphemeral
		}
		if d := capability.TransportFailoverTiming(mode).PostTakeoverActivation; d > budget {
			budget = d
		}
	}
	return budget
}

// runConvergenceWatch polls rt until it converges, is superseded, or ctx ends. At
// budget expiry it latches the applied-but-not-converged degraded state (and
// flips MetricConfigDegraded to 1), then KEEPS watching so a later genuine
// convergence clears it — per-session supervision retries forever.
//
// It carries no config version of its own. A reload that says the same thing as
// the running one keeps this runtime and only adopts the new document, so the
// version the App reports as applied can move while this watch runs. Every
// diagnostic below therefore reads the version at the moment it is written,
// which is what CurrentAppliedConfig reports to the operator at that moment.
//
// gen is the watch generation startConvergenceWatch gave it; see
// convergenceWatcherCurrent.
func (a *App) runConvergenceWatch(ctx context.Context, rt *goruntime.Runtime, gen uint64, budget time.Duration) {
	deadline := a.clk.Now().Add(budget)
	timer := a.clk.NewTimer(bootstrapConvergencePollInterval)
	defer timer.Stop()

	marked := false
	for {
		if !a.convergenceWatcherCurrent(rt, gen) {
			return
		}
		if rt.ReadinessLevel(ctx) >= bootstrapConvergenceReadyLevel {
			a.clearConvergenceDegraded(rt, gen)
			return
		}
		if !marked && !a.clk.Now().Before(deadline) {
			configVersion := a.appliedConfigVersion()
			reason := fmt.Sprintf(
				"config version %d applied but transport sessions have not converged (want at least %s) "+
					"within the %s activation budget; the reload committed while the transport cannot reach "+
					"its declared broker state — check session health / broker-side denials and revert the "+
					"config if the sessions cannot converge",
				configVersion, bootstrapConvergenceReadyLevel, budget)
			if a.markConvergenceDegraded(rt, gen, reason) {
				marked = true
				a.logger.Warn("bootstrap: reload applied but NOT converged — apply succeeded while the "+
					"transport has not reached its declared broker state; ConfigDegraded=1 with "+
					"the convergence reason until sessions converge or the config is reverted",
					"config_version", configVersion, "budget", budget.String())
			} else {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
			timer.Reset(bootstrapConvergencePollInterval)
		}
	}
}

// convergenceWatcherCurrent reports whether rt is still the App's installed
// runtime, gen the newest watch generation, and the App not wedged, so a
// superseded watcher never clobbers its successor's state. The runtime alone
// cannot tell the watches apart: an in-place reload keeps the runtime while the
// configuration it runs — and so the budget it is judged by — changes.
func (a *App) convergenceWatcherCurrent(rt *goruntime.Runtime, gen uint64) bool {
	return a.runtimeRef.Get() == rt && !a.wedged.Load() && a.convergenceGeneration() == gen
}

// convergenceGeneration is the generation of the newest convergence watch.
func (a *App) convergenceGeneration() uint64 {
	a.convergenceMu.Lock()
	defer a.convergenceMu.Unlock()
	return a.convergenceGen
}

// markConvergenceDegraded latches the degraded state iff rt is still installed,
// the App not wedged, and gen the newest watch generation.
func (a *App) markConvergenceDegraded(rt *goruntime.Runtime, gen uint64, reason string) bool {
	a.convergenceMu.Lock()
	// installPlan publishes a runtime before its watch replaces this one, so the
	// installed runtime is read under the lock that gates the mark, not before it.
	if a.convergenceRt != rt || a.convergenceGen != gen || a.runtimeRef.Get() != rt || a.wedged.Load() {
		a.convergenceMu.Unlock()
		return false
	}
	a.convergenceDegraded = true
	a.convergenceReason = reason
	a.convergenceMu.Unlock()
	a.emitConfigDegradedGauge(true)
	return true
}

// appliedConfigVersion reports the version of the config the App has applied, or
// 0 when it has applied none.
func (a *App) appliedConfigVersion() int {
	applied := a.appliedRef.Get()
	if applied == nil {
		return 0
	}
	return applied.Version
}

// clearConvergenceDegraded clears the degraded state iff rt is still installed,
// gen is the newest watch generation, and the mark belongs to this runtime.
//
// The version logged is read here, for the same reason the mark reads it where it
// writes its reason: it must name the document the App holds now, not one a
// skipped reload has already adopted in its place.
func (a *App) clearConvergenceDegraded(rt *goruntime.Runtime, gen uint64) {
	configVersion := a.appliedConfigVersion()
	a.convergenceMu.Lock()
	owned := a.convergenceRt == rt && a.convergenceGen == gen && a.convergenceDegraded
	if owned {
		a.convergenceDegraded = false
		a.convergenceReason = ""
	}
	a.convergenceMu.Unlock()
	if !owned {
		return
	}
	a.emitConfigDegradedGauge(false)
	a.logger.Info("bootstrap: transport sessions converged after the activation budget; clearing "+
		"applied-but-not-converged degraded state", "config_version", configVersion)
}

// convergenceDegradedState returns the current applied-but-not-converged state
// for the deep-health projection (degradedConfigWatch).
func (a *App) convergenceDegradedState() (bool, string) {
	a.convergenceMu.Lock()
	defer a.convergenceMu.Unlock()
	return a.convergenceDegraded, a.convergenceReason
}

// emitConfigDegradedGauge emits the 0/1 ConfigDegraded gauge, mirroring the
// generic Supervisor so operators get the same signal from the shipped process.
func (a *App) emitConfigDegradedGauge(degraded bool) {
	if a.metricsExporter == nil {
		return
	}
	value := 0.0
	if degraded {
		value = 1.0
	}
	a.metricsExporter.Gauge(shared.MetricConfigDegraded, value)
}
