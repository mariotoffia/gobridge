package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/lib/bootstrap"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/lib/model"
)

func main() {
	var bootstrapPath string
	flag.StringVar(&bootstrapPath, "bootstrap-file", "", "path to the bootstrap JSON file")
	flag.Parse()

	cfg, err := loadConfig(bootstrapPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load bootstrap config: %v\n", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	app := bootstrap.NewApp(cfg, bootstrap.WithLogger(logger))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := app.Run(ctx); err != nil {
		logger.Error("file-based bootstrap exited with error", "error", err)
		os.Exit(1)
	}
}

func loadConfig(path string) (model.BootstrapConfig, error) {
	if path != "" {
		return bootstrap.LoadBootstrapConfigFile(path)
	}
	return bootstrap.LoadBootstrapConfigFromEnv()
}
