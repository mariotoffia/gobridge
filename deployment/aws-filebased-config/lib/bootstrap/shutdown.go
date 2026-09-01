package bootstrap

import (
	"context"
	"net/http"
)

// App shutdown and the terminal-state watch: the SIGTERM path, the blocking Run
// loop a host process waits on, and the two ways this process decides it must
// exit — a runtime that entered a terminal state, and a wedged App that has no
// runtime at all and cannot get one back on its own.

func (a *App) Stop(ctx context.Context) error {
	a.mu.Lock()
	if !a.started {
		a.mu.Unlock()
		return nil
	}
	a.started = false
	if a.watchCancel != nil {
		a.watchCancel()
		a.watchCancel = nil
	}
	a.mu.Unlock()

	// Wait for watchLoop goroutine to finish before tearing down resources it may
	// still be using (e.g. mid-applyLogicalConfig) — but SELECTABLY against the
	// shutdown budget. A reload's own teardown is deliberately not bounded by
	// process shutdown, so an unconditional wait here put a stuck reload ahead of
	// the runtime drain, the HTTP shutdown and the metrics flush: SIGTERM never
	// reached them before the platform's SIGKILL. When the budget runs out we
	// proceed; the watcher context is already cancelled and the goroutine unwinds
	// on its own.
	if !waitCtx(ctx, a.watchWg.Wait) && a.logger != nil {
		a.logger.Warn("bootstrap: shutdown budget expired waiting for the config watcher to unwind; " +
			"continuing with runtime and server teardown")
	}

	// Stop the coordinated cluster rollout drive before tearing down the runtime it
	// swaps: this waits for its goroutine to exit and resigns the coordinator lease
	// so a successor takes over immediately instead of waiting out the lease TTL.
	// The wait is bounded by the same budget — a lease store that will not release
	// must not hold the rest of shutdown behind it.
	if a.stopRolloutDrive != nil {
		a.stopRolloutDrive(ctx)
		a.stopRolloutDrive = nil
	}

	manager := a.manager
	httpServer := a.httpServer
	transportServer := a.transportServer
	metricsExporter := a.metricsExporter
	currentRuntime := a.runtimeRef.Get()
	currentApplied := a.appliedRef.Get()

	if manager != nil {
		manager.Stop()
	}

	var firstErr error
	if httpServer != nil {
		if err := httpServer.Stop(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// Drain the current registry's SSE senders BEFORE stopping the transport
	// server. transportServer.Stop calls server.Shutdown, which blocks on the
	// long-lived SSE handlers until they release (their request contexts are
	// NOT cancelled by Shutdown), stalling the full ctx budget. Factory.Close
	// unblocks every SSE handler (idempotent via SSESender.Close's sync.Once),
	// so the subsequent Shutdown completes promptly.
	//
	// Load registryRef HERE, after httpServer.Stop has drained in-flight admin
	// handlers — NOT in the snapshot block above. An admin config-commit
	// (applyCommittedConfig) races Stop: it locks a.mu independently of
	// started, so it can installPlan a NEW registry after Stop set
	// started=false. Snapshotting the registry before httpServer.Stop would
	// drain the OLD one while the transport still serves the NEW registry's SSE
	// handlers, re-stalling Shutdown. httpServer is now closed, so no further
	// apply can install a registry past this point; this Load is final.
	currentRegistry := a.registryRef.Load()
	if currentRegistry != nil && currentRegistry.http != nil {
		if err := currentRegistry.http.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if transportServer != nil {
		if err := transportServer.Stop(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if currentRuntime != nil {
		// Derive the drain from the shutdown ctx: one budget for the whole
		// SIGTERM path, not shutdown_timeout + drain_timeout stacked.
		if err := stopRuntime(ctx, currentRuntime, currentApplied); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// Close the metrics exporter last so late runtime-shutdown metrics are
	// flushed. Close stops the flush goroutine and performs a final flush;
	// it is idempotent (safe if an injected exporter is also closed elsewhere).
	if metricsExporter != nil {
		if err := metricsExporter.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

func (a *App) Run(ctx context.Context) error {
	if err := a.Start(ctx); err != nil {
		return err
	}

	// Terminal backstop: the transport/admin/monitor servers keep serving
	// (and /health keeps reporting) even when the runtime is unrecoverably
	// dead, so waiting only on ctx.Done() would leave an ECS/Kubernetes task
	// "running" forever while bridging nothing. Exit non-zero on terminal so
	// the orchestrator restarts the task.
	var runErr error
	select {
	case <-ctx.Done():
	case <-a.terminalCh:
		runErr = ErrRuntimeTerminal
		a.logger.Error("bootstrap: runtime entered terminal (unrecoverable) state; " +
			"exiting non-zero so the orchestrator restarts the task")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.shutdownTimeout)
	defer cancel()
	if err := a.Stop(shutdownCtx); err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}

func (a *App) watchTerminal(ctx context.Context) {
	ticker := a.clk.NewTicker(a.terminalPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			if a.runtimeTerminal() {
				select {
				case a.terminalCh <- struct{}{}:
				default:
				}
				return
			}
		}
	}
}

// runtimeTerminal reports whether the active runtime is in an unrecoverable
// terminal state. The App swaps runtimes directly, so a nil runtime during a
// swap window is transient (not terminal) — only a live-but-terminal runtime,
// or a WEDGED App (swap + recovery both failed, leaving no runtime), counts.
// terminalProbe is a test seam; production leaves it nil.
func (a *App) runtimeTerminal() bool {
	if a.terminalProbe != nil {
		return a.terminalProbe()
	}
	if a.wedged.Load() {
		return true
	}
	rt := a.runtimeRef.Get()
	return rt != nil && rt.Terminal()
}

// enterWedgedState records that a prepare/commit swap AND its recovery both
// failed, leaving the App with no active runtime and no self-recovery path. It
// tears the request-facing surface down to a clean "nothing running" state and
// latches the wedged flag so runtimeTerminal reports terminal (watchTerminal
// takes the process down) and the monitor /live probe fails closed via the
// RuntimeProvider sentinel. The flag is cleared by installPlan if a later
// reload succeeds.
func (a *App) enterWedgedState() {
	a.wedged.Store(true)
	a.runtimeRef.Set(nil)
	a.appliedRef.Set(nil)
	a.handlerRef.Set(http.NotFoundHandler())
}
