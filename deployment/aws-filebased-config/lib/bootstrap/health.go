package bootstrap

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mariotoffia/gobridge/httpapi"
)

// The App's deep-health projection of live reconfiguration: whether this process
// can still observe config changes, whether it is running the config it was told
// to run, and — for a coordinated cohort — what the rollout barrier and this
// member's durable baseline look like.

// degradedConfigWatch reports whether live reconfiguration is degraded: the
// config manager's layer watcher cannot be (re)established, so the App keeps
// serving its last good config but no longer observes config changes. It is the
// deep-health projection wired to httpapi's DegradedProvider so operators can
// see a bridge running blind. The terminal "wedged" state (no active runtime at
// all) is a separate, harder failure already surfaced via /live and the
// terminal backstop, so it is intentionally not folded in here.
func (a *App) degradedConfigWatch() (bool, string) {
	// RECONFIG-1: an applied-but-not-converged swap is a degraded state even when
	// the config-watch layer itself is healthy. Report it alongside (or instead of)
	// a watcher error so operators see the same ConfigDegraded signal the generic
	// Supervisor emits.
	convDegraded, convReason := a.convergenceDegradedState()

	if a.manager == nil {
		return convDegraded, convReason
	}
	errs := a.manager.WatchErrors()
	if len(errs) == 0 {
		return convDegraded, convReason
	}
	layers := make([]string, 0, len(errs))
	for layer := range errs {
		layers = append(layers, layer)
	}
	slices.Sort(layers) // deterministic reason ordering
	parts := make([]string, len(layers))
	for i, layer := range layers {
		parts[i] = fmt.Sprintf("%s: %v", layer, errs[layer])
	}
	reason := "config watch degraded: " + strings.Join(parts, "; ")
	if convDegraded {
		reason += "; " + convReason
	}
	return true, reason
}

func (a *App) configWatchHealth() httpapi.ConfigWatchHealth {
	degraded, reason := a.degradedConfigWatch()
	status := httpapi.ConfigWatchHealth{Degraded: degraded, Reason: reason}
	if a.manager == nil {
		return status
	}
	status.ReconfigurePending = a.manager.ReconfigurePending()
	if version, ok := a.manager.AppliedVersion(); ok {
		status.DesiredVersion = &version
	}
	if version, ok := a.manager.RunningVersion(); ok {
		status.RunningVersion = &version
	}
	if err := a.manager.LastApplyError(); err != nil {
		status.LastApplyError = err.Error()
		status.Degraded = true
	}
	if status.ReconfigurePending {
		status.Degraded = true
		if status.Reason == "" {
			status.Reason = "desired configuration is not running"
		}
	}
	// Surface the coordinated cluster rollout barrier (design §9) so an operator
	// reading deep health during a rollout sees WHO the cohort is waiting for
	// instead of a bare "desired configuration is not running". Omitted entirely
	// when no barrier runs.
	if a.rolloutDriver != nil {
		if r, ok := a.rolloutDriver.Status(); ok {
			status.Rollout = &httpapi.ClusterRolloutHealth{
				MemberID:        r.MemberID,
				Generation:      r.Generation,
				State:           r.State,
				ConfigVersion:   r.ConfigVersion,
				Epoch:           r.Epoch,
				Acked:           r.Acked,
				Nacked:          r.Nacked,
				Reason:          r.Reason,
				CandidateStaged: r.Staged,
				Applied:         r.Applied,

				ObservationAgeMS:   r.ObservationAge.Milliseconds(),
				Stale:              r.Stale,
				LastError:          r.LastError,
				ArtifactGeneration: r.ArtifactGeneration,
				TerminalGeneration: r.TerminalGeneration,
				TerminalReason:     r.TerminalReason,
			}
			// The generation-zero baseline this member verified at startup: what a
			// restart of THIS member would recover to. An operator diagnosing a
			// mixed cohort reads it beside the observed generation.
			if b := a.baselineRef.Load(); b != nil {
				status.Rollout.BaselineGeneration = b.Generation
				status.Rollout.BaselineDigest = b.Digest
			}
		}
	}
	return status
}
