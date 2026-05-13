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
		os.Exit(1)
	}

	fileSource := fileconfig.NewSource(*configPath, reg)
	baseCfg, err := fileSource.Load(context.Background())
	if err != nil {
		logger.Error("failed to load config", "path", *configPath, "error", err)
		os.Exit(1)
	}

	fileWatcher := fileconfig.NewWatcher(*configPath, reg,
		fileconfig.WithWatchConfig(baseCfg.ConfigWatch),
		fileconfig.WithLogger(logger),
	)

	mgr := config.NewManager(
		config.Layer{Name: "file", Loader: fileSource, Watcher: fileWatcher},
		config.WithManagerLogger(logger),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := mgr.Load(ctx)
	if err != nil {
		logger.Error("invalid config", "error", err)
		os.Exit(1)
	}

	sup := bridge.NewSupervisor(
		bridge.WithSupervisorLogger(logger),
		bridge.WithReconfigStrategy(bridge.NewWindowedStrategy(10*time.Second, 30*time.Second, nil)),
		bridge.WithOnSwap(func(ev bridge.SwapEvent) {
			if ev.Error != nil {
				logger.Error("reconfiguration failed",
					"swap_mode", ev.SwapMode, "error", ev.Error, "duration", ev.Duration)
			} else {
				logger.Info("reconfiguration applied",
					"swap_mode", ev.SwapMode, "duration", ev.Duration)
			}
		}),
	)

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

	watchCh, err := mgr.Watch(ctx)
	if err != nil {
		logger.Error("failed to start config watcher", "error", err)
		os.Exit(1)
	}
	defer mgr.Stop()

	supDone := make(chan error, 1)
	go func() {
		supDone <- sup.Run(ctx, cfg, watchCh)
	}()

	// Wait for the supervisor to build and start the initial runtime.
	rt := waitForSupervisorRuntime(sup, clock.System, 10*time.Second)
	if rt == nil {
		logger.Error("supervisor did not produce a runtime within timeout")
		cancel()
		os.Exit(1)
	}

	if cfg.HTTP != nil {
		apiCfg := httpapi.Config{
			AdminAddr:     cfg.HTTP.AdminAddr,
			MonitorAddr:   cfg.HTTP.MonitorAddr,
			AdminAPIKey:   cfg.HTTP.AdminAPIKey,
			MonitorAPIKey: cfg.HTTP.MonitorAPIKey,
			CORSOrigins:   cfg.HTTP.CORSOrigins,
			RuntimeProvider: func() ports.Runtime {
				rt := sup.Runtime()
				if rt == nil {
					return nil
				}
				return rt
			},
			ConfigStore:    &cfgparser.FileStore{Path: *configPath, Registry: reg},
			ConfigProvider: sup.Config,
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
			os.Exit(1)
		}
		defer func() { _ = srv.Stop(context.Background()) }()
		logger.Info("HTTP servers started", "admin", apiCfg.AdminAddr, "monitor", apiCfg.MonitorAddr)
	}

	logger.Info("bridge started", "instance_id", rt.InstanceID())

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case received := <-sig:
		logger.Info("shutdown signal received", "signal", received.String())
	case err := <-supDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("supervisor exited unexpectedly", "error", err)
		}
	}

	cancel()

	go func() {
		s := <-sig
		logger.Error("second signal received, forcing exit", "signal", s.String())
		os.Exit(2)
	}()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Bridge.ShutdownTimeoutDuration())
	defer shutdownCancel()

	select {
	case err := <-supDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("supervisor shutdown error", "error", err)
		}
	case <-shutdownCtx.Done():
		logger.Error("supervisor shutdown timed out")
	}

	logger.Info("bridge stopped")
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
