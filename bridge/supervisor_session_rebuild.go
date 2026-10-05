package bridge

import (
	"fmt"
	"maps"
)

// onSessionUnrecoverable is the runtime's session-unrecoverable handler. It
// takes the rebuild when the running configuration has a unit for the session
// that can be rebuilt in place, and runs it on its own goroutine: the runtime
// calls this on the session's supervisor, which a reload waits for.
func (s *Supervisor) onSessionUnrecoverable(sessionID string, _ error) bool {
	s.mu.RLock()
	cfg := s.cfg
	transports := maps.Clone(s.transports)
	s.mu.RUnlock()
	if _, ok := PlanSessionRebuild(cfg, sessionID, transports); !ok {
		return false
	}
	go s.rebuildSession(sessionID)
	return true
}

// rebuildSession rebuilds the unit that holds sessionID in place, under the
// lifecycle lock every reload takes. It does nothing when the current runtime
// no longer runs or no longer records the fault for the session: a reload or a
// stop got there first. It does nothing either once the Supervisor shuts
// down. When the rebuild leaves the fault in place, or a unit does not stop
// cleanly, the Supervisor stops the runtime and wedges, which restarts the
// process as the terminal session did before (ADR-0004).
//
// A rebuild is not a reload: the running configuration, the convergence watch
// and the reload metrics stay as they are, and no SwapEvent is emitted.
func (s *Supervisor) rebuildSession(sessionID string) {
	s.mu.RLock()
	ctx := s.baseCtx
	s.mu.RUnlock()
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.mu.RLock()
	rt, cfg := s.rt, s.cfg
	transports := maps.Clone(s.transports)
	s.mu.RUnlock()
	if ctx == nil || ctx.Err() != nil || rt == nil || !rt.IsRunning() || !rt.SessionUnrecoverable(sessionID) {
		return
	}
	s.mu.Lock()
	s.swapping = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.swapping = false
		s.mu.Unlock()
	}()
	plan, ok := PlanSessionRebuild(cfg, sessionID, transports)
	if !ok { // the running configuration changed under the report; nothing is safe to rebuild
		err := fmt.Errorf("session %q", sessionID)
		if s.logger != nil {
			s.logger.Error("supervisor: no unit to rebuild the failed session in; stopping the runtime",
				"session_id", sessionID)
		}
		s.stopAbandoned(ctx, rt, cfg)
		s.wedgeAfterFailedStop("a session rebuild found no unit for the failed session", err)
		return
	}
	plan.DrainTimeout = s.drainTimeoutFrom(cfg)
	outcome, err := plan.Apply(ctx, rt, s.newBuilder, s.swapPhaseCtx)
	// A rebuild refused before it retired anything leaves the session failed and
	// unserved, which only a process restart clears now. Not so at shutdown: the
	// runtime is being stopped anyway.
	if outcome == InPlaceUnchanged && ctx.Err() == nil && rt.IsRunning() && rt.SessionUnrecoverable(sessionID) {
		outcome = InPlaceWedged
	}
	if outcome == InPlaceTorn || outcome == InPlaceWedged {
		err = s.settleFailedInPlace(ctx, rt, cfg, outcome, err,
			"a session rebuild could not clear the failed session or stop its unit cleanly")
	}
	if s.logger == nil {
		return
	}
	fields := append([]any{"session_id", sessionID, "outcome", outcome.String()}, inPlaceLogFields(plan)...)
	switch outcome {
	case InPlaceApplied:
		s.logger.Info("supervisor: rebuilt failed session in place", fields...)
	case InPlaceUnchanged:
		s.logger.Warn("supervisor: session rebuild did not apply", append(fields, "error", err)...)
	default:
		s.logger.Error("supervisor: session rebuild failed", append(fields, "error", err)...)
	}
}
