package bootstrap

import (
	"context"
	"errors"
	"fmt"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

// applyInPlace reloads the installed runtime to logical in place when the
// change is confined to reload units (bridge.PlanInPlaceReload says which
// changes are): only the units that differ are retired and added, and every
// other session, receiver, sender and route keeps running untouched. handled
// is false when the reload takes a full swap instead — no running runtime is
// installed, or the change is not confined to units — and nothing was done.
//
// Parts are built over the installed registry's own factories: its HTTP
// transport mounted the endpoints the unchanged units still serve on the mux
// the transport server holds, and a factory latches capabilities from what it
// built.
//
// A failure ends as the matching failure of a full swap does. When nothing
// changed or the retired units were restored, the runtime keeps serving the
// running configuration. When it runs neither configuration, it is stopped and
// the running configuration rebuilt, as a failed prepare/commit commit does.
// When a unit did not stop cleanly, or the torn runtime does not, the App
// stops it and wedges (ADR-0004): its sessions may still hold the broker
// identities a rebuild would claim.
func (a *App) applyInPlace(ctx context.Context, logical *ports.BridgeConfig, inputs *resolvedInputs, epoch uint64) (bool, error) {
	rt, installed := a.runtimeRef.Get(), a.registryRef.Load()
	if rt == nil || installed == nil || !rt.IsRunning() {
		return false, nil
	}
	reload, ok := bridge.PlanInPlaceReload(installed.cfg, inputs.RuntimeConfig, installed.transports)
	if !ok {
		return false, nil
	}
	newBuilder := func(cfg *ports.BridgeConfig) *bridge.Builder { return installed.registerOn(a.newBuilder(cfg)) }
	if err := a.seedManagedSubscriptionBaselines(ctx, inputs.RuntimeConfig, newBuilder(inputs.RuntimeConfig)); err != nil {
		return true, err
	}
	if err := a.authorize(epoch); err != nil {
		return true, err
	}
	oldApplied := a.appliedRef.Get()
	// Apply's teardown gets the budget every reload-path stop of this root gets.
	reload.DrainTimeout = drainTimeout(oldApplied)
	outcome, err := reload.Apply(ctx, rt, newBuilder, nil)
	a.logInPlaceReload(reload, outcome, err)

	switch outcome {
	case bridge.InPlaceApplied:
		if err := a.authorize(epoch); err != nil {
			return true, err
		}
		a.appliedRef.Set(logical)
		a.apiKeysRef.Set(inputs.AdminAPIKey, inputs.MonitorAPIKey)
		// The same factories and mux keep serving; only the configuration they
		// run changed.
		reg := *installed
		reg.cfg = inputs.RuntimeConfig
		a.registryRef.Store(&reg)
		if a.rootCtx != nil {
			a.startConvergenceWatch(a.rootCtx, rt, logical)
		}
		if a.onRuntimeInstalled != nil {
			a.onRuntimeInstalled()
		}
		return true, nil
	case bridge.InPlaceTorn:
		if stopErr := stopRuntime(context.Background(), rt, oldApplied); stopErr != nil {
			err = errors.Join(err, fmt.Errorf("stop runtime: %w", stopErr))
			a.enterWedgedState()
		} else {
			a.runtimeRef.Set(nil)
			a.recoverPrevious(ctx, oldApplied)
		}
		// Recovery installs a registry of its own, and a wedge serves none.
		a.closeSupersededHTTP(ctx, installed)
	case bridge.InPlaceWedged:
		_ = stopRuntime(context.Background(), rt, oldApplied)
		a.enterWedgedState()
		a.closeSupersededHTTP(ctx, installed)
	}
	return true, fmt.Errorf("bootstrap: in-place reload (%s): %w", outcome, err)
}

// logInPlaceReload logs what an in-place reload retired and added, and how it
// ended.
func (a *App) logInPlaceReload(reload *bridge.InPlaceReload, outcome bridge.InPlaceOutcome, err error) {
	retiredRoutes, addedRoutes, retiredSessions, addedSessions := reload.Summary()
	fields := []any{
		"outcome", outcome.String(),
		"retired_routes", retiredRoutes, "added_routes", addedRoutes,
		"retired_sessions", retiredSessions, "added_sessions", addedSessions,
	}
	if err != nil {
		a.logger.Warn("bootstrap: in-place reload failed", append(fields, "error", err)...)
		return
	}
	a.logger.Info("bootstrap: reloaded in place", fields...)
}
