package bridge

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// autoSwap reports whether the Supervisor chooses the swap mode of each reload.
// An explicit SwapInPlace counts: it can only ever be chosen, so taken as a
// full swap mode it would fall back to an overlap swap even where an exclusive
// broker identity requires a prepare-commit one.
func (s *Supervisor) autoSwap() bool {
	return s.swapMode == SwapAuto || s.swapMode == SwapInPlace
}

// planInPlace returns the plan of an in-place reload from oldCfg, which oldRt
// runs, to newCfg, or nil when the reload takes a full replacement: an explicit
// full swap mode asks for one, no running runtime is there to keep, or the
// change is not confined to reload units (PlanInPlaceReload says which changes
// are).
func (s *Supervisor) planInPlace(oldRt *runtime.Runtime, oldCfg, newCfg *ports.BridgeConfig) *InPlaceReload {
	if !s.autoSwap() || oldRt == nil || !oldRt.IsRunning() {
		return nil
	}
	s.mu.RLock()
	transports := maps.Clone(s.transports)
	s.mu.RUnlock()
	plan, ok := PlanInPlaceReload(oldCfg, newCfg, transports)
	if !ok {
		return nil
	}
	return plan
}

// applyInPlace reloads oldRt, which runs oldCfg, in place by plan, and returns
// the runtime that runs the next configuration: oldRt itself. Each build phase
// runs under a swap deadline of its own, as the phases of a prepare-commit swap
// do, and each retire under the drain timeout the Supervisor's own stops use.
//
// A failure ends as the matching failure of a full swap does. When nothing
// changed or the retired units were restored, oldRt keeps serving the running
// configuration. When oldRt runs neither configuration, it is replaced as a
// failed swap replaces the old runtime: stopped, then the running configuration
// built afresh, or a wedge when either step fails. When a retired unit did not
// stop cleanly, the Supervisor stops oldRt and wedges (ADR-0004).
func (s *Supervisor) applyInPlace(ctx context.Context, oldRt *runtime.Runtime, oldCfg *ports.BridgeConfig, plan *InPlaceReload) (*runtime.Runtime, error) {
	plan.DrainTimeout = s.drainTimeoutFrom(oldCfg)
	outcome, err := plan.Apply(ctx, oldRt, s.newBuilder, s.swapPhaseCtx)
	switch outcome {
	case InPlaceApplied:
		// oldRt now runs the next configuration, so the watch started for the
		// running one must stop judging it before applyConfig publishes the
		// configuration and starts the watch of the next.
		s.nextConvergenceGen()
		return oldRt, nil
	case InPlaceTorn:
		// A runtime that does not stop cleanly may still hold the broker
		// identities the rebuild would claim a second time, exactly as an old
		// runtime whose Stop fails during a full swap.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.drainTimeoutFrom(oldCfg))
		stopErr := oldRt.Stop(stopCtx)
		cancel()
		if stopErr != nil {
			err = errors.Join(err, fmt.Errorf("stop old runtime: %w", stopErr))
			s.wedgeAfterFailedStop(stopErr)
			break
		}
		s.recoverOldOrWedge(ctx, oldCfg)
	case InPlaceWedged:
		s.stopAbandoned(ctx, oldRt, oldCfg)
		s.wedgeAfterFailedStop(err)
	}
	return nil, fmt.Errorf("in-place reload (%s): %w", outcome, err)
}

// inPlaceLogFields are the log fields naming what plan retired and added, or
// none when the reload was not in place.
func inPlaceLogFields(plan *InPlaceReload) []any {
	if plan == nil {
		return nil
	}
	retiredRoutes, addedRoutes, retiredSessions, addedSessions := plan.Summary()
	return []any{
		"retired_routes", retiredRoutes, "added_routes", addedRoutes,
		"retired_sessions", retiredSessions, "added_sessions", addedSessions,
	}
}
