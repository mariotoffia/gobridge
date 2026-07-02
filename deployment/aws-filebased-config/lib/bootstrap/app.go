package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

type Option func(*App)

func WithLogger(logger *slog.Logger) Option {
	return func(a *App) { a.logger = logger }
}

func WithParameterResolver(resolver parameterResolver) Option {
	return func(a *App) { a.parameterResolver = resolver }
}

func WithCredentialStore(store ports.CredentialStore) Option {
	return func(a *App) { a.credentialStore = store }
}

// WithDynamoDBClient overrides the *dynamodb.Client used by the
// DynamoDB store factory. When unset (the default) the App builds a
// client from the ambient AWS environment during Start via
// newDynamoDBClient. Tests and local emulation (e.g. LocalStack)
// inject a pre-configured client here.
func WithDynamoDBClient(client *dynamodb.Client) Option {
	return func(a *App) { a.dynamoDBClient = client }
}

// WithPluginRegistry overrides the *ports.Registry used to decode
// blueprints loaded from the file source / re-parsed during secret
// resolution. When unset (the default) the App constructs a fresh
// registry and populates it with the adapters this binary bundles
// (paho, sqs, native + DynamoDB store, http transport). Tests use this
// option to inject hermetic stubs.
func WithPluginRegistry(reg *ports.Registry) Option {
	return func(a *App) { a.pluginRegistry = reg }
}

// WithShutdownTimeout sets the graceful shutdown deadline. Defaults to 30s.
func WithShutdownTimeout(d time.Duration) Option {
	return func(a *App) { a.shutdownTimeout = d }
}

type App struct {
	cfg deployinfra.BootstrapConfig

	logger            *slog.Logger
	parameterResolver parameterResolver
	credentialStore   ports.CredentialStore
	pluginRegistry    *ports.Registry
	dynamoDBClient    *dynamodb.Client

	manager         *config.Manager
	httpServer      *httpapi.Server
	transportServer *transportServer

	logicalRef bridgeConfigRef
	appliedRef bridgeConfigRef
	runtimeRef runtimeRef
	apiKeysRef apiKeysRef
	handlerRef *transportHandlerRef

	watchCancel context.CancelFunc
	watchWg     sync.WaitGroup

	shutdownTimeout time.Duration

	// mu protects started, watchCancel, and serializes config reloads.
	mu      sync.Mutex
	started bool
}

func NewApp(cfg deployinfra.BootstrapConfig, opts ...Option) *App {
	cfg = cfg.Normalized()
	app := &App{
		cfg:        cfg,
		logger:     slog.Default(),
		handlerRef: newTransportHandlerRef(),
	}
	for _, opt := range opts {
		opt(app)
	}
	if app.shutdownTimeout <= 0 {
		app.shutdownTimeout = 30 * time.Second
	}
	if app.pluginRegistry == nil {
		app.pluginRegistry = newDefaultPluginRegistry()
	}
	return app
}

func (a *App) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started {
		return fmt.Errorf("bootstrap: app already started")
	}

	if err := a.cfg.Validate(); err != nil {
		return err
	}

	if a.parameterResolver == nil {
		resolver, err := newSSMParameterResolver(ctx, a.cfg)
		if err != nil {
			return err
		}
		a.parameterResolver = resolver
	}
	if a.credentialStore == nil {
		store, err := newDefaultCredentialStore(ctx, a.cfg)
		if err != nil {
			return err
		}
		a.credentialStore = store
	}
	if a.dynamoDBClient == nil {
		client, err := newDynamoDBClient(ctx, a.cfg)
		if err != nil {
			return err
		}
		a.dynamoDBClient = client
	}

	source := newOptionalFileSource(a.cfg.ConfigFilePath, a.pluginRegistry, func() *ports.BridgeConfig {
		return defaultLogicalConfig(a.cfg)
	})
	watcher := newPollWatcher(a.cfg, a.pluginRegistry, a.logger)
	a.manager = config.NewManager(
		config.Layer{Name: "file", Loader: source, Watcher: watcher},
		config.WithManagerLogger(a.logger),
	)

	logicalCfg, err := a.manager.Load(ctx)
	if err != nil {
		return err
	}
	a.logicalRef.Set(logicalCfg)

	if err := a.applyLogicalConfig(ctx, logicalCfg); err != nil {
		return err
	}

	// NodeRole is intentionally NOT consulted here: every node starts
	// the transport, admin, and monitor servers. node_role is reserved
	// / non-operative at runtime today (see infra.BootstrapConfig.NodeRole).
	a.transportServer = newTransportServer(a.handlerRef, a.logger)
	if err := a.transportServer.Start(a.cfg.TransportHTTPAddr); err != nil {
		return fmt.Errorf("bootstrap: start transport HTTP server: %w", err)
	}

	apiCfg := httpapi.Config{
		AdminAddr:             a.cfg.AdminAddr,
		MonitorAddr:           a.cfg.MonitorAddr,
		CORSOrigins:           a.cfg.CORSOrigins,
		AdminAPIKeyProvider:   a.apiKeysRef.AdminKey,
		MonitorAPIKeyProvider: a.apiKeysRef.MonitorKey,
		RuntimeProvider: func() ports.Runtime {
			rt := a.runtimeRef.Get()
			if rt == nil {
				return nil
			}
			return rt
		},
		ConfigStore: &cfgparser.FileStore{Path: a.cfg.ConfigFilePath, Registry: a.pluginRegistry},
		// ConfigProvider must expose the *effective* (currently running)
		// config, so read from appliedRef -- the config of the last
		// successfully-applied runtime. logicalRef holds the last config
		// read from disk, which may be a reload that FAILED validation or
		// apply (watchLoop keeps the last-good runtime on rejection); using
		// it here would surface a rejected config to operators as if it were
		// live. appliedRef is nil only when nothing is cleanly running, and
		// every configProvider consumer handles nil (GET /config -> 503).
		ConfigProvider: a.appliedRef.Get,
	}
	a.httpServer = httpapi.New(nil, apiCfg,
		httpapi.WithServerLogger(a.logger),
		httpapi.WithAuditLogger(httpapi.NewSlogAuditLogger(a.logger)),
	)
	if err := a.httpServer.Start(ctx); err != nil {
		_ = a.transportServer.Stop(context.Background())
		return fmt.Errorf("bootstrap: start admin/monitor HTTP server: %w", err)
	}

	watchCh, err := a.manager.Watch(ctx)
	if err != nil {
		a.manager.Stop()
		_ = a.httpServer.Stop(context.Background())
		_ = a.transportServer.Stop(context.Background())
		return fmt.Errorf("bootstrap: start config watcher: %w", err)
	}

	watchCtx, watchCancel := context.WithCancel(ctx)
	a.watchCancel = watchCancel
	a.started = true

	a.watchWg.Go(func() {
		a.watchLoop(watchCtx, watchCh)
	})

	return nil
}

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

	// Wait for watchLoop goroutine to finish before tearing down resources
	// it may still be using (e.g. mid-applyLogicalConfig).
	a.watchWg.Wait()

	manager := a.manager
	httpServer := a.httpServer
	transportServer := a.transportServer
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
	if transportServer != nil {
		if err := transportServer.Stop(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if currentRuntime != nil {
		if err := stopRuntime(currentRuntime, currentApplied); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

func (a *App) Run(ctx context.Context) error {
	if err := a.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.shutdownTimeout)
	defer cancel()
	return a.Stop(shutdownCtx)
}

func (a *App) AdminURL() string {
	if a.httpServer == nil {
		return ""
	}
	return a.httpServer.AdminURL()
}

func (a *App) MonitorURL() string {
	if a.httpServer == nil {
		return ""
	}
	return a.httpServer.MonitorURL()
}

func (a *App) TransportURL() string {
	if a.transportServer == nil {
		return ""
	}
	return a.transportServer.URL()
}

func (a *App) CurrentLogicalConfig() *ports.BridgeConfig {
	return a.logicalRef.Get()
}

func (a *App) CurrentAppliedConfig() *ports.BridgeConfig {
	return a.appliedRef.Get()
}

func (a *App) CurrentRuntime() *goruntime.Runtime {
	return a.runtimeRef.Get()
}

func (a *App) watchLoop(ctx context.Context, watchCh <-chan *ports.BridgeConfig) {
	for {
		select {
		case <-ctx.Done():
			return
		case logicalCfg, ok := <-watchCh:
			if !ok {
				return
			}
			a.logicalRef.Set(logicalCfg)

			// Serialize config reloads to prevent concurrent
			// applyLogicalConfig calls from racing on runtime swap.
			a.mu.Lock()
			err := a.applyLogicalConfig(ctx, logicalCfg)
			a.mu.Unlock()
			if err != nil {
				a.logger.Warn("bootstrap: config reload rejected; keeping last good runtime", "error", err)
			}
		}
	}
}

type swapMode int

const (
	swapModeOverlap swapMode = iota
	swapModePrepareCommit
)

type runtimePlan struct {
	logical  *ports.BridgeConfig
	resolved *ports.BridgeConfig
	inputs   *resolvedInputs
	mode     swapMode

	registry *factoryRegistry
	plan     *bridge.BuildPlan
	runtime  *goruntime.Runtime
}

func (a *App) applyLogicalConfig(ctx context.Context, logical *ports.BridgeConfig) error {
	if err := validateFilesystemProfile(a.cfg, logical); err != nil {
		return err
	}

	plan, err := a.prepareRuntimePlan(ctx, logical)
	if err != nil {
		return err
	}

	oldRuntime := a.runtimeRef.Get()
	oldApplied := a.appliedRef.Get()

	switch plan.mode {
	case swapModePrepareCommit:
		return a.applyPrepareCommit(ctx, plan, oldRuntime, oldApplied)
	default:
		return a.applyOverlap(ctx, plan, oldRuntime, oldApplied)
	}
}

func (a *App) prepareRuntimePlan(ctx context.Context, logical *ports.BridgeConfig) (*runtimePlan, error) {
	inputs, err := resolveInputs(ctx, a.parameterResolver, a.cfg, a.pluginRegistry, logical)
	if err != nil {
		return nil, err
	}

	registry := a.newFactoryRegistry(inputs.RuntimeConfig)
	mode := registry.detectSwapMode(inputs.RuntimeConfig)

	plan := &runtimePlan{
		logical:  logical,
		resolved: inputs.RuntimeConfig,
		inputs:   inputs,
		mode:     mode,
		registry: registry,
	}

	switch mode {
	case swapModePrepareCommit:
		bp, err := registry.builder.Plan(ctx)
		if err != nil {
			return nil, err
		}
		plan.plan = bp
	default:
		rt, err := registry.builder.Build(ctx)
		if err != nil {
			return nil, err
		}
		plan.runtime = rt
	}

	return plan, nil
}

func (a *App) applyOverlap(
	ctx context.Context,
	plan *runtimePlan,
	oldRuntime *goruntime.Runtime,
	oldApplied *ports.BridgeConfig,
) error {
	if err := plan.runtime.Start(ctx); err != nil {
		return fmt.Errorf("bootstrap: start runtime: %w", err)
	}

	// If anything below panics, ensure the started runtime is cleaned up.
	installed := false
	defer func() {
		if !installed {
			_ = stopRuntime(plan.runtime, plan.logical)
		}
	}()

	a.installPlan(plan)
	installed = true

	if oldRuntime != nil {
		if err := stopRuntime(oldRuntime, oldApplied); err != nil {
			a.logger.Warn("bootstrap: stop old runtime after overlap swap", "error", err)
		}
	}
	return nil
}

func (a *App) applyPrepareCommit(
	ctx context.Context,
	plan *runtimePlan,
	oldRuntime *goruntime.Runtime,
	oldApplied *ports.BridgeConfig,
) error {
	if oldRuntime != nil {
		if err := stopRuntime(oldRuntime, oldApplied); err != nil {
			a.logger.Warn("bootstrap: stop old runtime before prepare/commit swap", "error", err)
		}
	}
	a.runtimeRef.Set(nil)

	newRuntime, err := plan.plan.Commit(ctx)
	if err != nil {
		a.recoverPrevious(ctx, oldApplied)
		return fmt.Errorf("bootstrap: complete runtime: %w", err)
	}
	if err := newRuntime.Start(ctx); err != nil {
		a.recoverPrevious(ctx, oldApplied)
		return fmt.Errorf("bootstrap: start runtime: %w", err)
	}
	plan.runtime = newRuntime
	a.installPlan(plan)
	return nil
}

func (a *App) recoverPrevious(ctx context.Context, logical *ports.BridgeConfig) {
	if logical == nil {
		a.runtimeRef.Set(nil)
		a.appliedRef.Set(nil)
		a.handlerRef.Set(http.NotFoundHandler())
		return
	}

	plan, err := a.prepareRuntimePlan(ctx, logical)
	if err != nil {
		a.logger.Error("bootstrap: failed to rebuild previous runtime after prepare/commit failure", "error", err)
		a.runtimeRef.Set(nil)
		a.appliedRef.Set(nil)
		a.handlerRef.Set(http.NotFoundHandler())
		return
	}

	switch plan.mode {
	case swapModePrepareCommit:
		plan.runtime, err = plan.plan.Commit(ctx)
	default:
		// Overlap mode: plan.runtime was already built by prepareRuntimePlan.
	}
	if err == nil {
		err = plan.runtime.Start(ctx)
	}
	if err != nil {
		a.logger.Error("bootstrap: failed to restart previous runtime after prepare/commit failure", "error", err)
		a.runtimeRef.Set(nil)
		a.appliedRef.Set(nil)
		a.handlerRef.Set(http.NotFoundHandler())
		return
	}

	a.installPlan(plan)
}

func (a *App) installPlan(plan *runtimePlan) {
	a.runtimeRef.Set(plan.runtime)
	a.appliedRef.Set(plan.logical)
	a.apiKeysRef.Set(plan.inputs.AdminAPIKey, plan.inputs.MonitorAPIKey)
	a.handlerRef.Set(plan.registry.transportHandler())
}
