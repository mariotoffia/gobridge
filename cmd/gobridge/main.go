// Command gobridge is a DEMONSTRATION / REFERENCE binary for the GoBridge
// runtime — it is NOT the production binary and must not be shipped as one.
// It deliberately links a MINIMAL adapter set — the MQTT (paho) transport plus
// the native in-memory and SQLite stores — so it builds with no cloud SDK
// dependencies and runs out of the box for local development and the
// documented scenarios.
//
// Because only MQTT + native stores are registered, a config that references
// any other transport or store (SQS, Azure Service Bus, AMQP, DynamoDB, …) is
// REJECTED at startup or on reload: this binary is not a general "shipped
// bridge". A production deployment links only the transports and stores it
// actually uses and registers them at the two sites this binary already
// demonstrates: the config-decoder registry (reg.Register) and the supervisor
// factories (sup.RegisterTransport / sup.RegisterStoreFactory).
//
// The PRODUCTION binary/image is the file-based composition root
// deployment/aws-filebased-config/lib/cmd/gobridge-filebased (published as
// ghcr.io/mariotoffia/gobridge), or a custom composition root you build the
// same way. See the AWS wiring guidance inline in run(), and the
// deployment/aws-filebased-config profile for a complete production example.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/httpapi"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	credentials "github.com/mariotoffia/gobridge/runtime/credentials"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	filecreds "github.com/mariotoffia/gobridge/adapters/native/credentials/file"
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
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		// Usage is written to the flag output (stderr); a failed write there is
		// not actionable, so the error is deliberately discarded.
		_, _ = fmt.Fprintf(out, `gobridge — DEMO / REFERENCE binary (NOT for production).
Links a minimal adapter set only: MQTT transport + native memory/SQLite stores.
A config referencing any other transport/store (SQS, Azure, AMQP, DynamoDB, …)
is REJECTED at startup/reload. For production use the composition root
deployment/aws-filebased-config/lib/cmd/gobridge-filebased (image
ghcr.io/mariotoffia/gobridge), or build your own.

Usage of %s:
`, os.Args[0])
		flag.PrintDefaults()
	}
	configPath := flag.String("config", "bridge.yaml", "path to configuration file")
	logLevel := flag.String("log-level", "info", "log level (debug, info, warn, error)")
	credentialsDir := flag.String("credentials-dir", "credentials",
		"base directory backing file:// credential URIs (native file credential store)")
	flag.Parse()

	logger := newLogger(*logLevel)

	// Loud, unmissable demo-only banner: this reference binary registers only
	// MQTT + native stores and is NOT the production build. The production
	// binary/image is deployment/aws-filebased-config/lib/cmd/gobridge-filebased
	// (ghcr.io/mariotoffia/gobridge). Emitting it at WARN keeps it visible even
	// at the default log level so nobody ships the demo by mistake.
	logger.Warn("cmd/gobridge is a DEMO/REFERENCE binary (MQTT transport + native memory/SQLite stores only) — NOT for production; " +
		"use deployment/aws-filebased-config/lib/cmd/gobridge-filebased (image ghcr.io/mariotoffia/gobridge) or a custom composition root")

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
	// Start-empty (Finding c15/550): a missing config file is a supported,
	// healthy state — mirror the deployment profile's optionalFileSource. The
	// bridge boots with an empty logical config (bridge.id only, zero routes)
	// behind a loud WARN, and converges once a config is pushed through the
	// admin config API (the txn store treats a missing file as first-write)
	// or the file is created — the watcher watches the directory, so file
	// creation is picked up. Any other load error (unreadable, bad parse)
	// stays fatal.
	loader := &startEmptySource{src: fileSource, path: *configPath, logger: logger}
	baseCfg, err := loader.Load(context.Background())
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
		config.Layer{Name: "file", Loader: loader, Watcher: fileWatcher},
		config.WithManagerLogger(logger),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := mgr.Load(ctx)
	if err != nil {
		logger.Error("invalid config", "error", err)
		return 1
	}

	// The reload pipeline reports DEFINITIVE apply outcomes back to the config
	// manager (XCUT-A) so the manager's desired-vs-running divergence tracking
	// (RunningVersion / ReconfigurePending) actually clears after a swap. The
	// manager correlates by the EXACT config pointer it emitted, which the
	// pipeline forwards unchanged as SwapEvent.NewConfig.
	pipeline := newReloadPipeline(reg, logger, withApplyResultNotifier(mgr))

	// Credential store wiring (Finding 11). The stock binary registers the
	// native file:// credential repository so file:// credential URIs in the
	// config resolve out of the box — previously NO credential store was
	// registered here, so an operator copying a documented file:// example
	// into this image got "no credential store registered". Production builds
	// add SSM (or other) pull stores the SAME way: resolver.Register(...).
	//
	// The resolver is lifted into a runtime-owned push store by the supervisor
	// (poll-based wrapper). EmitOnStart is set (Finding 1) so a rotation that
	// lands in the build->watch window is surfaced on the first tick rather
	// than silently baselined; a default jitter (~10% of the interval,
	// LOW-severity finding) de-synchronizes polls so many sessions do not
	// stampede the backend on the same tick.
	credResolver := newDefaultCredentialResolver(*credentialsDir, logger)
	credPollInterval := credentials.DefaultCredentialPollInterval
	credPollConfig := ports.PollBasedWrapperConfig{
		PollInterval: credPollInterval,
		Jitter:       credPollInterval / 10,
		EmitOnStart:  true,
	}

	sup := bridge.NewSupervisor(
		bridge.WithSupervisorLogger(logger),
		// File-change debouncing is done by the reload pipeline (below) so
		// admin commits can bypass the window and apply in-band; the Supervisor
		// therefore applies each config the pipeline forwards immediately.
		bridge.WithReconfigStrategy(bridge.NewDirectStrategy()),
		bridge.WithOnSwap(pipeline.onSwap),
		bridge.WithSupervisorPolledCredentialStore(credResolver, credPollConfig),
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
	//   // sqs.NewFactory(logger, metrics...) — the optional variadic
	//   // metrics exporter is threaded into every SQS receiver/sender so
	//   // the adapter's metrics emit; omit it for the Noop fallback.
	//   sup.RegisterTransport("sqs", sqs.NewFactory(logger))
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
	//   // The exporter and tracer are SHARED across every hot-reloaded runtime,
	//   // so runtime.Stop only Flushes them — it never Closes them. This
	//   // composition root owns their Close and must call it exactly once at
	//   // process shutdown, or the exporter's flush goroutine leaks and buffered
	//   // spans are dropped (CRITICAL 2):
	//   defer me.Close(context.Background())
	//   defer tr.Close(context.Background())
	//   sup := bridge.NewSupervisor(
	//       // ... existing options ...
	//       bridge.WithSupervisorMetrics(me),
	//       bridge.WithSupervisorTracer(tr),
	//       bridge.WithSupervisorAuditLogger(auditLogger),
	//   )
	//   // Pass the SAME exporter into the HTTP server so admin-plane DLQ
	//   // redrive metrics share the one sink (see httpapi.New below):
	//   //   httpapi.New(rt, apiCfg, /* ... */, httpapi.WithMetrics(me))
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
	// supStopped is closed by the goroutine below AFTER it has buffered Run's
	// single result on supDone. It is a close-only broadcast that lets
	// waitForSupervisorRuntime notice an early Run exit (an initial build/start
	// failure) WITHOUT consuming supDone: supDone carries exactly one value and
	// is reserved for the single downstream reader (the primary select below, or
	// awaitSupervisorShutdown). A closed channel is broadcast-safe, so observing
	// it in the wait and again downstream cannot race or steal the result.
	supStopped := make(chan struct{})
	go func() {
		err := sup.Run(ctx, cfg, pipeline.changes())
		supDone <- err // buffered (cap 1): never blocks; value now readable
		close(supStopped)
	}()

	// Wait for the supervisor to build and start the initial runtime, bounded by
	// initialRuntimeWait (which bounds a slow or hung SYNCHRONOUS initial build —
	// see the constant). The wait also observes supStopped, so an initial
	// build/start failure surfaces promptly instead of blocking the full ceiling.
	waitRes := waitForSupervisorRuntime(sup.Runtime, clock.System, initialRuntimeWait, supStopped)
	rt := waitRes.runtime
	if rt == nil {
		if waitRes.supEnded {
			// Run returned before publishing a runtime: the SYNCHRONOUS initial
			// build or non-blocking start failed (e.g. credential resolution or
			// store construction errored). Broker/session connects run in the
			// background and never gate publication, so they cannot be the cause.
			// Surface its actual error, buffered on supDone. Reading supDone
			// here is race-free: this branch returns immediately, so neither the
			// primary select nor awaitSupervisorShutdown — the single intended
			// reader of supDone — is ever reached.
			if err := <-supDone; err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("supervisor exited before producing a runtime", "error", err)
			} else {
				logger.Error("supervisor stopped before producing a runtime")
			}
		} else {
			logger.Error("supervisor did not produce a runtime within timeout",
				"timeout", initialRuntimeWait)
		}
		cancel()
		return 1
	}

	// Boot apply confirmed: the supervisor built and published the initial
	// runtime for the exact boot config pointer (mgr.Load's desiredConfig). Tell
	// the manager so RunningVersion advances off its pre-apply sentinel and
	// ReconfigurePending clears — otherwise the first divergence report would
	// show the boot config as never-applied until a later reload (XCUT-A). This
	// is the definitive success ack for the boot config; later reloads are acked
	// through pipeline.onSwap.
	mgr.NotifyApplyResult(cfg, nil)

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
			// Surface both watcher failure and desired/running apply divergence.
			ConfigWatchProvider: func() httpapi.ConfigWatchHealth {
				return configWatchHealth(sup, mgr)
			},
			// Route admin start/stop through the supervisor so POST /bridge/stop
			// is a clean deliberate pause (not process-suicide) and POST
			// /bridge/start rebuilds a fresh single-use runtime afterwards
			// (CRITICAL 1).
			BridgeController: sup,
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
		defer func() {
			// Bound the HTTP drain so a wedged in-flight admin request cannot
			// hang process shutdown forever; reuse the bridge shutdown budget.
			stopCtx, stopCancel := context.WithTimeout(context.Background(), cfg.Bridge.ShutdownTimeoutDuration())
			defer stopCancel()
			_ = srv.Stop(stopCtx)
		}()
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

// degradedConfigWatch combines the two independent live-reconfiguration
// degraded signals into the single projection surfaced on /deephealth. The
// supervisor reports degraded when its config-change stream closes; the config
// manager reports degraded when a layer's change watcher cannot be
// re-established. Either means the bridge keeps serving its last good config but
// can no longer observe config changes without a restart.
func degradedConfigWatch(sup *bridge.Supervisor, mgr *config.Manager) (bool, string) {
	if degraded, reason := sup.Degraded(); degraded {
		return true, reason
	}
	if errs := mgr.WatchErrors(); len(errs) > 0 {
		layers := make([]string, 0, len(errs))
		for layer := range errs {
			layers = append(layers, layer)
		}
		slices.Sort(layers) // deterministic reason ordering
		parts := make([]string, len(layers))
		for i, layer := range layers {
			parts[i] = fmt.Sprintf("%s: %v", layer, errs[layer])
		}
		return true, "config watch degraded: " + strings.Join(parts, "; ")
	}
	return false, ""
}

func configWatchHealth(sup *bridge.Supervisor, mgr *config.Manager) httpapi.ConfigWatchHealth {
	degraded, reason := degradedConfigWatch(sup, mgr)
	status := httpapi.ConfigWatchHealth{
		Degraded:           degraded,
		Reason:             reason,
		ReconfigurePending: mgr.ReconfigurePending(),
	}
	if version, ok := mgr.AppliedVersion(); ok {
		status.DesiredVersion = &version
	}
	if version, ok := mgr.RunningVersion(); ok {
		status.RunningVersion = &version
	}
	if err := mgr.LastApplyError(); err != nil {
		status.LastApplyError = err.Error()
		status.Degraded = true
	}
	if status.ReconfigurePending {
		status.Degraded = true
		if status.Reason == "" {
			status.Reason = "desired configuration is not running"
		}
	}
	// A coordinated cluster rollout makes ReconfigurePending expected rather
	// than alarming for as long as the barrier is undecided: this member has
	// deliberately not applied a config the cohort has not committed. Surface
	// the barrier so an operator reading /deephealth sees WHO the cohort is
	// waiting for instead of a bare "desired configuration is not running".
	if rollout, ok := sup.RolloutStatus(); ok {
		status.Rollout = &httpapi.ClusterRolloutHealth{
			MemberID:        rollout.MemberID,
			Generation:      rollout.Generation,
			State:           rollout.State,
			ConfigVersion:   rollout.ConfigVersion,
			Epoch:           rollout.Epoch,
			Acked:           rollout.Acked,
			Nacked:          rollout.Nacked,
			Reason:          rollout.Reason,
			CandidateStaged: rollout.Staged,
		}
	}
	return status
}

// terminalPollInterval is how often the liveness backstop checks whether the
// current runtime has gone terminal. Terminal state only follows a sustained
// failure (e.g. ~30s of lease-store outage before step-down), so a coarse poll
// is ample and cheap.
const terminalPollInterval = 5 * time.Second

// terminalConfirmSamples is how many CONSECUTIVE positive terminal reads the
// backstop requires before it exits the process. A single positive sample can
// be a transient read during a healthy reconfiguration swap window; requiring N
// consecutive confirmations means a swap-window blip never kills a healthy
// process, while a genuine terminal/wedged state (which persists) still trips
// after N×terminalPollInterval (CRITICAL 3).
const terminalConfirmSamples = 3

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

// watchTerminal polls isTerminal every poll interval, returning true only after
// terminalConfirmSamples CONSECUTIVE positive reads (so a transient swap-window
// blip never kills a healthy process), or false when ctx is cancelled. It
// carries no runtime knowledge itself (the caller supplies the predicate), which
// keeps the poll loop trivially testable.
func watchTerminal(ctx context.Context, clk clock.Clock, poll time.Duration, isTerminal func() bool) bool {
	consecutive := 0
	for {
		select {
		case <-ctx.Done():
			return false
		case <-clk.After(poll):
			if isTerminal() {
				consecutive++
				if consecutive >= terminalConfirmSamples {
					return true
				}
			} else {
				consecutive = 0
			}
		}
	}
}

// initialRuntimeWait bounds how long run() waits for the supervisor to publish
// its initial runtime before giving up and exiting non-zero. The runtime is
// published right after the SYNCHRONOUS initial build (Supervisor.buildRuntime)
// and a non-blocking Runtime.Start — broker/session connects run in background
// goroutines and never gate publication, so sup.Runtime() goes non-nil within
// milliseconds of a healthy build. This wait therefore exists to bound a slow or
// hung SYNCHRONOUS startup build (credential resolution, store construction),
// which is UNBOUNDED for the initial build — unlike reconfiguration swaps, whose
// build is bounded by defaultSwapDeadline. 60s tolerates a slow-but-healthy cold
// cloud start (e.g. SSM/STS credential resolve plus a DynamoDB describe or a
// SQLite migration) while still backstopping a hung dependency, and mirrors the
// swap build's defaultSwapDeadline. A genuine build error surfaces promptly via
// supStopped rather than waiting out the full ceiling (Chunk 16 Finding 1).
const initialRuntimeWait = 60 * time.Second

// runtimeWaitResult reports the outcome of waitForSupervisorRuntime: either the
// initial runtime became available (runtime != nil), or the supervisor's Run
// returned before producing one (supEnded). Distinguishing the two lets run()
// surface the supervisor's real error instead of a misleading timeout, and exit
// promptly rather than blocking the full ceiling.
type runtimeWaitResult struct {
	runtime  *goruntime.Runtime
	supEnded bool
}

// waitForSupervisorRuntime blocks until runtimeOf returns a non-nil runtime, the
// supervisor's Run returns before publishing one (supStopped closed), or the
// ceiling elapses. It NEVER reads supDone: the single buffered result is left
// intact for the one downstream reader, so this wait cannot steal the shutdown
// read. supStopped is a close-only broadcast, safe to observe here and again
// downstream.
func waitForSupervisorRuntime(
	runtimeOf func() *goruntime.Runtime,
	clk clock.Clock,
	timeout time.Duration,
	supStopped <-chan struct{},
) runtimeWaitResult {
	// ESSENTIAL: runtime init poll
	deadline := clk.After(timeout)
	for {
		if rt := runtimeOf(); rt != nil {
			return runtimeWaitResult{runtime: rt}
		}
		select {
		case <-supStopped:
			// Run returned before publishing a runtime. Re-check once in case a
			// runtime raced in just as Run exited; otherwise report the early
			// exit so the caller surfaces the buffered supDone error instead of
			// waiting out the full ceiling.
			if rt := runtimeOf(); rt != nil {
				return runtimeWaitResult{runtime: rt}
			}
			return runtimeWaitResult{supEnded: true}
		case <-deadline:
			return runtimeWaitResult{}
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

// startEmptySource wraps the file config source so a MISSING config file
// starts an empty, healthy bridge instead of failing the process (start-empty,
// mirroring the deployment profile's optionalFileSource). The fallback is
// loud: a missing file is warned on every load so an operator who mistyped
// -config sees why nothing is bridged. Only shared.ErrNotFound falls back —
// an unreadable or unparseable file stays a fatal load error, because
// silently replacing a config that EXISTS but cannot be read would swap real
// routes for none.
type startEmptySource struct {
	src    *fileconfig.Source
	path   string
	logger *slog.Logger
}

var _ ports.Loader = (*startEmptySource)(nil)

func (s *startEmptySource) Load(ctx context.Context) (*ports.BridgeConfig, error) {
	cfg, err := s.src.Load(ctx)
	if err == nil {
		return cfg, nil
	}
	if errors.Is(err, shared.ErrNotFound) {
		s.logger.Warn(
			"config file not found; starting empty (no routes will be bridged) — "+
				"create the file or push a config through the admin config API",
			"path", s.path,
		)
		return defaultEmptyConfig(), nil
	}
	return nil, err
}

// defaultEmptyConfig is the start-empty logical config: the one field
// validation requires (bridge.id) plus the same explicit shutdown budgets the
// deployment profile's defaultLogicalConfig sets. Zero routes — the bridge is
// a no-op until configured.
func defaultEmptyConfig() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:              "gobridge",
			DeploymentMode:  "standalone",
			ShutdownTimeout: "30s",
			DrainTimeout:    "30s",
		},
	}
}

// newDefaultCredentialResolver builds the stock resolver and best-effort
// registers the native file:// store (Finding 11). A file-store init failure —
// e.g. a read-only working directory where ./credentials does not already
// exist — is NOT fatal: file:// URIs then fail at resolve time with a clear
// "no credential repository" error, but a config that uses no file://
// credentials (pms:// only) still boots (adversarial Finding 2). EmitOnStart
// and jitter are configured by the caller on the poll wrapper.
func newDefaultCredentialResolver(dir string, logger *slog.Logger) *goruntime.CredentialResolver {
	res := goruntime.NewCredentialResolver(goruntime.WithCredentialResolverLogger(logger))
	if repo, err := filecreds.New(dir, filecreds.WithLogger(logger)); err != nil {
		logger.Warn("file credential store unavailable; file:// credential URIs will not resolve",
			"path", dir, "error", err)
	} else {
		res.Register(repo)
	}
	return res
}
