package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// The shipped file-based composition root: the App type itself, the options a
// caller wires it with, and the accessors a host process reads it through. Its
// behaviour lives beside it by phase — startup.go, shutdown.go,
// config_applier.go, config_watch.go, health.go, rollout.go — because the App is
// one long-lived object with several independent lifecycles, and a reader
// following one of them should not have to page through the others.

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

// WithShutdownTimeout pins the graceful shutdown deadline, overriding the boot
// config's bridge.shutdown_timeout. Unset, the App adopts that field (30s when
// it too is unset), so the one budget an operator writes down is the one the
// process actually spends.
func WithShutdownTimeout(d time.Duration) Option {
	return func(a *App) {
		a.shutdownTimeout = d
		a.shutdownTimeoutPinned = d > 0
	}
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

	// Coordinated cluster rollout (design cluster-config-rollout-protocol.md
	// Phase 6). rolloutConfig carries the coordination stores + cadence (injected
	// via WithClusterRolloutStores for tests/custom stores, or built from the
	// DynamoDB client in Start). rolloutDriver is this App's barrier host, built in
	// Start ONLY when the boot config opts into cluster.rollout: coordinated; nil
	// otherwise, so a deployment that did not opt in keeps the ADR 0012 refusal.
	// stopRolloutDrive stops the drive goroutine on Stop, bounded by the process
	// shutdown budget it is handed.
	rolloutConfig bridge.ClusterRolloutConfig
	rolloutDriver *bridge.ClusterRolloutDriver
	// baselineRef holds the generation-zero committed artifact this member verified
	// at startup (nil when the deployment stamped no admitted baseline document).
	// It is written once in Start, before any server is listening, and read
	// concurrently by deep-health probes.
	baselineRef      atomic.Pointer[rolloutBaseline]
	stopRolloutDrive func(context.Context)

	watchCancel context.CancelFunc
	watchWg     sync.WaitGroup

	shutdownTimeout time.Duration
	// shutdownTimeoutPinned records that a caller supplied the budget through
	// WithShutdownTimeout, so Start must not replace it with the boot config's
	// bridge.shutdown_timeout.
	shutdownTimeoutPinned bool
	terminalPollInterval  time.Duration

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
