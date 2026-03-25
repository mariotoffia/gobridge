package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/httpapi"

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

	builder := bridge.NewBuilder(cfg, bridge.WithLogger(logger))

	builder.RegisterTransport("mqtt", paho.NewBridgeFactory(logger))
	builder.RegisterStoreFactory("memory", nativestore.NewMemoryStoreFactory())

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
	//   builder.RegisterTransport("sqs", sqs.NewBridgeFactory(logger))
	//   builder.RegisterStoreFactory("dynamodb", awsstore.NewDynamoDBStoreFactory(ddbClient))

	rt, err := builder.Build(ctx)
	if err != nil {
		logger.Error("failed to build runtime", "error", err)
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

	if err := rt.Start(ctx); err != nil {
		logger.Error("failed to start runtime", "error", err)
		os.Exit(1)
	}
	logger.Info("bridge started", "instance_id", rt.InstanceID())

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	received := <-sig
	logger.Info("shutdown signal received", "signal", received.String())

	cancel()

	go func() {
		s := <-sig
		logger.Error("second signal received, forcing exit", "signal", s.String())
		os.Exit(2)
	}()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Bridge.ShutdownTimeoutDuration())
	defer shutdownCancel()

	if err := rt.Stop(shutdownCtx); err != nil {
		logger.Error("runtime shutdown error", "error", err)
	}
	logger.Info("bridge stopped")
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

