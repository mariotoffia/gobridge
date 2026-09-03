package bootstrap

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mariotoffia/gobridge/bridge"
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
	// The rollout block first: it is the one part of this projection that does not
	// come from the config manager, so a deployment wired without one must still
	// publish it rather than reporting a coordinated cohort as having no barrier.
	a.applyRolloutHealth(&status)
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
	return status
}

// applyRolloutHealth folds the coordinated cluster rollout barrier (design §9)
// into the projection, so an operator reading deep health during a rollout sees
// WHO the cohort is waiting for instead of a bare "desired configuration is not
// running" — and so a member the barrier has left behind says so on the field
// every health check already watches. A no-op when no barrier runs.
func (a *App) applyRolloutHealth(status *httpapi.ConfigWatchHealth) {
	if a.rolloutDriver == nil {
		return
	}
	r, ok := a.rolloutDriver.Status()
	if !ok {
		return
	}
	status.Rollout = rolloutHealth(r, a.baselineRef.Load())
	if degraded, reason := r.DegradedState(); degraded {
		status.Degraded = true
		status.Reason = appendReason(status.Reason, reason)
	}
}

// rolloutHealth projects one rollout observation, plus the generation-zero
// baseline this member verified at startup, into the deep-health block.
//
// Every field here answers a question divergence raises and nothing else does:
// Converged is who the confirm barrier still waits for, Applied whether THIS
// member runs the decided generation, ObservedAt/Stale whether the rest of the
// block is current at all, and BaselineGeneration what a restart of this member
// would recover to. baseline is nil when the deployment stamped no admitted
// baseline document.
func rolloutHealth(r bridge.RolloutStatus, baseline *rolloutBaseline) *httpapi.ClusterRolloutHealth {
	out := &httpapi.ClusterRolloutHealth{
		MemberID:        r.MemberID,
		Generation:      r.Generation,
		State:           r.State,
		ConfirmPending:  r.ConfirmPending,
		ConfigVersion:   r.ConfigVersion,
		Epoch:           r.Epoch,
		Acked:           r.Acked,
		Nacked:          r.Nacked,
		Converged:       r.Converged,
		Reason:          r.Reason,
		CandidateStaged: r.Staged,
		NotVoting:       r.NotVoting,
		Applied:         r.Applied,

		ObservedAt:         r.ObservedAt,
		ObservationAgeMS:   r.ObservationAge.Milliseconds(),
		Stale:              r.Stale,
		LastError:          r.LastError,
		ArtifactGeneration: r.ArtifactGeneration,
		TerminalGeneration: r.TerminalGeneration,
		TerminalReason:     r.TerminalReason,
	}
	if baseline != nil {
		out.BaselineGeneration = baseline.Generation
		out.BaselineDigest = baseline.Digest
	}
	return out
}

// appendReason joins a further degraded cause onto an existing reason, so a
// rollout divergence never overwrites (or is overwritten by) a config-watch one.
func appendReason(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}
