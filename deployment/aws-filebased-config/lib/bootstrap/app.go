package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

type Option func(*App)

// ErrRuntimeTerminal is returned by App.Run when the active runtime enters a
// terminal (unrecoverable) state — a background component failed fatally or
// panicked and the runtime cancelled itself. Run exits with this error so the
// process terminates non-zero and the orchestrator (ECS/Kubernetes) restarts
// the task, rather than leaving a "running" container that bridges nothing.
var ErrRuntimeTerminal = errors.New("bootstrap: runtime entered terminal state")

// defaultTerminalPollInterval is how often the terminal backstop samples the
// active runtime. Terminal state only follows a sustained failure, so a coarse
// poll is cheap and sufficient.
const defaultTerminalPollInterval = 5 * time.Second

func WithLogger(logger *slog.Logger) Option {
	return func(a *App) { a.logger = logger }
}

// WithLogLevelVar wires a *slog.LevelVar so bridge.log_level from the reloaded
// bridge config takes effect at runtime (including hot reload) without a
// redeploy. The same LevelVar must back the *slog.Logger handler passed via
// WithLogger. When unset, bridge.log_level is parsed and merged but has no
// effect on emitted log output.
func WithLogLevelVar(v *slog.LevelVar) Option {
	return func(a *App) { a.logLevelVar = v }
}

// WithTerminalPollInterval overrides the terminal-backstop poll interval.
// Primarily for tests; production uses defaultTerminalPollInterval.
func WithTerminalPollInterval(d time.Duration) Option {
	return func(a *App) { a.terminalPollInterval = d }
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

// WithMetricsExporter overrides the runtime metrics exporter. When unset
// (the default) the App builds one during Start from
// BootstrapConfig.MetricsExporter (noop when empty). Tests and local
// emulation inject a pre-configured or fake exporter here. The App takes
// ownership: it Closes the exporter on Stop.
func WithMetricsExporter(exporter ports.MetricsExporter) Option {
	return func(a *App) { a.metricsExporter = exporter }
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
	logLevelVar       *slog.LevelVar
	parameterResolver parameterResolver
	credentialStore   ports.CredentialStore
	pluginRegistry    *ports.Registry
	dynamoDBClient    *dynamodb.Client

	// metricsExporter is the runtime metrics backend passed to every
	// bridge.Builder via newFactoryRegistry. nil selects noop (no metrics).
	// Built once in Start from cfg.MetricsExporter (or injected via
	// WithMetricsExporter) and Closed in Stop — the App owns its lifecycle.
	metricsExporter ports.MetricsExporter

	manager         *config.Manager
	httpServer      *httpapi.Server
	transportServer *transportServer

	logicalRef bridgeConfigRef
	appliedRef bridgeConfigRef
	runtimeRef runtimeRef
	apiKeysRef apiKeysRef
	handlerRef *transportHandlerRef

	// registryRef retains the factory registry backing the
	// currently-installed runtime so its HTTP transport's SSE senders can be
	// drained when the registry is superseded (a config swap) or when the App
	// shuts down. Draining unblocks the long-lived SSE handlers so a fronting
	// transport server.Shutdown does not hang, and disconnects clients pinned
	// to the superseded mux so they reconnect to the newly installed one.
	// Stored by installPlan alongside the other refs; an atomic.Pointer
	// because Stop reads it after releasing a.mu (like the other refs).
	registryRef atomic.Pointer[factoryRegistry]

	watchCancel context.CancelFunc
	watchWg     sync.WaitGroup

	shutdownTimeout      time.Duration
	terminalPollInterval time.Duration

	// clk drives the terminal-backstop poll ticker. Defaults to
	// clock.System; tests keep real time (poll interval is injectable).
	clk clock.Clock

	// terminalCh is signalled once by the terminal backstop when the active
	// runtime goes terminal. Buffered so the backstop never blocks.
	terminalCh chan struct{}

	// terminalProbe is a test seam overriding the terminal check; nil in
	// production (runtimeTerminal reads the active runtime).
	terminalProbe func() bool

	// wedged is set once when a prepare/commit swap AND its recoverPrevious
	// both fail, leaving the App with no active runtime (runtimeRef == nil)
	// and no path back on its own. Unlike a transient nil during a swap
	// window, a wedged App bridges nothing and cannot self-heal, so it must
	// be restarted by the orchestrator. runtimeTerminal reports true while
	// wedged (so watchTerminal fires and Run exits non-zero) and the monitor
	// /live probe fails closed via the RuntimeProvider sentinel. This mirrors
	// cmd/gobridge polling sup.Terminal(), which likewise covers the wedged
	// nil-runtime case the concrete *runtime.Runtime check alone misses.
	wedged atomic.Bool

	// rootCtx is the App-lifetime context (== watchCtx) set in Start and cancelled
	// by Stop. The post-swap convergence watch (RECONFIG-1) derives from it so it
	// lives across reloads and stops with the App. nil before Start completes.
	rootCtx context.Context

	// Post-swap convergence state (RECONFIG-1). A committed swap only proves the
	// new runtime BUILT and Start returned; MQTT dials/reconciles in the
	// background, so a valid-but-broker-rejected config can be acknowledged as
	// applied while the transport never reaches broker truth. The convergence watch
	// polls the installed runtime's readiness and, if it does not reach
	// LevelSubscribed within the activation budget, latches an applied-but-not-
	// converged degraded state (surfaced in deep health + MetricConfigDegraded).
	convergenceMu          sync.Mutex
	convergenceDegraded    bool
	convergenceReason      string
	convergenceRt          *goruntime.Runtime
	convergenceWatchCancel context.CancelFunc

	// mu protects started, watchCancel, and serializes config reloads.
	mu      sync.Mutex
	started bool

	// lastAppliedFingerprint is the canonical content hash (see
	// configFingerprint) of the last config that applyLogicalConfig applied
	// successfully. It makes reloads idempotent: the poll watcher re-emits a
	// config after every on-disk change — including the admin-commit write
	// that applyCommittedConfig already applied in-band — and a blind
	// re-apply would trigger a SECOND full stop→rebuild→start swap (and, in
	// prepare/commit mode, a second exposure to the swap-failure→wedge path)
	// seconds after the first, on every commit. Guarded by mu (every apply
	// path holds mu), so no atomic is needed.
	lastAppliedFingerprint string

	// onRuntimeInstalled and onReloadSkipped are test seams (nil in
	// production). onRuntimeInstalled fires on every successful runtime
	// install (swap); onReloadSkipped fires when applyLogicalIfChanged
	// recognises an already-applied config and skips the rebuild. Tests use
	// them to assert exactly one rebuild per admin commit.
	onRuntimeInstalled func()
	onReloadSkipped    func()
}

func NewApp(cfg deployinfra.BootstrapConfig, opts ...Option) *App {
	cfg = cfg.Normalized()
	app := &App{
		cfg:        cfg,
		logger:     slog.Default(),
		handlerRef: newTransportHandlerRef(),
		terminalCh: make(chan struct{}, 1),
		clk:        clock.System,
	}
	for _, opt := range opts {
		opt(app)
	}
	if app.shutdownTimeout <= 0 {
		app.shutdownTimeout = 30 * time.Second
	}
	if app.terminalPollInterval <= 0 {
		app.terminalPollInterval = defaultTerminalPollInterval
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
	// Build the runtime metrics exporter once (noop => nil). It is shared by
	// every bridge.Builder across config reloads and owns a flush goroutine,
	// so it is created here (not in newFactoryRegistry, which runs per
	// reload) and Closed in Stop. On a later Start failure the deferred
	// cleanup below Closes it to avoid a goroutine leak.
	//
	// Built BEFORE the credential store so the store's runtime.Credential
	// resolver can emit credential resolve/stale metrics through this same
	// exporter (Finding 4).
	if a.metricsExporter == nil {
		exporter, err := newMetricsExporter(ctx, a.cfg, a.logger)
		if err != nil {
			return err
		}
		a.metricsExporter = exporter
	}
	startOK := false
	defer func() {
		if !startOK && a.metricsExporter != nil {
			_ = a.metricsExporter.Close(context.Background())
			a.metricsExporter = nil
		}
	}()

	if a.credentialStore == nil {
		store, err := newDefaultCredentialStore(ctx, a.cfg, a.metricsExporter, a.logger)
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

	source := newOptionalFileSource(a.cfg.ConfigFilePath, a.pluginRegistry, a.logger, func() *ports.BridgeConfig {
		return defaultLogicalConfig(a.cfg)
	})
	watcher := newPollWatcher(ctx, a.cfg, a.pluginRegistry, a.logger)
	a.manager = config.NewManager(
		config.Layer{Name: "file", Loader: source, Watcher: watcher},
		config.WithManagerLogger(a.logger),
	)

	logicalCfg, err := a.manager.Load(ctx)
	if err != nil {
		return err
	}
	a.logicalRef.Set(logicalCfg)

	// The file-based profile configures the admin/monitor listeners from
	// bootstrap env/SSM (a.cfg + apiKeysRef) and expects TLS to terminate at the
	// load balancer (ALB), so the bridge config `http:` block is not honored.
	// Enforce that policy BEFORE building the runtime or starting any server: a
	// tls_cert_file/tls_key_file pair is an explicit "encrypt this" the profile
	// cannot satisfy, so fail closed rather than silently serve the admin API in
	// plaintext; a bare addrs/keys block is warned and ignored (Chunk 16
	// Finding 2).
	if err := checkIgnoredHTTPBlock(a.logger, logicalCfg); err != nil {
		return err
	}

	if _, err := a.applyLogicalIfChanged(ctx, logicalCfg, true); err != nil {
		return err
	}
	a.manager.NotifyApplyResult(logicalCfg, nil)

	// Every node starts the transport, admin, and monitor servers regardless
	// of NodeRole (workers still expose the admin listener today — see
	// infra.BootstrapConfig.NodeRole). NodeRole IS consulted below for the
	// config single-writer posture (apiCfg.ConfigSingleWriter): only the
	// control/single node is the sole durable config writer.
	a.transportServer = newTransportServer(a.handlerRef, a.logger)
	if err := a.transportServer.Start(a.cfg.TransportHTTPAddr); err != nil {
		return fmt.Errorf("bootstrap: start transport HTTP server: %w", err)
	}

	apiCfg := httpapi.Config{
		AdminAddr:             a.cfg.AdminAddr,
		MonitorAddr:           a.cfg.MonitorAddr,
		CORSOrigins:           a.cfg.CORSOrigins,
		AdminAPIKeysProvider:  a.apiKeysRef.AdminKeys,
		MonitorAPIKeyProvider: a.apiKeysRef.MonitorKey,
		RuntimeProvider: func() ports.Runtime {
			if rt := a.runtimeRef.Get(); rt != nil {
				return rt
			}
			// No active runtime. During a normal swap window this is
			// transient — return nil so /live stays 200 and the process is
			// not restarted. Once WEDGED (a prepare/commit swap AND its
			// recovery both failed) there is no self-recovery path, so
			// expose a sentinel terminal runtime: the monitor /live probe
			// (503 only when rt != nil && rt.Terminal()) then fails closed
			// and the orchestrator restarts the task. This closes the
			// wedged-nil blind spot on the /live side, matching the terminal
			// backstop's coverage via runtimeTerminal().
			if a.wedged.Load() {
				return terminalRuntime{}
			}
			return nil
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
		// Surface both watcher failure and desired/running apply divergence.
		ConfigWatchProvider: a.configWatchHealth,
		// ConfigApplier converges the running runtime in-band when a config
		// is committed through the admin transactions API, reusing the exact
		// reload path the file watcher drives (applyLogicalConfig) instead of
		// waiting for the next poll. httpapi invokes it AFTER the durable
		// write, so a returned error surfaces as committed_not_applied (the
		// operator reconciles) rather than a false "committed" while the
		// runtime diverges. Without this wiring the committed_not_applied /
		// errConfigApplyFailed path is dead in the shipped binary.
		ConfigApplier: a.applyCommittedConfig,
		// ConfigSingleWriter asserts THIS admin process is the sole durable
		// writer of the config store. The profile's ConfigStore is a
		// parser.FileStore (non-CAS: no ports.ConditionalConfigStore), so the
		// httpapi config-transaction commit path FAILS CLOSED on a durable
		// commit unless single-writer is asserted. In the file-based profile
		// the CONTROL/single node owns the RW EFS mount and is the only admin
		// writer (GoBridgeSingle is one task; GoBridgeCluster forces the
		// control service to DesiredCount=1 and mounts workers RO), so the
		// control role IS the sole writer. Derive the flag from NodeRole rather
		// than hardcoding: worker nodes get false (their commits correctly fail
		// closed — a RO EFS mount could not durably persist anyway), and a
		// genuine multi-writer deployment must instead wire a CAS ConfigStore
		// (ports.ConditionalConfigStore.SaveIfVersion — see xcut filestore-cas),
		// which is always safe regardless of this flag.
		ConfigSingleWriter: a.configSingleWriter(),
	}
	a.httpServer = httpapi.New(nil, apiCfg,
		httpapi.WithServerLogger(a.logger),
		httpapi.WithAuditLogger(httpapi.NewSlogAuditLogger(a.logger)),
		// Reuse the SAME shared, close-shielded exporter the runtime receives
		// (registry.go wires it via bridge.WithMetrics) so admin-plane DLQ
		// redrive metrics land in one sink. nil for the noop profile → no-op.
		httpapi.WithMetrics(a.metricsExporter),
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
	a.rootCtx = watchCtx
	a.started = true

	// RECONFIG-1: watch the INITIALLY-applied runtime for convergence too (initial
	// startup has the same truthfulness gap as a reload). installPlan skipped
	// starting it during the pre-rootCtx initial apply, so start it here.
	a.startConvergenceWatch(watchCtx, a.runtimeRef.Get(), a.appliedRef.Get())

	a.watchWg.Go(func() {
		a.watchLoop(watchCtx, watchCh)
	})
	a.watchWg.Go(func() {
		a.watchTerminal(watchCtx)
	})

	startOK = true
	return nil
}

// configSingleWriter reports whether THIS node is the sole durable writer of
// the config store, which the served httpapi.Config asserts via
// ConfigSingleWriter. The file-based profile's ConfigStore is a
// parser.FileStore — a non-CAS store (it does not implement
// ports.ConditionalConfigStore) — so the httpapi config-transaction commit path
// fails closed on a durable commit unless single-writer is asserted.
//
// The decision is derived from the deploy-time NodeRole rather than hardcoded:
//
//   - control (and the empty default normalized to control by
//     BootstrapConfig.Normalized, covering GoBridgeSingle and library/local
//     use) owns the RW EFS mount and is the ONLY admin writer — GoBridgeCluster
//     forces the control service to DesiredCount=1 — so it is the sole writer.
//   - worker mounts EFS read-only in GoBridgeCluster and is NOT a durable
//     writer; returning false makes a worker's commit fail closed (correct: a
//     RO mount could not persist anyway) instead of a silent last-writer-wins.
//
// A genuine multi-writer deployment (multiple concurrent admin writers against
// one backend) cannot be made safe with a non-CAS FileStore; it MUST wire a
// ports.ConditionalConfigStore instead, which is always safe regardless of this
// flag (xcut filestore-cas: parser.FileStore lacks SaveIfVersion today).
func (a *App) configSingleWriter() bool {
	return a.cfg.NodeRole == deployinfra.NodeRoleControl
}

// checkIgnoredHTTPBlock enforces the file-based deployment profile's policy for
// the optional bridge config `http:` block. This profile sources admin/monitor
// addresses and API keys from bootstrap env/SSM (a.cfg / apiKeysRef) and expects
// TLS to terminate at the load balancer (ALB); it does NOT feed ports.HTTPConfig
// into the served httpapi.Config. Only the cmd/gobridge binary and library
// embeddings honor the `http:` block.
//
// A tls_cert_file/tls_key_file entry is an explicit "encrypt this" instruction
// the profile cannot satisfy in-process. Silently serving the admin API in
// plaintext would be a security fail-open, so any TLS entry FAILS CLOSED and
// aborts startup — mirroring httpapi's own refusal to run a misconfigured TLS
// listener (httpapi/server.go rejects a half-set pair). An operator who needs
// in-process TLS must use the cmd/gobridge binary or a library embedding.
//
// A bare `http:` block (addresses/keys only, no TLS entry) is legitimately
// overridden by the SSM-driven bootstrap, so it is warned-and-ignored rather
// than rejected: WARN-and-continue keeps existing deployments booting. The
// addresses/keys are deliberately NOT re-sourced from logical.HTTP (that would
// double-source and conflict with the SSM-driven design); API keys are secrets
// and are never logged.
func checkIgnoredHTTPBlock(logger *slog.Logger, logical *ports.BridgeConfig) error {
	if logical == nil || logical.HTTP == nil {
		return nil
	}
	if logical.HTTP.TLSCertFile != "" || logical.HTTP.TLSKeyFile != "" {
		return fmt.Errorf("bootstrap: the file-based deployment profile does not serve in-process TLS " +
			"(TLS is expected to terminate at the load balancer), so the bridge config `http:` block's " +
			"tls_cert_file/tls_key_file cannot be honored here; continuing would silently serve the admin " +
			"API in plaintext. Remove the TLS pair from bridge.yaml, or use the cmd/gobridge binary or a " +
			"library embedding if you need in-process TLS")
	}
	logger.Warn("bootstrap: the file-based deployment profile does NOT honor the bridge config `http:` block; "+
		"admin/monitor addresses and API keys are configured via bootstrap env/SSM and TLS is expected to "+
		"terminate at the load balancer (ALB). The `http:` block is honored only by the cmd/gobridge binary "+
		"and library embeddings — it is IGNORED here",
		"admin_addr", logical.HTTP.AdminAddr,
		"monitor_addr", logical.HTTP.MonitorAddr,
		"tls_cert_file_set", logical.HTTP.TLSCertFile != "",
		"tls_key_file_set", logical.HTTP.TLSKeyFile != "",
	)
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
		// SIGTERM path, not shutdown_timeout + drain_timeout stacked (MQTT-C4).
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
			// applyLogicalIfChanged skips the rebuild when the emitted
			// config is byte-identical (canonically) to the running one —
			// e.g. the poll watcher re-emitting the admin-commit write that
			// applyCommittedConfig already applied in-band — so an admin
			// commit costs exactly one runtime swap, not two.
			a.mu.Lock()
			skipped, err := a.applyLogicalIfChanged(ctx, logicalCfg, true)
			a.mu.Unlock()
			a.manager.NotifyApplyResult(logicalCfg, err)
			switch {
			case err != nil:
				a.logger.Warn("bootstrap: config reload rejected; keeping last good runtime", "error", err)
			case skipped:
				a.logger.Debug("bootstrap: config reload matches the running config (already applied in-band); skipping redundant runtime swap")
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
	// Fail closed on an uncoordinated clustered live reload (finding H8). A
	// per-process reload has no cluster-wide version barrier or coordinated
	// rollback, so rolling a new config into (or out of) a clustered cohort would
	// leave members split across versions with no all-member readiness gate.
	// Refuse the whole class here — BEFORE any Plan/build/resource creation or
	// stop — and require an externally coordinated whole-cohort replacement
	// (docs/runbooks/cluster-config-rollout.md). Returning an error routes
	// through the EXISTING reload-failure path: watchLoop keeps the last-good
	// runtime, and applyCommittedConfig surfaces committed_not_applied.
	//
	// oldApplied == nil means this is the INITIAL apply (a fresh boot into a
	// clustered config is legitimate), not a live reload, so it is exempt.
	// bridge.IsClusteredDeployment is the SHARED predicate: guard when EITHER the
	// applied OR the proposed config is clustered. A genuine no-op re-emit never
	// reaches here — applyLogicalIfChanged short-circuits it on the content
	// fingerprint before calling applyLogicalConfig, so no-op reloads stay
	// accepted.
	if oldApplied := a.appliedRef.Get(); oldApplied != nil &&
		(bridge.IsClusteredDeployment(oldApplied) || bridge.IsClusteredDeployment(logical)) {
		return fmt.Errorf("bootstrap: refusing live reload of a clustered deployment: a per-process " +
			"reload has no cluster-wide version barrier or coordinated rollback, so a rolling reload " +
			"would split the cohort across config versions. Externally coordinate a whole-cohort " +
			"replacement (stage, validate every member, quiesce ingress, drain/stop all members, " +
			"commit, start all members, verify the version/readiness barrier, then re-enable ingress); " +
			"see docs/runbooks/cluster-config-rollout.md")
	}

	// Apply bridge.log_level first so debug logging takes effect immediately
	// on reload — even when the reload that raised the level is itself being
	// diagnosed. A no-op when no LevelVar was wired (WithLogLevelVar).
	a.applyLogLevel(logical)

	if err := validateDeploymentProfile(a.cfg, logical); err != nil {
		return err
	}

	plan, err := a.prepareRuntimePlan(ctx, logical)
	if err != nil {
		return err
	}

	oldRuntime := a.runtimeRef.Get()
	oldApplied := a.appliedRef.Get()
	oldRegistry := a.registryRef.Load()

	switch plan.mode {
	case swapModePrepareCommit:
		return a.applyPrepareCommit(ctx, plan, oldRuntime, oldApplied, oldRegistry)
	default:
		return a.applyOverlap(ctx, plan, oldRuntime, oldApplied, oldRegistry)
	}
}

func (a *App) prepareRuntimePlan(ctx context.Context, logical *ports.BridgeConfig) (*runtimePlan, error) {
	inputs, err := resolveInputs(ctx, a.parameterResolver, a.cfg, a.pluginRegistry, logical)
	if err != nil {
		return nil, err
	}
	if err := applyMQTTMemoryProfile(inputs.RuntimeConfig, a.cfg); err != nil {
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
	oldRegistry *factoryRegistry,
) error {
	if err := plan.runtime.Start(ctx); err != nil {
		// RECONFIG-2: a candidate whose late Start fails still opened stores,
		// sessions, and adapter resources during Build. Stop it before returning so
		// repeated failing applies do not leak store handles and background state.
		// Bounded teardown (context.Background) like every other reload-path stop.
		_ = stopRuntime(context.Background(), plan.runtime, plan.logical)
		return fmt.Errorf("bootstrap: start runtime: %w", err)
	}

	// If anything below panics, ensure the started runtime is cleaned up.
	// Reload-path drain: NOT bounded by process shutdown (context.Background).
	installed := false
	defer func() {
		if !installed {
			_ = stopRuntime(context.Background(), plan.runtime, plan.logical)
		}
	}()

	a.installPlan(plan)
	installed = true

	if oldRuntime != nil {
		if err := stopRuntime(context.Background(), oldRuntime, oldApplied); err != nil {
			a.logger.Warn("bootstrap: stop old runtime after overlap swap", "error", err)
		}
	}
	// Drain the superseded registry's SSE senders ONLY on this success tail:
	// installPlan has swapped handlerRef to the new mux, so clients still
	// pinned to the OLD sender must be released to reconnect to the new one
	// (otherwise they sit on the orphaned sender receiving heartbeats but no
	// events). This is deliberately AFTER installPlan and NOT on the early
	// "start runtime" error return above — that path leaves the old
	// runtime/handler live, so draining there would disconnect healthy clients
	// from a still-serving sender.
	a.closeSupersededHTTP(ctx, oldRegistry)
	return nil
}

func (a *App) applyPrepareCommit(
	ctx context.Context,
	plan *runtimePlan,
	oldRuntime *goruntime.Runtime,
	oldApplied *ports.BridgeConfig,
	oldRegistry *factoryRegistry,
) error {
	// Every return path here supersedes the old registry: success installs the
	// new mux via installPlan, and each failure path routes through
	// recoverPrevious — which installs a freshly-rebuilt registry or wedges to
	// http.NotFoundHandler — never the old one. So drain the old registry's SSE
	// senders on the way out regardless of outcome. Unlike applyOverlap there
	// is no "old still live" early return to protect: the old runtime is
	// stopped up front (below), so its handler is already being torn down.
	defer a.closeSupersededHTTP(ctx, oldRegistry)

	if oldRuntime != nil {
		if err := stopRuntime(context.Background(), oldRuntime, oldApplied); err != nil {
			// RECONFIG-2: prepare/commit stops the old runtime BEFORE committing the
			// new one, so a failed stop leaves the old runtime in an uncertain state
			// (its exclusive broker session / lease may still be held). Proceeding to
			// commit a new runtime onto the same identity would risk two owners.
			// Abort instead: keep the old runtime installed (runtimeRef unchanged) and
			// surface the error as committed_not_applied so an operator reconciles.
			return fmt.Errorf("bootstrap: abort prepare/commit swap; old runtime did not stop cleanly "+
				"(uncertain ownership, refusing to commit a replacement): %w", err)
		}
	}
	a.runtimeRef.Set(nil)

	newRuntime, err := plan.plan.Commit(ctx)
	if err != nil {
		// RECONFIG-2: a partially-built candidate from a failed Commit still holds
		// resources; stop it (nil-safe) before rebuilding the previous runtime.
		if newRuntime != nil {
			_ = stopRuntime(context.Background(), newRuntime, plan.logical)
		}
		a.recoverPrevious(ctx, oldApplied)
		return fmt.Errorf("bootstrap: complete runtime: %w", err)
	}
	if err := newRuntime.Start(ctx); err != nil {
		// RECONFIG-2: stop the committed-but-unstarted candidate before recovering,
		// so its opened stores/sessions are released rather than leaked.
		_ = stopRuntime(context.Background(), newRuntime, plan.logical)
		a.recoverPrevious(ctx, oldApplied)
		return fmt.Errorf("bootstrap: start runtime: %w", err)
	}
	plan.runtime = newRuntime
	a.installPlan(plan)
	return nil
}

func (a *App) recoverPrevious(ctx context.Context, logical *ports.BridgeConfig) {
	if logical == nil {
		a.enterWedgedState()
		return
	}

	plan, err := a.prepareRuntimePlan(ctx, logical)
	if err != nil {
		a.logger.Error("bootstrap: failed to rebuild previous runtime after prepare/commit failure", "error", err)
		a.enterWedgedState()
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
		// RECONFIG-2: the recovery candidate itself failed to commit/start. Stop it
		// (nil-safe) so it does not leak resources on top of the failed swap before
		// entering the wedged state that the orchestrator restarts out of.
		if plan.runtime != nil {
			_ = stopRuntime(context.Background(), plan.runtime, logical)
		}
		a.logger.Error("bootstrap: failed to restart previous runtime after prepare/commit failure", "error", err)
		a.enterWedgedState()
		return
	}

	a.installPlan(plan)
}

// degradedConfigWatch reports whether live reconfiguration is degraded: the
// config manager's layer watcher cannot be (re)established, so the App keeps
// serving its last good config but no longer observes config changes. It is the
// deep-health projection wired to httpapi's DegradedProvider so operators can
// see a bridge running blind. The terminal "wedged" state (no active runtime at
// all) is a separate, harder failure already surfaced via /live and the
// terminal backstop, so it is intentionally not folded in here.
func (a *App) degradedConfigWatch() (bool, string) {
	// RECONFIG-1: an applied-but-not-converged swap is a degraded state even when
	// the config-watch layer itself is healthy. Report it alongside (or instead of)
	// a watcher error so operators see the same ConfigDegraded signal the generic
	// Supervisor emits.
	convDegraded, convReason := a.convergenceDegradedState()

	if a.manager == nil {
		return convDegraded, convReason
	}
	errs := a.manager.WatchErrors()
	if len(errs) == 0 {
		return convDegraded, convReason
	}
	layers := make([]string, 0, len(errs))
	for layer := range errs {
		layers = append(layers, layer)
	}
	slices.Sort(layers) // deterministic reason ordering
	parts := make([]string, len(layers))
	for i, layer := range layers {
		parts[i] = fmt.Sprintf("%s: %v", layer, errs[layer])
	}
	reason := "config watch degraded: " + strings.Join(parts, "; ")
	if convDegraded {
		reason += "; " + convReason
	}
	return true, reason
}

func (a *App) configWatchHealth() httpapi.ConfigWatchHealth {
	degraded, reason := a.degradedConfigWatch()
	status := httpapi.ConfigWatchHealth{Degraded: degraded, Reason: reason}
	if a.manager == nil {
		return status
	}
	status.ReconfigurePending = a.manager.ReconfigurePending()
	if version, ok := a.manager.AppliedVersion(); ok {
		status.DesiredVersion = &version
	}
	if version, ok := a.manager.RunningVersion(); ok {
		status.RunningVersion = &version
	}
	if err := a.manager.LastApplyError(); err != nil {
		status.LastApplyError = err.Error()
		status.Degraded = true
	}
	if status.ReconfigurePending {
		status.Degraded = true
		if status.Reason == "" {
			status.Reason = "desired configuration is not running"
		}
	}
	return status
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

func (a *App) installPlan(plan *runtimePlan) {
	// A successful apply/recovery clears any prior wedged latch: the App now
	// has an active runtime again and can self-heal.
	a.wedged.Store(false)
	a.runtimeRef.Set(plan.runtime)
	a.appliedRef.Set(plan.logical)
	a.apiKeysRef.Set(plan.inputs.AdminAPIKey, plan.inputs.MonitorAPIKey)
	a.handlerRef.Set(plan.registry.transportHandler())
	// Retain the installed registry so its HTTP transport's SSE senders can be
	// drained when this registry is later superseded or on shutdown (see
	// closeSupersededHTTP and Stop). Stored last, after handlerRef already
	// points at this registry's mux.
	a.registryRef.Store(plan.registry)
	// RECONFIG-1: begin (or supersede) the post-swap convergence watch for the
	// freshly installed runtime. Skipped during the pre-rootCtx initial apply
	// (Start begins the initial watch explicitly once rootCtx exists).
	if a.rootCtx != nil {
		a.startConvergenceWatch(a.rootCtx, plan.runtime, plan.logical)
	}
	if a.onRuntimeInstalled != nil {
		a.onRuntimeInstalled()
	}
}

// closeSupersededHTTP drains the SSE senders of a superseded (or shutting-down)
// factory registry's HTTP transport so clients pinned to its mux disconnect and
// reconnect to the newly installed one, and so a fronting server.Shutdown does
// not hang on long-lived SSE handlers. nil-safe; idempotent (SSESender.Close
// uses sync.Once).
func (a *App) closeSupersededHTTP(ctx context.Context, reg *factoryRegistry) {
	if reg == nil || reg.http == nil {
		return
	}
	if err := reg.http.Close(ctx); err != nil {
		a.logger.Warn("bootstrap: drain superseded HTTP SSE senders", "error", err)
	}
}

// applyCommittedConfig is the httpapi ConfigApplier hook. It converges the
// running runtime on a config committed through the admin transactions API by
// driving the same reload path the file watcher uses (applyLogicalConfig),
// serialized under mu against watchLoop's reloads. httpapi calls it after the
// durable write, so a returned error is surfaced as committed_not_applied
// (disk and runtime diverged; the operator reconciles).
//
// The commit's durable write changes the on-disk content hash, so the poll
// watcher re-emits the same config on its next tick. applyLogicalIfChanged
// records this config's fingerprint here, so that re-emit is recognised as
// already-applied and SKIPPED — avoiding a second, redundant stop→rebuild→start
// swap (and a second exposure to the swap-failure→wedge path) seconds after
// this one. If the watcher happens to win the race and apply the committed
// config first, applyLogicalIfChanged short-circuits here instead: either way
// a commit costs exactly one runtime swap.
func (a *App) applyCommittedConfig(ctx context.Context, cfg *ports.BridgeConfig) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logicalRef.Set(cfg)
	if _, err := a.applyLogicalIfChanged(ctx, cfg, false); err != nil {
		return fmt.Errorf("bootstrap: apply committed config: %w", err)
	}
	return nil
}

// applyLogicalIfChanged applies logical unless it is byte-identical (in
// canonical wire form) to the last successfully-applied config, in which case
// it is a no-op that returns skipped=true. This makes reloads idempotent so a
// config re-emitted by the poll watcher (which fires after every on-disk
// change, including the admin-commit write applyCommittedConfig already applied
// in-band) does not trigger a second, redundant stop→rebuild→start swap.
//
// Caller MUST hold a.mu. parsed indicates logical is already in the watcher's
// parsed form (see parsedFingerprint). The fingerprint is recorded only on a
// successful apply, so a rejected reload does not suppress a later retry of the
// same bytes once the underlying problem is fixed.
func (a *App) applyLogicalIfChanged(ctx context.Context, logical *ports.BridgeConfig, parsed bool) (bool, error) {
	fp := a.parsedFingerprint(logical, parsed)
	if fp != "" && fp == a.lastAppliedFingerprint {
		if a.onReloadSkipped != nil {
			a.onReloadSkipped()
		}
		return true, nil
	}
	if err := a.applyLogicalConfig(ctx, logical); err != nil {
		return false, err
	}
	if fp != "" {
		a.lastAppliedFingerprint = fp
	}
	return false, nil
}

// parsedFingerprint computes the fingerprint of cfg as the poll watcher
// observes it — i.e. after a parse round-trip. The watcher always emits parsed
// configs, so a config already in parsed form (parsed=true: from the watcher,
// manager.Load, or a prior reload) is fingerprinted directly. The in-band
// commit path passes the in-memory merged config (parsed=false); it is
// canonicalised through cloneBridgeConfig — Parse(MarshalYAML(cfg)) — so its
// fingerprint matches the parsed form the watcher re-emits from the identical
// on-disk projection (FileStore.Save writes MarshalYAML(cfg); the watcher
// parses those exact bytes). No parse∘marshal fixed-point is assumed: both
// sides fingerprint Parse(MarshalYAML(cfg)). Returns "" when the fingerprint
// cannot be computed, which fails open (the config is applied, not skipped).
func (a *App) parsedFingerprint(cfg *ports.BridgeConfig, parsed bool) string {
	canonical := cfg
	if !parsed {
		clone, err := cloneBridgeConfig(cfg, a.pluginRegistry)
		if err != nil || clone == nil {
			return ""
		}
		canonical = clone
	}
	fp, err := configFingerprint(canonical)
	if err != nil {
		return ""
	}
	return fp
}

// configFingerprint returns a stable content hash of cfg in its canonical wire
// form. Two configs with the same fingerprint marshal to identical bytes, so
// re-applying one over the other is a genuine no-op — the basis for skipping
// the poll watcher's re-emit of a config already applied.
func configFingerprint(cfg *ports.BridgeConfig) (string, error) {
	if cfg == nil {
		return "", nil
	}
	data, err := cfgparser.MarshalYAML(cfg)
	if err != nil {
		return "", fmt.Errorf("bootstrap: fingerprint config: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
