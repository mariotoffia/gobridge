package main

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/ports"
)

// degradedConfigWatch combines the two independent live-reconfiguration
// degraded signals into the single projection surfaced on /deephealth. The
// supervisor reports degraded when its config-change stream closes; the config
// manager reports degraded when a layer's change watcher cannot be
// re-established. Either means the bridge keeps serving its last good config but
// can no longer observe config changes without a restart.
func degradedConfigWatch(sup *bridge.Supervisor, mgr *config.Manager) (bool, string) {
	if degraded, reason := sup.Degraded(); degraded {
		return true, reason
	}
	if reason := watchErrorReason(mgr.WatchErrors()); reason != "" {
		return true, reason
	}
	return false, ""
}

func configWatchHealth(sup *bridge.Supervisor, mgr *config.Manager, bootHTTP *ports.HTTPConfig) httpapi.ConfigWatchHealth {
	degraded, reason := degradedConfigWatch(sup, mgr)
	status := httpapi.ConfigWatchHealth{
		Degraded:           degraded,
		Reason:             reason,
		ReconfigurePending: mgr.ReconfigurePending(),
	}
	if version, ok := mgr.AppliedVersion(); ok {
		status.DesiredVersion = &version
	}
	if version, ok := mgr.RunningVersion(); ok {
		status.RunningVersion = &version
	}
	if err := mgr.LastApplyError(); err != nil {
		status.LastApplyError = err.Error()
		status.Degraded = true
	}
	if running := sup.Config(); running != nil {
		status.RestartRequired = httpTopologyRestartReason(bootHTTP, running.HTTP)
	}
	if status.ReconfigurePending {
		status.Degraded = true
		if status.Reason == "" {
			status.Reason = "desired configuration is not running"
		}
	}
	// A coordinated cluster rollout makes ReconfigurePending expected rather
	// than alarming for as long as the barrier is undecided: this member has
	// deliberately not applied a config the cohort has not committed. Surface
	// the barrier so an operator reading /deephealth sees WHO the cohort is
	// waiting for instead of a bare "desired configuration is not running".
	if rollout, ok := sup.RolloutStatus(); ok {
		status.Rollout = &httpapi.ClusterRolloutHealth{
			MemberID:        rollout.MemberID,
			Generation:      rollout.Generation,
			State:           rollout.State,
			ConfigVersion:   rollout.ConfigVersion,
			Epoch:           rollout.Epoch,
			Acked:           rollout.Acked,
			Nacked:          rollout.Nacked,
			Reason:          rollout.Reason,
			CandidateStaged: rollout.Staged,
			Applied:         rollout.Applied,

			ObservationAgeMS:   rollout.ObservationAge.Milliseconds(),
			Stale:              rollout.Stale,
			LastError:          rollout.LastError,
			ArtifactGeneration: rollout.ArtifactGeneration,
			TerminalGeneration: rollout.TerminalGeneration,
			TerminalReason:     rollout.TerminalReason,
		}
	}
	return status
}

// terminalPollInterval is how often the liveness backstop checks whether the
// current runtime has gone terminal. Terminal state only follows a sustained
// failure (e.g. ~30s of lease-store outage before step-down), so a coarse poll
// is ample and cheap.
const terminalPollInterval = 5 * time.Second

// terminalConfirmSamples is how many CONSECUTIVE positive terminal reads the
// backstop requires before it exits the process. A single positive sample can
// be a transient read during a healthy reconfiguration swap window; requiring N
// consecutive confirmations means a swap-window blip never kills a healthy
// process, while a genuine terminal/wedged state (which persists) still trips
// after N×terminalPollInterval.
const terminalConfirmSamples = 3

// watchTerminal polls isTerminal every poll interval, returning true only after
// terminalConfirmSamples CONSECUTIVE positive reads (so a transient swap-window
// blip never kills a healthy process), or false when ctx is cancelled. It
// carries no runtime knowledge itself (the caller supplies the predicate), which
// keeps the poll loop trivially testable.
func watchTerminal(ctx context.Context, clk clock.Clock, poll time.Duration, isTerminal func() bool) bool {
	consecutive := 0
	for {
		select {
		case <-ctx.Done():
			return false
		case <-clk.After(poll):
			if isTerminal() {
				consecutive++
				if consecutive >= terminalConfirmSamples {
					return true
				}
			} else {
				consecutive = 0
			}
		}
	}
}
