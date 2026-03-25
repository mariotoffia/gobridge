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
)

func main() {
	configPath := flag.String("config", "bridge.yaml", "path to configuration file")
	logLevel := flag.String("log-level", "info", "log level (debug, info, warn, error)")
	flag.Parse()

	logger := newLogger(*logLevel)

	cfg, err := config.ParseFile(*configPath, config.FormatAuto)
	if err != nil {
		logger.Error("failed to parse config", "path", *configPath, "error", err)
		os.Exit(1)
	}
	if err := config.Validate(cfg); err != nil {
		logger.Error("invalid config", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	builder := bridge.NewBuilder(cfg, bridge.WithLogger(logger))

	// Register transport and store factories here. In production, the
	// concrete factories come from the adapter modules:
	//
	//   builder.RegisterTransport("mqtt", paho.NewTransportFactory(logger))
	//   builder.RegisterTransport("sqs", sqs.NewTransportFactory(logger))
	//   builder.RegisterStoreFactory("dynamodb", ddb.NewStoreFactory(client))
	//   builder.RegisterStoreFactory("memory", native.NewStoreFactory())
	//
	// The binary that imports this main package wires the specific adapters.

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

