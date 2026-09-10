package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	parser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/ports"
)

// WithInitialConfig supplies optional logical YAML/JSON, never an overlay.
func WithInitialConfig(contents string) Option {
	return func(a *App) { a.initialContents = contents }
}

// WithInitialConfigSource supplies a startup-only source through the loader port.
func WithInitialConfigSource(source ports.Loader) Option {
	return func(a *App) { a.initialSource = source }
}

func (a *App) decodeInitialConfig(r io.Reader) (*ports.BridgeConfig, error) {
	return parser.Parse(r, parser.FormatYAML, a.pluginRegistry)
}

func (a *App) admitInitialConfig(ctx context.Context, cfg *ports.BridgeConfig) error {
	if err := config.Validate(cfg); err != nil {
		return err
	}
	if err := a.admitDeploymentProfile(ctx, cfg, "initialize"); err != nil {
		return err
	}
	if err := checkIgnoredHTTPBlock(a.logger, cfg); err != nil {
		return err
	}
	copy, err := cloneBridgeConfig(cfg, a.pluginRegistry)
	if err != nil {
		return err
	}
	if err := applyMQTTMemoryProfile(copy, a.cfg); err != nil {
		return err
	}
	return a.newFactoryRegistry(copy).builder.Preflight(ctx)
}

type repositoryEpochKey struct{}

type repositoryEvent struct {
	observation ports.ConfigObservation
	epoch       uint64
}

// One observation owner and one serialized apply worker. Backend calls never
// hold the health/state lock. Missing fences even while construction is busy.
func (a *App) observeRepository(ctx context.Context, target ports.ConfigStore) {
	if err := a.ensureConfigRepository(ctx, target); err != nil {
		a.repositoryError(err)
	}
	observations, err := a.manager.Observe(ctx)
	if err != nil {
		a.repositoryError(err)
		return
	}
	work := newRepositoryInbox()
	a.watchWg.Go(func() {
		defer work.close()
		for {
			select {
			case <-ctx.Done():
				return
			case observation, ok := <-observations:
				if !ok {
					return
				}
				if observation.Kind == ports.ConfigMissing {
					a.withdrawConfiguration()
				}

				work.put(repositoryEvent{observation, a.observationEpoch.Load()})
			}
		}
	})
	if a.initialSource == nil && a.initialContents != "" {
		a.initialSource = parser.NewInlineSource(a.initialContents, a.pluginRegistry)
	}
	ticker := a.clk.NewTicker(a.cfg.EffectivePollInterval())
	defer ticker.Stop()
	var retry *repositoryEvent
	initializeAllowed := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			if initializeAllowed && a.missing.Load() && !a.activated.Load() && a.cfg.NodeRole == "control" && a.initialSource != nil {
				attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
				err := config.Initialize(attempt, target, a.initialSource, a.admitInitialConfig)
				cancel()
				if err != nil {
					a.repositoryError(err)
				}
			}
			if retry != nil && !a.missing.Load() && !a.wedged.Load() && a.runtimeRef.Get() == nil {
				a.mu.Lock()
				if retry.epoch == a.observationEpoch.Load() {
					attempt, cancel := context.WithTimeout(context.WithValue(ctx, repositoryEpochKey{}, retry.epoch), 30*time.Second)
					err := a.activateRepository(attempt, retry.observation.Config)
					cancel()
					if err == nil {
						retry = nil
					} else {
						a.repositoryError(err)
					}
				}
				a.mu.Unlock()
			}

		case <-work.changed:
			ev, ok, closed := work.take()
			if !ok {
				if closed {
					a.repositoryError(errors.New("configuration observation ended"))
					return
				}
				continue
			}
			switch ev.observation.Kind {
			case ports.ConfigReadError:
				initializeAllowed = false
				retry = nil
				a.repositoryError(ev.observation.Err)
			case ports.ConfigMissing:
				initializeAllowed = true
				retry = nil
				if !a.idleRepository(ctx) {
					return
				}
			case ports.ConfigPresent:
				initializeAllowed = false
				a.mu.Lock()
				if ctx.Err() == nil && !a.wedged.Load() && ev.epoch == a.observationEpoch.Load() {
					a.missing.Store(false)
					attempt, cancel := context.WithTimeout(context.WithValue(ctx, repositoryEpochKey{}, ev.epoch), 30*time.Second)
					err := a.activateRepository(attempt, ev.observation.Config)
					cancel()
					if err != nil {
						retry = &ev
						a.repositoryError(err)
					} else {
						retry = nil
					}
				}
				a.mu.Unlock()
			}
		}
	}
}

type repositoryDiagnostic struct {
	reason         string
	startupPending bool
}

func (a *App) repositoryError(err error) {
	if err == nil {
		a.observationError.Store(nil)
		return
	}
	reason := "configuration repository or activation failed"
	a.observationError.Store(&repositoryDiagnostic{reason: reason, startupPending: httpapi.ConfigStartupPending(err)})
	a.logger.Warn(reason)
}

// Caller holds mu. Absence is never resolved from a rollout artifact.
func (a *App) activateRepository(ctx context.Context, logical *ports.BridgeConfig) error {
	if logical == nil {
		return fmt.Errorf("bootstrap: missing configuration")
	}
	first := a.runtimeRef.Get() == nil
	if first {
		a.activating.Store(true)
		defer a.activating.Store(false)
	}
	if !first && a.isStaleSourceConfig(logical) {
		return nil
	}
	boot := logical
	if first {
		if err := bridge.ValidateDormantReactivation(a.historical, logical); err != nil {
			return err
		}
		if bridge.IsCoordinatedRollout(logical) {
			if err := a.buildRolloutDriver(ctx); err != nil {
				return err
			}
			if a.rolloutDriver == nil {
				return fmt.Errorf("bootstrap: coordinated rollout is not wired")
			}
			var err error
			boot, err = a.rolloutDriver.ResolveBoot(ctx, logical)
			if err != nil {
				return err
			}
		}
	}
	if err := checkIgnoredHTTPBlock(a.logger, boot); err != nil {
		return err
	}
	a.logicalRef.Set(logical)
	if _, err := a.applyLogicalIfChanged(ctx, boot, true); err != nil {
		if errors.Is(err, ports.ErrApplyInFlight) {
			a.repositoryError(nil)
			return nil
		}
		a.manager.NotifyApplyResult(logical, err)
		return err
	}
	if a.missing.Load() {
		return fmt.Errorf("configuration removed during activation")
	}
	if first {
		if err := a.seedRolloutBaseline(ctx, boot); err != nil {
			a.failDormancy()
			return err
		}
		if !a.shutdownTimeoutPinned {
			a.shutdownTimeout = boot.Bridge.ShutdownTimeoutDuration()
			a.processBudget.Store(int64(a.shutdownTimeout))
		}
		if a.rolloutDriver != nil {
			a.stopRolloutDrive = a.rolloutDriver.Start(a.rootCtx, a.clk, a.metricsExporter)
		}
	}
	a.activated.Store(true)
	a.reconcileBootApplyResult(logical, boot)
	a.repositoryError(nil)
	return nil
}

func (a *App) idleRepository(ctx context.Context) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	rt, previous := a.runtimeRef.Get(), a.appliedRef.Get()
	// The existing coordinated protocol cannot safely suspend a cohort. Exit
	// instead of allowing cached commits to resurrect this member. Run owns
	// bounded drive/runtime shutdown; no recovery to previous is attempted.
	if a.rolloutDriver != nil || bridge.IsClusteredDeployment(previous) {
		a.failDormancy()
		return false
	}
	if rt != nil {
		budget := 30 * time.Second
		if previous != nil {
			budget = previous.Bridge.DrainTimeoutDuration()
		}
		stopCtx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		stopped := make(chan error, 1)
		go func() { stopped <- stopRuntime(stopCtx, rt, previous) }()
		var stopErr error
		select {
		case stopErr = <-stopped:
		case <-stopCtx.Done():
			stopErr = stopCtx.Err()
		}
		if stopErr != nil || rt.Terminal() {
			a.failDormancy()
			return false
		}
		a.closeSupersededHTTP(ctx, a.registryRef.Load())
		a.historical = previous
	}
	a.convergenceMu.Lock()
	if a.convergenceWatchCancel != nil {
		a.convergenceWatchCancel()
	}
	a.convergenceMu.Unlock()
	a.runtimeRef.Set(nil)
	a.appliedRef.Set(nil)
	a.logicalRef.Set(nil)
	a.registryRef.Store(nil)
	a.lastAppliedFingerprint = ""
	a.handlerRef.Set(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	a.manager.NotifyIdle()
	a.repositoryError(nil)
	return true
}

func (a *App) failDormancy() {
	a.wedged.Store(true)
	select {
	case a.terminalCh <- struct{}{}:
	default:
	}
	if rt := a.runtimeRef.Get(); rt != nil {
		rt.Fence()
	}
}

// Provisioning is a dev-only action behind the already-started control plane.
func (a *App) ensureConfigRepository(ctx context.Context, target ports.ConfigStore) error {
	if !a.cfg.DevMode {
		return nil
	}
	if provisioner, ok := target.(interface{ EnsureTable(context.Context) error }); ok {
		return provisioner.EnsureTable(ctx)
	}
	return nil
}
