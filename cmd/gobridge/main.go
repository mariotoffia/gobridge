// Command gobridge is a demonstration / reference binary for the GoBridge
// runtime. It deliberately links a MINIMAL adapter set — the MQTT (paho)
// transport plus the native in-memory and SQLite stores — so it builds with no
// cloud SDK dependencies and runs out of the box for local development and the
// documented scenarios.
//
// It is NOT a production build. A production deployment links only the
// transports and stores it actually uses and registers them at the two sites
// this binary already demonstrates: the config-decoder registry (reg.Register)
// and the supervisor factories (sup.RegisterTransport / sup.RegisterStoreFactory).
// See the AWS wiring guidance inline in run(), and the
// deployment/aws-filebased-config profile for a complete production example.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/httpapi"
	goruntime "github.com/mariotoffia/gobridge/runtime"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/ports"
)

func main() {
	os.Exit(run())
}

// run executes the full bridge lifecycle and returns the process exit code.
// main is a thin os.Exit(run()) wrapper so that every deferred cleanup (config
// watcher, HTTP server, context cancel) runs before the process exits: os.Exit
// skips defers, so a terminal-triggered exit must flow through a return value
// rather than an os.Exit buried in the body.
func run() int {
	configPath := flag.String("config", "bridge.yaml", "path to configuration file")
	logLevel := flag.String("log-level", "info", "log level (debug, info, warn, error)")
	flag.Parse()

	logger := newLogger(*logLevel)

	// Build the per-process plugin registry by registering each
	// adapter we link in. Adding a new transport/store means a new
	// Register call here — the import alone no longer suffices.
	reg := ports.NewRegistry()
	if err := errors.Join(
		paho.Register(reg),
		nativestore.Register(reg),
	); err != nil {
		logger.Error("failed to register plugin decoders", "error", err)
		return 1
	}

	fileSource := fileconfig.NewSource(*configPath, reg)
	baseCfg, err := fileSource.Load(context.Background())
	if err != nil {
		logger.Error("failed to load config", "path", *configPath, "error", err)
		return 1
	}

	watcherOpts := []fileconfig.WatcherOption{
		fileconfig.WithWatchConfig(baseCfg.ConfigWatch),
		fileconfig.WithLogger(logger),
	}
	// Baseline the watcher's change detection from the hash of the exact bytes
	// the initial Load parsed, rather than a disk re-read taken at Watch time.
	// Otherwise a file edited between Load and Watch is absorbed into the
	// baseline and never emitted, silently running stale config. LoadHash is
	// populated only after the successful Load above.
	if h, ok := fileSource.LoadHash(); ok {
		watcherOpts = append(watcherOpts, fileconfig.WithBaselineHash(h))
	}

	fileWatcher := fileconfig.NewWatcher(*configPath, reg, watcherOpts...)

	mgr := config.NewManager(
		config.Layer{Name: "file", Loader: fileSource, Watcher: fileWatcher},
		config.WithManagerLogger(logger),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := mgr.Load(ctx)
	if err != nil {
		logger.Error("invalid config", "error", err)
		return 1
	}

	pipeline := newReloadPipeline(reg, logger)

	sup := bridge.NewSupervisor(
		bridge.WithSupervisorLogger(logger),
		// File-change debouncing is done by the reload pipeline (below) so
		// admin commits can bypass the window and apply in-band; the Supervisor
		// therefore applies each config the pipeline forwards immediately.
		bridge.WithReconfigStrategy(bridge.NewDirectStrategy()),
		bridge.WithOnSwap(pipeline.onSwap),
	)

	// Demo adapter set: MQTT transport + native memory/SQLite stores only.
	// Production builds register their own transports/stores here (and the
	// matching config decoders above) — see the AWS guidance below.
	sup.RegisterTransport("mqtt", paho.NewFactory(logger))
	sup.RegisterStoreFactory("memory", nativestore.NewMemoryStoreFactory())
	sup.RegisterStoreFactory("sqlite", nativestore.NewSQLiteStoreFactory())

	// AWS adapters require an AWS SDK client. Uncomment and configure
	// when deploying with AWS backing services:
	//
	//   import awsconfig "github.com/aws/aws-sdk-go-v2/config"
	//   import "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	//   import "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	//   import awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	//
	//   awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	//   ddbClient := dynamodb.NewFromConfig(awsCfg)
	//   sup.RegisterTransport("sqs", sqs.NewBridgeFactory(logger))
	//   sup.RegisterStoreFactory("dynamodb", awsstore.NewDynamoDBStoreFactory(ddbClient))

	// Observability wiring (Finding 15). This demo binary links no telemetry
	// exporter, so metrics/traces/audit default to the runtime's Noop
	// implementations. The Supervisor now forwards a MetricsExporter, Tracer,
	// and AuditLogger into every runtime it builds (including hot-reloads) via
	// the options below — pass a real exporter here to instrument a config-driven
	// deployment. The adapters/otel and adapters/aws/metrics packages provide
	// concrete exporters; they are omitted here to keep the demo dependency-free:
	//
	//   import otelmetrics "github.com/mariotoffia/gobridge/adapters/otel/metrics"
	//   import oteltracing "github.com/mariotoffia/gobridge/adapters/otel/tracing"
	//
	//   me, _ := otelmetrics.New(ctx, /* exporter opts */)
	//   tr, _ := oteltracing.New(ctx, /* exporter opts */)
	//   sup := bridge.NewSupervisor(
	//       // ... existing options ...
	//       bridge.WithSupervisorMetrics(me),
	//       bridge.WithSupervisorTracer(tr),
	//       bridge.WithSupervisorAuditLogger(auditLogger),
	//   )
	//
	// The runtime then instruments lease/outbox stores and honors the tracer and
	// audit logger automatically. A production/deploy profile selects the
	// exporter (env or config) and passes it here.

	watchCh, err := mgr.Watch(ctx)
	if err != nil {
		logger.Error("failed to start config watcher", "error", err)
		return 1
	}
	defer mgr.Stop()

	// Debounce raw file-watcher changes here (moved out of the Supervisor) and
	// merge them with in-band admin commits onto the single channel the
	// Supervisor drains. The pipeline drops the watcher's re-emit of a config an
	// admin commit already applied in-band, so a commit costs exactly one swap.
	windowedFile := bridge.NewWindowedStrategy(10*time.Second, 30*time.Second, nil).Filter(ctx, watchCh)
	go pipeline.run(ctx, windowedFile)

	supDone := make(chan error, 1)
	go func() {
		supDone <- sup.Run(ctx, cfg, pipeline.changes())
	}()

	// Wait for the supervisor to build and start the initial runtime.
	rt := waitForSupervisorRuntime(sup, clock.System, 10*time.Second)
	if rt == nil {
		logger.Error("supervisor did not produce a runtime within timeout")
		cancel()
		return 1
	}

	if cfg.HTTP != nil {
		apiCfg := httpapi.Config{
			AdminAddr:     cfg.HTTP.AdminAddr,
			MonitorAddr:   cfg.HTTP.MonitorAddr,
			AdminAPIKey:   cfg.HTTP.AdminAPIKey,
			MonitorAPIKey: cfg.HTTP.MonitorAPIKey,
			CORSOrigins:   cfg.HTTP.CORSOrigins,
			TLSCertFile:   cfg.HTTP.TLSCertFile,
			TLSKeyFile:    cfg.HTTP.TLSKeyFile,
			RuntimeProvider: func() ports.Runtime {
				rt := sup.Runtime()
				if rt == nil {
					return nil
				}
				return rt
			},
			ConfigStore:    &cfgparser.FileStore{Path: *configPath, Registry: reg},
			ConfigProvider: sup.Config,
			// Apply a committed config in-band by feeding it to the Supervisor's
			// reload path (bypassing the debounce window) and blocking until the
			// swap outcome is known — a failed apply surfaces as
			// committed_not_applied instead of a false "committed". Without this
			// the errConfigApplyFailed path is dead and commits rely solely on
			// the (debounced) file watcher to converge.
			ConfigApplier: pipeline.applyCommitted,
		}
		if apiCfg.AdminAddr == "" {
			apiCfg.AdminAddr = ":8080"
		}
		if apiCfg.MonitorAddr == "" {
			apiCfg.MonitorAddr = ":8081"
		}
		auditLogger := httpapi.NewSlogAuditLogger(logger)
		srv := httpapi.New(rt, apiCfg,
			httpapi.WithServerLogger(logger),
			httpapi.WithAuditLogger(auditLogger),
		)
		if err := srv.Start(ctx); err != nil {
			logger.Error("failed to start HTTP server", "error", err)
			return 1
		}
		defer func() { _ = srv.Stop(context.Background()) }()
		logger.Info("HTTP servers started", "admin", apiCfg.AdminAddr, "monitor", apiCfg.MonitorAddr)
	}

	logger.Info("bridge started", "instance_id", rt.InstanceID())

	// Deployment-independent liveness backstop. The /live 503-on-terminal path
	// only restarts the process where a Kubernetes livenessProbe is wired. With
	// HTTP disabled or no probe (systemd, bare process), a terminal
	// (unrecoverable) runtime would otherwise stall silently forever. This
	// watcher takes the process down so an external supervisor restarts it. The
	// channel is buffered so the watcher never blocks if we exit for another
	// reason first.
	//
	// The predicate polls sup.Terminal(), NOT sup.Runtime().Terminal(): when a
	// swap AND its recovery both fail the supervisor is left WEDGED with no
	// active runtime (sup.Runtime() == nil), routing nothing. A runtime-only
	// check would miss that case and idle alive forever (Finding 7). sup.Terminal
	// covers both the wedged nil-runtime case and an active-but-terminal runtime.
	terminalCh := make(chan struct{}, 1)
	go func() {
		if watchTerminal(ctx, clock.System, terminalPollInterval, sup.Terminal) {
			terminalCh <- struct{}{}
		}
	}()

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	exitCode := 0
	supExited := false
	select {
	case received := <-sig:
		logger.Info("shutdown signal received", "signal", received.String())
	case err := <-supDone:
		// The supervisor self-exited: its single result is now consumed, so the
		// bounded shutdown wait below must not read supDone a second time — that
		// read would block until the full ShutdownTimeout elapsed (C3-FU5).
		supExited = true
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("supervisor exited unexpectedly", "error", err)
		}
	case <-terminalCh:
		logger.Error("runtime entered terminal (unrecoverable) state; " +
			"exiting non-zero so the orchestrator restarts the process")
		exitCode = 1
	}

	cancel()

	go func() {
		s := <-sig
		logger.Error("second signal received, forcing exit", "signal", s.String())
		os.Exit(2)
	}()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Bridge.ShutdownTimeoutDuration())
	defer shutdownCancel()

	awaitSupervisorShutdown(supExited, supDone, shutdownCtx.Done(), logger)

	logger.Info("bridge stopped")
	return exitCode
}

// terminalPollInterval is how often the liveness backstop checks whether the
// current runtime has gone terminal. Terminal state only follows a sustained
// failure (e.g. ~30s of lease-store outage before step-down), so a coarse poll
// is ample and cheap.
const terminalPollInterval = 5 * time.Second

// awaitSupervisorShutdown blocks — bounded by the shutdown deadline (done) —
// for the supervisor goroutine to report it has unwound after the root context
// was cancelled. When alreadyExited is true the supervisor already self-exited
// and its single result was consumed by the primary select in run(); there is
// nothing left to wait for, so it returns immediately rather than reading the
// now-drained supDone a second time. A second read would never complete and the
// call would block until the full ShutdownTimeout elapsed (C3-FU5).
func awaitSupervisorShutdown(alreadyExited bool, supDone <-chan error, done <-chan struct{}, logger *slog.Logger) {
	if alreadyExited {
		return
	}
	select {
	case err := <-supDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("supervisor shutdown error", "error", err)
		}
	case <-done:
		logger.Error("supervisor shutdown timed out")
	}
}

// watchTerminal polls isTerminal every poll interval, returning true the first
// time it reports terminal or false when ctx is cancelled. It carries no
// runtime knowledge itself (the caller supplies the predicate), which keeps the
// poll loop trivially testable.
func watchTerminal(ctx context.Context, clk clock.Clock, poll time.Duration, isTerminal func() bool) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-clk.After(poll):
			if isTerminal() {
				return true
			}
		}
	}
}

func waitForSupervisorRuntime(sup *bridge.Supervisor, clk clock.Clock, timeout time.Duration) *goruntime.Runtime {
	// ESSENTIAL: runtime init poll
	deadline := clk.After(timeout)
	for {
		if rt := sup.Runtime(); rt != nil {
			return rt
		}
		select {
		case <-deadline:
			return nil
		case <-clk.After(20 * time.Millisecond):
		}
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
