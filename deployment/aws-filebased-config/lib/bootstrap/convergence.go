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

// Post-swap convergence watch for the shipped AWS bootstrap (RECONFIG-1).
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
	// ever reached under a.mu (installPlan), so once Stop has cancelled rootCtx an
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
	wasDegraded := a.convergenceDegraded
	a.convergenceDegraded = false
	a.convergenceReason = ""
	a.convergenceMu.Unlock()
	if wasDegraded {
		a.emitConfigDegradedGauge(false)
	}

	budget := a.convergenceBudget(cfg)
	version := cfg.Version
	a.watchWg.Go(func() {
		a.runConvergenceWatch(ctx, rt, version, budget)
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
func (a *App) runConvergenceWatch(ctx context.Context, rt *goruntime.Runtime, configVersion int, budget time.Duration) {
	deadline := a.clk.Now().Add(budget)
	timer := a.clk.NewTimer(bootstrapConvergencePollInterval)
	defer timer.Stop()

	marked := false
	for {
		if !a.convergenceWatcherCurrent(rt) {
			return
		}
		if rt.ReadinessLevel(ctx) >= bootstrapConvergenceReadyLevel {
			a.clearConvergenceDegraded(rt, configVersion)
			return
		}
		if !marked && !a.clk.Now().Before(deadline) {
			reason := fmt.Sprintf(
				"config version %d applied but transport sessions have not converged (want at least %s) "+
					"within the %s activation budget; the reload committed while the transport cannot reach "+
					"its declared broker state — check session health / broker-side denials and revert the "+
					"config if the sessions cannot converge",
				configVersion, bootstrapConvergenceReadyLevel, budget)
			if a.markConvergenceDegraded(rt, reason) {
				marked = true
				a.logger.Warn("bootstrap: reload applied but NOT converged — apply succeeded while the "+
					"transport has not reached its declared broker state (RECONFIG-1); ConfigDegraded=1 with "+
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
// runtime and the App is not wedged, so a superseded watcher never clobbers its
// successor's state.
func (a *App) convergenceWatcherCurrent(rt *goruntime.Runtime) bool {
	return a.runtimeRef.Get() == rt && !a.wedged.Load()
}

// markConvergenceDegraded latches the degraded state iff rt is still installed.
func (a *App) markConvergenceDegraded(rt *goruntime.Runtime, reason string) bool {
	if !a.convergenceWatcherCurrent(rt) {
		return false
	}
	a.convergenceMu.Lock()
	if a.convergenceRt != rt {
		a.convergenceMu.Unlock()
		return false
	}
	a.convergenceDegraded = true
	a.convergenceReason = reason
	a.convergenceMu.Unlock()
	a.emitConfigDegradedGauge(true)
	return true
}

// clearConvergenceDegraded clears the degraded state iff rt is still installed and
// the mark belongs to this runtime.
func (a *App) clearConvergenceDegraded(rt *goruntime.Runtime, configVersion int) {
	a.convergenceMu.Lock()
	owned := a.convergenceRt == rt && a.convergenceDegraded
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
