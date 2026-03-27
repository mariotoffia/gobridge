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
	"github.com/mariotoffia/gobridge/httpapi"
	goruntime "github.com/mariotoffia/gobridge/runtime"

	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
)

func main() {
	configPath := flag.String("config", "bridge.yaml", "path to configuration file")
	logLevel := flag.String("log-level", "info", "log level (debug, info, warn, error)")
	flag.Parse()

	logger := newLogger(*logLevel)

	fileSource := fileconfig.NewSource(*configPath)
	baseCfg, err := fileSource.Load(context.Background())
	if err != nil {
		logger.Error("failed to load config", "path", *configPath, "error", err)
		os.Exit(1)
	}

	fileWatcher := fileconfig.NewWatcher(*configPath,
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
		bridge.WithReconfigStrategy(bridge.NewWindowedStrategy(10*time.Second, 30*time.Second)),
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

	sup.RegisterTransport("mqtt", paho.NewBridgeFactory(logger))
	sup.RegisterStoreFactory("memory", nativestore.NewMemoryStoreFactory())

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
	rt := waitForSupervisorRuntime(sup, 10*time.Second)
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

func waitForSupervisorRuntime(sup *bridge.Supervisor, timeout time.Duration) *goruntime.Runtime {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rt := sup.Runtime(); rt != nil {
			return rt
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
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
