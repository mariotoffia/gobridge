// Command gobridge is the reference composition root for the GoBridge runtime
// and the binary packaged by deployment/kubernetes. Without build tags it is a
// blank root: no transports, stores or telemetry exporters are linked. The file
// config source, file:// credentials and admin/monitor HTTP API remain available.
//
// Add plugin families with gobridge_mqtt and gobridge_native build tags, or
// gobridge_all for every available family. The Kubernetes image explicitly
// selects MQTT and native memory/SQLite stores. Configs naming a transport or
// store outside the compiled families fail at decoding with an unknown-kind error.
//
// The AWS image ghcr.io/mariotoffia/gobridge is the other shipped composition
// root, deployment/aws-filebased-config/lib/cmd/gobridge-filebased: MQTT, SQS
// and HTTP transports, DynamoDB stores, secrets from SSM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/httpapi"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	credentials "github.com/mariotoffia/gobridge/runtime/credentials"

	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	filecreds "github.com/mariotoffia/gobridge/adapters/native/credentials/file"
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
		_, _ = fmt.Fprintf(out, `gobridge — reference composition root (the Kubernetes profile's binary).
Links the MQTT transport, native memory/SQLite stores and file:// credentials.
A config referencing any other transport/store (SQS, Azure, AMQP, DynamoDB, …)
is REJECTED at startup/reload; the AWS image ghcr.io/mariotoffia/gobridge
(deployment/aws-filebased-config) bundles those, or build your own root.

Usage of %s:
`, os.Args[0])
		flag.PrintDefaults()
	}
	configPath := flag.String("config", "bridge.yaml", "path to configuration file")
	logLevel := flag.String("log-level", "info", "log level ("+strings.Join(ports.LogLevelNames(), ", ")+")")
	credentialsDir := flag.String("credentials-dir", "credentials",
		"base directory backing file:// credential URIs (native file credential store)")
	startEmpty := flag.Bool("start-empty", true,
		"start with an empty configuration when -config does not exist; "+
			"set false to refuse to boot a bridge that would carry no routes")
	var seedBaselines repeatableFlag
	flag.Var(&seedBaselines, "seed-managed-subscriptions",
		"seed the managed-subscription baseline of a persistent/exclusive MQTT session and exit: "+
			"`session-id` attests a NEW broker identity with no subscriptions, "+
			"`session-id=filter,filter` records the exact filters the existing broker session holds; repeatable")
	flag.Parse()

	logger := newLogger(*logLevel)

	// State the adapter set once at startup, so a config that names a transport
	// this root does not link fails with the reason already in the log.
	logger.Info("gobridge reference composition root: MQTT transport, native memory/SQLite stores, file:// credentials; " +
		"other transports and stores need the AWS image (ghcr.io/mariotoffia/gobridge) or a custom composition root")

	// Register only the decoders selected by the compiled plugin families.
	reg := ports.NewRegistry()
	if err := registerAllDecoders(reg); err != nil {
		logger.Error("failed to register plugin decoders", "error", err)
		return 1
	}

	fileSource := fileconfig.NewSource(*configPath, reg)
	// Start-empty: a missing config file is a supported, healthy state — mirror
	// the deployment profile's optionalFileSource. The bridge boots with an
	// empty logical config (bridge.id only, zero routes) behind a loud WARN and
	// converges once the file is created; the watcher watches the DIRECTORY, so
	// file creation is picked up. It does NOT converge through the admin config
	// API: this root binds its HTTP listeners once from the boot config, and the
	// start-empty config has no HTTP block, so a missing file means there is no
	// admin API and no probe port to recover through. Any other load error
	// (unreadable, bad parse) stays fatal, and -start-empty=false refuses the
	// fallback outright for a deployment that must never carry zero routes.
	loader := configLoader(fileSource, *configPath, *startEmpty, logger)

	// One-shot seed: the baseline is written through the same registry, loader
	// and store factories the bridge below would use, then the process exits
	// so an init container or an operator's shell gets a plain 0/1.
	if len(seedBaselines) > 0 {
		baselines, err := parseManagedSubscriptionBaselines(seedBaselines)
		if err != nil {
			logger.Error("invalid -seed-managed-subscriptions value", "error", err)
			return 2
		}
		if err := seedManagedSubscriptions(context.Background(), loader, baselines, logger); err != nil {
			logger.Error("failed to seed managed subscription baselines", "path", *configPath, "error", err)
			return 1
		}
		return 0
	}

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
	// manager so the manager's desired-vs-running divergence tracking
	// (RunningVersion / ReconfigurePending) actually clears after a swap. The
	// manager correlates by the EXACT config pointer it emitted, which the
	// pipeline forwards unchanged as SwapEvent.NewConfig.
	pipeline := newReloadPipeline(reg, logger, withApplyResultNotifier(mgr))

	// Credential store wiring. The stock binary registers the
	// native file:// credential repository so file:// credential URIs in the
	// config resolve out of the box — previously NO credential store was
	// registered here, so an operator copying a documented file:// example
	// into this image got "no credential store registered". Production builds
	// add SSM (or other) pull stores the SAME way: resolver.Register(...).
	//
	// The resolver is lifted into a runtime-owned push store by the supervisor
	// (poll-based wrapper). EmitOnStart is set so a rotation that
	// lands in the build->watch window is surfaced on the first tick rather
	// than silently baselined; a default jitter (~10% of the interval)
	// de-synchronizes polls so many sessions do not stampede the backend on the
	// same tick.
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

	if err := wireAllFactories(ctx, sup, logger, nil); err != nil {
		logger.Error("failed to wire plugin factories", "error", err)
		return 1
	}

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
	// show the boot config as never-applied until a later reload. This
	// is the definitive success ack for the boot config; later reloads are acked
	// through pipeline.onSwap.
	mgr.NotifyApplyResult(cfg, nil)

	if cfg.HTTP != nil {
		// The listeners below are bound ONCE, from this block. Keep a copy so
		// deep health can report a later reload that changed it as
		// restart-required instead of leaving the change silently inert.
		bootHTTP := *cfg.HTTP
		// Keys may arrive through the environment (a mounted Secret) so the
		// config file never has to carry them; see httpAPIKeys.
		adminKey, monitorKey := httpAPIKeys(cfg.HTTP, os.LookupEnv)
		apiCfg := httpapi.Config{
			AdminAddr:     cfg.HTTP.AdminAddr,
			MonitorAddr:   cfg.HTTP.MonitorAddr,
			AdminAPIKey:   adminKey,
			MonitorAPIKey: monitorKey,
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
			// Surface watcher failure, desired/running apply divergence, and any
			// desired change this process cannot apply without a restart.
			ConfigWatchProvider: func() httpapi.ConfigWatchHealth {
				return configWatchHealth(sup, mgr, &bootHTTP)
			},
			// The supervisor's own terminal state, so /live fails closed the
			// moment a swap AND its recovery both fail. Without it the probe sees
			// only "no runtime", which is indistinguishable from a healthy swap
			// window, and a wedged process keeps answering 200 until the
			// coarse-grained terminal backstop below finally trips.
			TerminalProvider: sup.Terminal,
			// Route admin start/stop through the supervisor so POST /bridge/stop
			// is a clean deliberate pause (not process-suicide) and POST
			// /bridge/start rebuilds a fresh single-use runtime afterwards.
			BridgeController: sup,
			// Apply a committed config in-band by feeding it to the Supervisor's
			// reload path (bypassing the debounce window) and blocking until the
			// swap outcome is known — a failed apply surfaces as
			// committed_not_applied instead of a false "committed". Without this
			// the errConfigApplyFailed path is dead and commits rely solely on
			// the (debounced) file watcher to converge.
			ConfigApplier: pipeline.applyCommitted,
		}
		apiCfg.AdminAddr = orDefault(apiCfg.AdminAddr, defaultAdminAddr)
		apiCfg.MonitorAddr = orDefault(apiCfg.MonitorAddr, defaultMonitorAddr)
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
			stopCtx, stopCancel := context.WithTimeout(context.Background(), currentShutdownTimeout(sup.Config, cfg))
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
	// check would miss that case and idle alive forever. sup.Terminal covers both
	// the wedged nil-runtime case and an active-but-terminal runtime.
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
		// read would block until the full ShutdownTimeout elapsed.
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

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), currentShutdownTimeout(sup.Config, cfg))
	defer shutdownCancel()

	awaitSupervisorShutdown(supExited, supDone, shutdownCtx.Done(), logger)

	logger.Info("bridge stopped")
	return exitCode
}

func newLogger(level string) *slog.Logger {
	// One enum for -log-level and bridge.log_level: an unrecognised flag value
	// falls back to info here (there is no prior level to keep at process start).
	lvl, _ := ports.ParseLogLevel(level)
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

// newDefaultCredentialResolver builds the stock resolver and best-effort
// registers the native file:// store. A file-store init failure —
// e.g. a read-only working directory where ./credentials does not already
// exist — is NOT fatal: file:// URIs then fail at resolve time with a clear
// "no credential repository" error, but a config that uses no file://
// credentials (pms:// only) still boots. EmitOnStart
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
