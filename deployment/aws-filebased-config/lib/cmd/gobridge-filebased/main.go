package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/lib/bootstrap"
)

// Build metadata, injected via -ldflags "-X main.version=... -X main.gitSHA=..."
// by the release pipeline. Defaults keep local builds honest.
//
//nolint:gochecknoglobals // -ldflags -X can only target package-level vars
var (
	version = "dev"
	gitSHA  = "unknown"
)

func main() {
	var bootstrapPath string
	var healthcheck bool
	flag.StringVar(&bootstrapPath, "bootstrap-file", "", "path to the bootstrap JSON file")
	flag.BoolVar(&healthcheck, "healthcheck", false, "probe the local monitor liveness endpoint and exit 0 (alive) or 1 (unavailable); used as the container HEALTHCHECK")
	flag.Parse()

	// Health check runs as its own short-lived process invocation (the
	// container HEALTHCHECK), so handle it before the mandatory config load:
	// it only needs the monitor port and tolerates a missing/invalid bootstrap
	// by falling back to the default monitor address.
	if healthcheck {
		os.Exit(runHealthcheck(bootstrapPath))
	}

	cfg, err := loadConfig(bootstrapPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load bootstrap config: %v\n", err)
		os.Exit(1)
	}

	// A single slog.LevelVar backs the handler AND is wired into the App so
	// bridge.log_level from the reloaded bridge config changes verbosity at
	// runtime (hot reload) without a redeploy.
	levelVar := new(slog.LevelVar)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: levelVar}))
	logger.Info("gobridge-filebased starting", "version", version, "git_sha", gitSHA)
	app := bootstrap.NewApp(cfg,
		bootstrap.WithLogger(logger),
		bootstrap.WithLogLevelVar(levelVar),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := app.Run(ctx); err != nil {
		logger.Error("file-based bootstrap exited with error", "error", err)
		os.Exit(1)
	}
}

func loadConfig(path string) (infra.BootstrapConfig, error) {
	if path != "" {
		return bootstrap.LoadBootstrapConfigFile(path)
	}
	return bootstrap.LoadBootstrapConfigFromEnv()
}

// healthcheckPath is the unauthenticated liveness probe served by the monitor
// server (see httpapi/monitor.go). It returns 200 while the process can still
// recover and 503 once the runtime is terminal.
const healthcheckPath = "/api/v1/monitor/live"

// runHealthcheck performs a localhost GET against the monitor liveness probe
// and returns a process exit code: 0 when the endpoint returns 200, 1
// otherwise. It loads the bootstrap config best-effort for the monitor port
// (falling back to infra.DefaultMonitorAddr) so the probe needs no extra
// tooling in the container image — the container HEALTHCHECK invokes this same
// static binary (no curl/wget).
func runHealthcheck(bootstrapPath string) int {
	addr := infra.DefaultMonitorAddr
	if cfg, err := loadConfig(bootstrapPath); err == nil {
		cfg = cfg.Normalized()
		if cfg.MonitorAddr != "" {
			addr = cfg.MonitorAddr
		}
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		fmt.Fprintf(os.Stderr, "healthcheck: cannot derive monitor port from %q: %v\n", addr, err)
		return 1
	}

	url := "http://127.0.0.1:" + port + healthcheckPath
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: build request: %v\n", err)
		return 1
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: GET %s: %v\n", url, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s returned %d\n", url, resp.StatusCode)
		return 1
	}
	return 0
}
