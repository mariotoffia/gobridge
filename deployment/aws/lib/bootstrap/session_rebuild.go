package bootstrap

import (
	"context"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

// onSessionUnrecoverable is the runtime's session-unrecoverable handler. It
// takes the rebuild when the installed configuration has a unit for the
// session that can be rebuilt in place, and runs it on its own goroutine: the
// runtime calls this on the session's supervisor, which a reload waits for.
// While an apply holds the apply lock it may run sessions the registry it
// installs last does not hold yet, so the handler takes the report and the
// rebuild decides under the lock.
func (a *App) onSessionUnrecoverable(sessionID string, _ error) bool {
	if a.mu.TryLock() {
		installed := a.registryRef.Load()
		a.mu.Unlock()
		if installed == nil {
			return false
		}
		if _, ok := bridge.PlanSessionRebuild(installed.cfg, sessionID, installed.transports); !ok {
			return false
		}
	}
	go a.rebuildSession(sessionID)
	return true
}

// rebuildSession rebuilds the unit that holds sessionID in place, serialized
// with every config reload by the App's apply lock. It does nothing when the
// installed runtime no longer runs or no longer records the fault: a reload or
// a stop got there first. It does nothing either once the App shuts down, its
// configuration is withdrawn, or it is wedged. When the rebuild leaves the
// fault in place, or a unit does not stop cleanly, the App stops the runtime
// and wedges, which restarts the process as the terminal session did before
// (ADR-0004).
//
// A rebuild that applies is not a reload: the applied configuration, the
// installed registry and the API keys stay as they are, and it seeds no
// baseline, starts no convergence watch and reports no installed runtime. A
// torn rebuild recovers the old configuration as a torn reload does.
func (a *App) rebuildSession(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// rootCtx, or Background for an App driven without Start.
	ctx := a.runtimeStartCtx(context.Background())
	rt, installed := a.runtimeRef.Get(), a.registryRef.Load()
	if rt == nil || installed == nil || !rt.IsRunning() || !rt.SessionUnrecoverable(sessionID) {
		return
	}
	if a.authorize(a.observationEpoch.Load()) != nil {
		return
	}
	oldApplied := a.appliedRef.Get()
	reload, ok := bridge.PlanSessionRebuild(installed.cfg, sessionID, installed.transports)
	if !ok { // the installed configuration changed under the report; nothing is safe to rebuild
		a.logger.Error("bootstrap: no unit to rebuild the failed session in; stopping the runtime",
			"session_id", sessionID)
		if err := stopRuntime(context.Background(), rt, oldApplied); err != nil {
			a.logger.Error("bootstrap: stop runtime", "error", err)
		}
		a.enterWedgedState()
		a.closeSupersededHTTP(ctx, installed)
		return
	}
	reload.DrainTimeout = drainTimeout(oldApplied)
	partBuilder := func(cfg *ports.BridgeConfig) *bridge.Builder { return installed.registerOn(a.newBuilder(cfg)) }
	phase := func(context.Context) (context.Context, context.CancelFunc) {
		return context.WithTimeout(a.runtimeStartCtx(ctx), applyAttemptBudget)
	}
	outcome, err := reload.Apply(ctx, rt, partBuilder, phase)
	// A rebuild refused before it retired anything leaves the session failed and
	// unserved, which only a process restart clears now. Not so at shutdown: the
	// runtime is being stopped anyway.
	if outcome == bridge.InPlaceUnchanged && ctx.Err() == nil && rt.IsRunning() && rt.SessionUnrecoverable(sessionID) {
		outcome = bridge.InPlaceWedged
	}
	if outcome == bridge.InPlaceTorn || outcome == bridge.InPlaceWedged {
		// A torn rebuild rebuilds the old configuration as a whole runtime: bound
		// it as an apply attempt is bounded, so it cannot hold the apply lock
		// for as long as a credential or broker call hangs.
		settleCtx, cancel := context.WithTimeout(ctx, applyAttemptBudget)
		err = a.settleFailedInPlace(settleCtx, outcome, err, rt, installed, oldApplied)
		cancel()
	}

	retiredRoutes, addedRoutes, retiredSessions, addedSessions := reload.Summary()
	fields := []any{
		"session_id", sessionID, "outcome", outcome.String(),
		"retired_routes", retiredRoutes, "added_routes", addedRoutes,
		"retired_sessions", retiredSessions, "added_sessions", addedSessions,
	}
	switch outcome {
	case bridge.InPlaceApplied:
		a.logger.Info("bootstrap: rebuilt failed session in place", fields...)
	case bridge.InPlaceUnchanged:
		a.logger.Warn("bootstrap: session rebuild did not apply", append(fields, "error", err)...)
	default:
		a.logger.Error("bootstrap: session rebuild failed", append(fields, "error", err)...)
	}
}
