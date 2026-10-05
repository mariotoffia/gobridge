package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// String names the mode for logs: overlap, prepare_commit, auto or in_place.
func (m SwapMode) String() string {
	switch m {
	case SwapOverlap:
		return "overlap"
	case SwapPrepareCommit:
		return "prepare_commit"
	case SwapAuto:
		return "auto"
	case SwapInPlace:
		return "in_place"
	}
	return fmt.Sprintf("SwapMode(%d)", int(m))
}

// LogValue logs the mode by name. A JSON handler would otherwise log the
// integer, since it does not consult String.
func (m SwapMode) LogValue() slog.Value { return slog.StringValue(m.String()) }

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
// built afresh, or a wedge when either step fails. When a retired unit, or a
// part built for a serialized reload, did not stop cleanly, the Supervisor
// stops oldRt and wedges (ADR-0004).
func (s *Supervisor) applyInPlace(ctx context.Context, oldRt *runtime.Runtime, oldCfg *ports.BridgeConfig, plan *InPlaceReload) (*runtime.Runtime, error) {
	plan.DrainTimeout = s.drainTimeoutFrom(oldCfg)
	outcome, err := plan.Apply(ctx, oldRt, s.newBuilder, s.swapPhaseCtx)
	if outcome == InPlaceApplied {
		// oldRt now runs the next configuration, so the watch started for the
		// running one must stop judging it before applyConfig publishes the
		// configuration and starts the watch of the next.
		s.nextConvergenceGen()
		return oldRt, nil
	}
	err = s.settleFailedInPlace(ctx, oldRt, oldCfg, outcome, err,
		"an in-place reload could not stop a retired unit or a built part cleanly")
	return nil, fmt.Errorf("in-place reload (%s): %w", outcome, err)
}

// settleFailedInPlace acts on outcome, the outcome of an in-place reload of rt,
// which ran cfg, that did not apply, and returns err joined with any stop
// failure it met. Unchanged leaves rt serving cfg. Torn stops rt and builds cfg
// afresh, or wedges when either step fails. Wedged stops rt and wedges, naming
// wedgeReason (ADR-0004).
func (s *Supervisor) settleFailedInPlace(ctx context.Context, rt *runtime.Runtime, cfg *ports.BridgeConfig,
	outcome InPlaceOutcome, err error, wedgeReason string,
) error {
	switch outcome {
	case InPlaceTorn:
		// A runtime that does not stop cleanly may still hold the broker
		// identities the rebuild would claim a second time, exactly as an old
		// runtime whose Stop fails during a full swap.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.drainTimeoutFrom(cfg))
		stopErr := rt.Stop(stopCtx)
		cancel()
		if stopErr != nil {
			err = errors.Join(err, fmt.Errorf("stop old runtime: %w", stopErr))
			s.wedgeAfterFailedStop("old runtime stop failed", stopErr)
			break
		}
		s.recoverOldOrWedge(ctx, cfg)
	case InPlaceWedged:
		s.stopAbandoned(ctx, rt, cfg)
		s.wedgeAfterFailedStop(wedgeReason, err)
	}
	return err
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
