package bridge

import (
	"context"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// Post-swap convergence watch.
//
// A successful apply() proves only that the new runtime BUILT and its
// Start() returned — for transports that dial and reconcile in background
// goroutines (MQTT), Start returns before the session has ever reached the
// broker. A syntactically-valid-but-broker-invalid config (an ACL-denied
// topic, rotated-away credentials) therefore commits as a SUCCESSFUL
// reload while the transport is down and every reload-level signal reports
// green; the divergence is visible only in session-level health. The watch
// below closes that gap without changing swap semantics: after each
// committed swap it observes the new runtime until its sessions genuinely
// converge, and surfaces "applied but not converged" as a distinct
// degraded state (MetricConfigDegraded + deep-health reason) when they do
// not within the transport's own declared activation budget. It never
// reverts — per-session supervision keeps retrying, and an operator revert
// stays the remediation — it makes the false-success observable.

const (
	// convergencePollInterval is how often the watch samples the new
	// runtime's readiness. Coarse on purpose: readiness snapshots sweep
	// every session health, and the signal only needs to beat a human
	// reading dashboards, not a control loop.
	convergencePollInterval = 2 * time.Second

	// convergenceBudgetFloor is the minimum time a freshly committed
	// runtime is given to converge before being declared
	// applied-but-not-converged. Covers one default connect (30s) plus one
	// default reconcile (30s) for transports that declare no activation
	// timing of their own.
	convergenceBudgetFloor = 60 * time.Second
)

// convergenceReadyLevel is the readiness level that counts as converged:
// every non-standby session connected AND its declared subscriptions
// satisfied. LevelSubscribed — not LevelFull — because a healthy standby
// instance is capped at LevelSubscribed by design (routes deliberately not
// ready until it holds the lease), and requiring Full would flag every
// active/standby deployment as non-converged forever. Both failure
// classes still trip it: rotated-away credentials never reach connected,
// and a broker-rejected SUBACK never satisfies subscriptions.
const convergenceReadyLevel = ports.LevelSubscribed

// watchPostSwapConvergence starts the background convergence watch for a
// freshly committed runtime. It is a no-op for a nil runtime (a reload
// recorded while the bridge is admin-paused). The watcher self-terminates
// when the runtime converges, is replaced by a later swap, or ctx ends.
func (s *Supervisor) watchPostSwapConvergence(ctx context.Context, rt *runtime.Runtime, cfg *ports.BridgeConfig) {
	if rt == nil || cfg == nil {
		return
	}
	go s.runConvergenceWatch(ctx, rt, cfg.Version, convergenceBudget(cfg))
}

// convergenceBudget derives the watch budget from the committed config: the
// largest post-takeover activation any session's transport declares
// (ports.TransportFailoverTimingConfig — for MQTT that is connect +
// reconcile + grace worst cases), floored at convergenceBudgetFloor. Using
// the transport's own declared worst case keeps the watch honest: it never
// cries wolf on a config whose legitimate activation is slow.
func convergenceBudget(cfg *ports.BridgeConfig) time.Duration {
	budget := convergenceBudgetFloor
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

func convergenceDegradedReason(configVersion int, level ports.ReadinessLevel, budget time.Duration) string {
	return fmt.Sprintf(
		"config version %d applied but transport sessions have not converged (readiness %s, "+
			"want at least %s) within the %s activation budget; the reload committed while the "+
			"transport cannot reach its declared broker state — check session health / broker-side "+
			"denials and revert the config if the sessions cannot converge",
		configVersion, level, convergenceReadyLevel, budget,
	)
}

// runConvergenceWatch polls the runtime until it converges, is replaced, or
// ctx ends. At budget expiry it marks the supervisor degraded with a
// convergence-specific reason (flipping MetricConfigDegraded to 1 and
// surfacing the reason in deep health), then KEEPS watching: per-session
// supervision retries forever, so a later genuine convergence clears the
// state again instead of leaving a stale alarm on a self-healed bridge.
func (s *Supervisor) runConvergenceWatch(ctx context.Context, rt *runtime.Runtime, configVersion int, budget time.Duration) {
	deadline := s.clk.Now().Add(budget)
	timer := s.clk.NewTimer(convergencePollInterval)
	defer timer.Stop()

	marked := false
	for {
		if !s.watcherCurrent(rt) {
			// A later swap replaced this runtime, or the bridge was
			// deliberately paused (StopBridge clears any convergence-owned
			// mark itself): this watcher's observation is obsolete either
			// way. Leave any remaining degraded mark for the successor path
			// (a successful later swap clears degraded; a failed one keeps
			// the operator signal).
			return
		}
		level := rt.ReadinessLevel(ctx)
		if level >= convergenceReadyLevel {
			// Deliberately SILENT on the healthy path (no per-reload log):
			// convergence within budget is the normal case, and a background
			// log here would race test/operator log capture that treats
			// apply() returning as "the swap is done logging". Only the
			// failure (Warn at budget expiry) and the recovery from it
			// (clearConvergenceDegraded's Info) write logs. The clear runs
			// even when THIS watcher never marked: convergence factually
			// resolves any convergence-owned mark a predecessor watcher
			// (an earlier swap or resume) left behind.
			s.clearConvergenceDegraded(rt, configVersion)
			return
		}
		if !marked && !s.clk.Now().Before(deadline) {
			if s.markConvergenceDegraded(rt, convergenceDegradedReason(configVersion, level, budget)) {
				marked = true
				if s.logger != nil {
					s.logger.Warn("supervisor: reload applied but NOT converged — reload success signals are "+
						"green while the transport has not reached its declared broker state; "+
						"MetricConfigDegraded=1 with the convergence reason until sessions converge or the "+
						"config is reverted",
						"config_version", configVersion,
						"readiness", level.String(),
						"budget", budget.String(),
					)
				}
			} else {
				// The runtime changed under us between the check and the mark;
				// the successor watcher owns the signal.
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
			timer.Reset(convergencePollInterval)
		}
	}
}

// watcherCurrent reports whether rt is still the supervisor's active runtime
// AND the bridge is not deliberately paused. A paused bridge's readiness is
// LevelLive by definition (the runtime is stopped), so continuing to observe
// it would convert an admin pause into a false applied-but-not-converged
// alarm; StopBridge owns clearing any existing convergence mark on pause.
func (s *Supervisor) watcherCurrent(rt *runtime.Runtime) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rt == rt && !s.paused
}

// markConvergenceDegraded sets the applied-but-not-converged degraded state
// iff rt is still the active runtime and the bridge is not paused, so a
// watcher for a superseded swap (or a runtime an operator just paused) can
// never clobber the state owned by its successor. Takes convergence
// ownership of the degraded state (degradedByConvergence) so later watchers,
// StopBridge, and apply() can distinguish it from foreign causes. Returns
// whether the mark was applied.
func (s *Supervisor) markConvergenceDegraded(rt *runtime.Runtime, reason string) bool {
	s.mu.Lock()
	if s.rt != rt || s.paused {
		s.mu.Unlock()
		return false
	}
	s.degraded = true
	s.degradedReason = reason
	s.degradedByConvergence = true
	s.mu.Unlock()
	s.emitConfigDegradedGauge(true)
	return true
}

// clearConvergenceDegraded clears the degraded state iff it is
// CONVERGENCE-OWNED (set by this or a predecessor watcher) and rt is still
// active — never someone else's degraded cause (a config-stream failure, a
// half-stopped runtime). Clearing predecessor marks matters: after a
// pause/resume the resumed runtime's watcher is a different instance from
// the one that marked, yet its observed convergence factually resolves the
// alarm.
func (s *Supervisor) clearConvergenceDegraded(rt *runtime.Runtime, configVersion int) {
	s.mu.Lock()
	owned := s.rt == rt && s.degraded && s.degradedByConvergence
	if owned {
		s.degraded = false
		s.degradedReason = ""
		s.degradedByConvergence = false
	}
	s.mu.Unlock()
	if !owned {
		return
	}
	s.emitConfigDegradedGauge(false)
	if s.logger != nil {
		s.logger.Info("supervisor: transport sessions converged after the activation budget; "+
			"clearing applied-but-not-converged degraded state",
			"config_version", configVersion)
	}
}
