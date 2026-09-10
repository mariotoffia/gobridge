package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	parser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/ports"
	credentials "github.com/mariotoffia/gobridge/runtime/credentials"
)

// A supervisor lifetime ends on repository absence. A later configuration gets
// a fresh supervisor, so old apply callbacks and paused configs cannot resurrect.
type configSession struct {
	initial             *ports.BridgeConfig
	initialAcknowledged bool
	observed            atomic.Pointer[commandObservation]
	sup                 *bridge.Supervisor
	pipeline            *reloadPipeline
	updates             chan *ports.BridgeConfig
	cancel              context.CancelFunc
	done                chan error
	stopAttempted       bool
	stopError           error
}

func (s *configSession) stop(ctx context.Context) (result error) {
	if s.stopAttempted {
		return s.stopError
	}
	defer func() { s.stopAttempted, s.stopError = true, result }()
	if rt := s.sup.Runtime(); rt != nil {
		rt.Fence()
	}
	s.cancel()
	select {
	case err := <-s.done:
		if err != nil {
			return err
		}
		if s.sup.Terminal() {
			return fmt.Errorf("runtime teardown retained terminal work")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func runObservedConfig(parent context.Context, path, credentialsDir string, reg *ports.Registry, control ports.HTTPConfig, embedded string, logger *slog.Logger, clk clock.Clock, hooks ...observedConfigHooks) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	source := fileconfig.NewSource(path, reg)
	watcher := fileconfig.NewWatcher(path, reg, fileconfig.WithClock(clk))
	var store ports.ConfigStore = &parser.FileStore{Path: path, Registry: reg}
	mgr := config.NewManager(config.Layer{Name: "file", Loader: source, Watcher: watcher}, config.WithManagerLogger(logger), config.WithManagerClock(clk))
	var current atomic.Pointer[configSession]
	var activated atomic.Bool
	// 0 = clean, 1 = retryable startup fault, 2 = unclassified/permanent refusal.
	var sourceError atomic.Uint32
	report := func(err error) {
		if err == nil {
			sourceError.Store(0)
		} else if httpapi.ConfigStartupPending(err) {
			sourceError.Store(1)
		} else {
			sourceError.Store(2)
		}
	}
	var absent atomic.Bool
	var generation atomic.Uint64
	var activationMu sync.Mutex
	var activationCancel context.CancelFunc
	var admission atomic.Pointer[bridge.Supervisor]
	var closeMetrics, closeTracer func(context.Context) error
	adminKey, monitorKey := httpAPIKeys(&control, os.LookupEnv)
	admit := func(ctx context.Context, cfg *ports.BridgeConfig) error {
		return admitObservedConfig(ctx, cfg, &control, admission.Load())
	}
	api := httpapi.Config{
		AdminAddr: control.AdminAddr, MonitorAddr: control.MonitorAddr,
		AdminAPIKey: adminKey, MonitorAPIKey: monitorKey, CORSOrigins: control.CORSOrigins,
		TLSCertFile: control.TLSCertFile, TLSKeyFile: control.TLSKeyFile,
		RuntimeProvider: func() ports.Runtime {
			if s := current.Load(); s != nil {
				if rt := s.sup.Runtime(); rt != nil {
					return rt
				}
			}
			return nil
		},
		ConfigProvider: func() *ports.BridgeConfig {
			if s := current.Load(); s != nil {
				return s.sup.Config()
			}
			return nil
		},
		TerminalProvider: func() bool { s := current.Load(); return s != nil && s.sup.Terminal() },
		ConfigStore:      store,
		BridgeController: observedController{current: &current, absent: &absent, generation: &generation, manager: mgr},
		ConfigDecoder:    func(r io.Reader) (*ports.BridgeConfig, error) { return parser.Parse(r, parser.FormatYAML, reg) },
		ConfigAdmitter:   admit,
		ConfigApplier: func(ctx context.Context, cfg *ports.BridgeConfig) error {
			if absent.Load() {
				return ports.ErrApplyInFlight
			}
			if s := current.Load(); s != nil && s.sup.Runtime() != nil {
				return s.pipeline.applyCommitted(ctx, cfg)
			}
			return ports.ErrApplyInFlight
		},
		ConfigWatchProvider: func() httpapi.ConfigWatchHealth {
			return observedConfigHealth(current.Load(), mgr, &control, activated.Load(), sourceError.Load())
		},
	}
	srv := httpapi.New(nil, api, httpapi.WithServerLogger(logger))
	if err := srv.Start(ctx); err != nil {
		return err
	}
	logger.Info("control plane started", "admin", srv.AdminURL(), "monitor", srv.MonitorURL())
	defer func() {
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		if s := current.Load(); s != nil {
			_ = s.stop(stopCtx)
		}
		_ = srv.Stop(stopCtx)
		if closeTracer != nil {
			_ = closeTracer(stopCtx)
		}
		if closeMetrics != nil {
			_ = closeMetrics(stopCtx)
		}
	}()
	observations := observeCommandConfig(ctx, source, watcher, mgr, func() {
		absent.Store(true)
		generation.Add(1)
		activationMu.Lock()
		if activationCancel != nil {
			activationCancel()
		}
		activationMu.Unlock()
		if session := current.Load(); session != nil {
			if rt := session.sup.Runtime(); rt != nil {
				activated.Store(true)
				rt.Fence()
			}
			session.cancel()
		}
	}, generation.Load, report)
	metrics, closeMetrics, err := newMetricsExporter(ctx, logger)
	if err != nil {
		return err
	}

	tracer, closeTracer, err := newTracer(ctx, logger)
	if err != nil {
		return err
	}
	validator := bridge.NewSupervisor(bridge.WithSupervisorBlueprintValidator(config.Validate))
	if err := wireAllFactories(ctx, validator, logger, metrics); err != nil {
		return err
	}
	admission.Store(validator)

	resolver := newDefaultCredentialResolver(credentialsDir, logger)
	start := func(cfg *ports.BridgeConfig, epoch uint64) (*configSession, error) {
		runCtx, runCancel := context.WithCancel(ctx)
		activationMu.Lock()
		activationCancel = runCancel
		if generation.Load() != epoch || absent.Load() {
			runCancel()
		}
		activationMu.Unlock()
		pipeline := newReloadPipeline(reg, logger, withApplyResultNotifier(mgr))
		interval := credentials.DefaultCredentialPollInterval
		sup := bridge.NewSupervisor(bridge.WithSupervisorLogger(logger), bridge.WithOnSwap(pipeline.onSwap),
			bridge.WithSupervisorBlueprintValidator(config.Validate), bridge.WithReconfigStrategy(bridge.NewDirectStrategy()),
			bridge.WithSupervisorPolledCredentialStore(resolver, ports.PollBasedWrapperConfig{PollInterval: interval, Jitter: interval / 10, EmitOnStart: true}),
			bridge.WithSupervisorMetrics(metrics), bridge.WithSupervisorTracer(tracer))
		if err := wireAllFactories(runCtx, sup, logger, metrics); err != nil {
			runCancel()
			return nil, err
		}
		session := &configSession{initial: cfg, sup: sup, pipeline: pipeline, updates: make(chan *ports.BridgeConfig, 1), cancel: runCancel, done: make(chan error, 1)}
		go pipeline.run(runCtx, bridge.NewWindowedStrategy(10*time.Second, 30*time.Second, clk).Filter(runCtx, session.updates))
		go func() { session.done <- sup.Run(runCtx, cfg, pipeline.changes()) }()
		return session, nil
	}
	var initial ports.Loader = parser.NewInlineSource(embedded, reg)
	initialize := func(ctx context.Context) error { return config.Initialize(ctx, store, initial, admit) }
	if len(hooks) > 0 {
		if hooks[0].initialize != nil {
			initialize = hooks[0].initialize
		}
		if hooks[0].start != nil {
			start = hooks[0].start
		}
	}
	var initializer *initializationWorker
	var initialized <-chan error
	if embedded != "" {
		initializer = newInitializationWorker(ctx, initialize)
		initialized = initializer.results
		defer initializer.stop()
	}
	ticker := clk.NewTicker(time.Second)
	defer ticker.Stop()
	var historical, desired *ports.BridgeConfig
	var desiredEpoch uint64
	missing, retryPending, initializeAllowed := false, false, false
	for {
		var done <-chan error
		if s := current.Load(); s != nil {
			done = s.done
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			session := current.Load()
			session.cancel()
			if absent.Load() {
				historical = session.sup.Config()
				current.Store(nil)
				mgr.NotifyIdle()
				missing, desired = true, nil
				if err != nil && !errors.Is(err, context.Canceled) {
					return fmt.Errorf("cannot prove runtime stopped: %w", err)
				}
				if session.sup.Terminal() || bridge.IsClusteredDeployment(historical) {
					return fmt.Errorf("configuration withdrawal requires process replacement")
				}
				continue
			}
			current.Store(nil)
			if activated.Load() {
				return fmt.Errorf("runtime stopped unexpectedly: %v", err)
			}
			if err == nil {
				err = fmt.Errorf("supervisor exited before initial activation")
			}
			mgr.NotifyApplyResult(session.initial, err)
			report(err)
			retryPending = true

		case err := <-initialized:
			initializer.complete()
			if err != nil && initializeAllowed && !activated.Load() {
				report(err)
			}

		case ev, ok := <-observations:
			if !ok {
				sourceError.Store(2)
				observations = nil
				continue
			}
			switch ev.Kind {
			case ports.ConfigReadError:
				initializeAllowed = false
				report(ev.Err)
				if current.Load() == nil {
					desired = nil
				}
			case ports.ConfigMissing:
				initializeAllowed = true
				report(nil)
				missing, desired = true, nil
				if s := current.Load(); s != nil {
					historical = s.sup.Config()
					stopCtx, stop := context.WithTimeout(ctx, 30*time.Second)
					err := s.stop(stopCtx)
					stop()
					if err != nil {
						return fmt.Errorf("cannot prove runtime stopped: %w", err)
					}
					current.Store(nil)
					if bridge.IsClusteredDeployment(historical) {
						return fmt.Errorf("coordinated configuration removed; process replacement required")
					}
				}
				mgr.NotifyIdle()
			case ports.ConfigPresent:
				initializeAllowed = false
				if initializer != nil {
					initializer.supersede()
				}
				if ev.generation != generation.Load() {
					continue
				}
				desiredEpoch = ev.generation
				absent.Store(false)
				missing, desired = false, ev.Config
				retryPending = false
				sourceError.Store(0)
				if err := admit(ctx, desired); err != nil {
					sourceError.Store(2)
					desired = nil
					continue
				}
				if s := current.Load(); s != nil {
					s.recordObservation(ev.Config, ev.generation)
					select {
					case <-s.updates:
					default:
					}
					select {
					case s.updates <- desired:
					case <-ctx.Done():
						return nil
					}
				}
			}
		case <-ticker.C():
			retryPending = false
			if initializeAllowed && missing && !activated.Load() && embedded != "" {
				initializer.start()
			}
			if s := current.Load(); s != nil {
				if s.sup.Terminal() {
					return fmt.Errorf("runtime entered terminal state")
				}
				if rt := s.sup.Runtime(); rt != nil {
					activated.Store(true)
					s.acknowledgeInitial(mgr)
				}
			}
		}
		if activated.Load() && initializer != nil {
			initializer.stop()
			initialized = nil
		}
		if current.Load() == nil && desired != nil && !missing && !retryPending {
			if err := bridge.ValidateDormantReactivation(historical, desired); err != nil {
				sourceError.Store(2)
				desired = nil
				continue
			}
			session, err := start(desired, desiredEpoch)
			if err != nil {
				mgr.NotifyApplyResult(desired, err)
				report(err)
				retryPending = true
				continue
			}
			session.recordObservation(desired, desiredEpoch)
			current.Store(session)
		}
	}
}

// Admin pause/resume addresses only the current authorized supervisor lifetime.
type observedController struct {
	current    *atomic.Pointer[configSession]
	absent     *atomic.Bool
	generation *atomic.Uint64
	manager    *config.Manager
}

func (c observedController) StartBridge(ctx context.Context) error {
	return c.resume(ctx)
}

func (c observedController) StopBridge(ctx context.Context) error {
	if s := c.current.Load(); s != nil {
		return s.sup.StopBridge(ctx)
	}
	return nil
}
