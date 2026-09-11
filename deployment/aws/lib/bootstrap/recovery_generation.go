package bootstrap

import "context"

// Withdrawal never waits for a build. Only the short Start transition shares
// this lock, so a recovery cannot begin running after its generation is revoked.
func (a *App) withdrawConfiguration() {
	a.generationMu.Lock()
	a.missing.Store(true)
	a.observationEpoch.Add(1)
	if a.recoveryCancel != nil {
		a.recoveryCancel()
	}
	candidate := a.recoveryRuntime
	a.generationMu.Unlock()
	if candidate != nil {
		candidate.Fence()
	}
	if rt := a.runtimeRef.Get(); rt != nil && rt != candidate {
		rt.Fence()
	}
	if a.applying.Load() {
		a.failDormancy()
	}
}

func (a *App) recoveryContext(parent context.Context) (context.Context, func()) {
	a.generationMu.Lock()
	epoch := a.observationEpoch.Load()
	ctx, cancel := context.WithCancel(parent)
	a.recoveryCancel = cancel
	if a.missing.Load() || a.wedged.Load() {
		cancel()
	}
	a.generationMu.Unlock()
	return context.WithValue(ctx, repositoryEpochKey{}, epoch), func() {
		a.generationMu.Lock()
		cancel()
		a.recoveryCancel, a.recoveryRuntime = nil, nil
		a.generationMu.Unlock()
	}
}

func (a *App) startRecovery(ctx, lifetime context.Context, plan *runtimePlan) error {
	a.generationMu.Lock()
	defer a.generationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.authorizePlan(plan); err != nil {
		return err
	}
	// Keep the unpublished candidate reachable by withdrawal until installation
	// or cleanup finishes; runtimeRef alone has a Start-to-install gap.
	a.recoveryRuntime = plan.runtime
	return plan.runtime.Start(lifetime)
}

// An apply deadline is not withdrawal. If recovery fails under that deadline
// while the process still authorizes the generation, the caller must fail
// terminally rather than leave a healthy process without its stopped runtime.
func (a *App) recoveryRevoked(ctx context.Context) bool {
	if a.missing.Load() || a.wedged.Load() {
		return true
	}
	if epoch, ok := ctx.Value(repositoryEpochKey{}).(uint64); ok && epoch != a.observationEpoch.Load() {
		return true
	}
	return a.rootCtx != nil && a.rootCtx.Err() != nil
}
