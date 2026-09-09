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
	_ "embed"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	goruntime "github.com/mariotoffia/gobridge/runtime"

	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	filecreds "github.com/mariotoffia/gobridge/adapters/native/credentials/file"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Build metadata is injected via -ldflags "-X main.version=... -X main.gitSHA=...".
//
//nolint:gochecknoglobals // Linker stamps require package-level string variables.
var version, gitSHA string

//go:embed initial-config.base64
var initialConfigBase64 string

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
Compiled plugins: %s
Select plugin families at build time with -tags gobridge_<family>; see PLUGIN.md.
Configs naming a transport or store not compiled in are rejected.
The file config source, file:// credentials and admin/monitor HTTP API remain available.

Usage of %s:
`, pluginSummary(), os.Args[0])
		flag.PrintDefaults()
	}
	showInitialDigest := flag.Bool("initial-config-digest", false, "print the SHA-256 of the exact embedded initial configuration bytes and exit without startup")
	showVersion := flag.Bool("version", false, "print version and compiled plugin families, then exit")
	configPath := flag.String("config", "bridge.yaml", "path to configuration file")
	logLevel := flag.String("log-level", "info", "log level ("+strings.Join(ports.LogLevelNames(), ", ")+")")
	credentialsDir := flag.String("credentials-dir", "credentials",
		"base directory backing file:// credential URIs (native file credential store)")
	_ = flag.Bool("start-empty", true, "deprecated: absent configuration waits without a data-plane runtime")
	adminAddr := flag.String("admin-addr", "", "process-owned admin listener; requires GOBRIDGE_ADMIN_API_KEY; otherwise use boot http settings")
	monitorAddr := flag.String("monitor-addr", defaultMonitorAddr, "process-owned monitor listener")
	tlsCert := flag.String("http-tls-cert", "", "process-owned HTTP TLS certificate")
	tlsKey := flag.String("http-tls-key", "", "process-owned HTTP TLS private key")
	var seedBaselines repeatableFlag
	flag.Var(&seedBaselines, "seed-managed-subscriptions",
		"seed the managed-subscription baseline of a persistent/exclusive MQTT session and exit: "+
			"`session-id` attests a NEW broker identity with no subscriptions, "+
			"`session-id=filter,filter` records the exact filters the existing broker session holds; repeatable")
	flag.Parse()
	if *showInitialDigest {
		return writeInitialConfigDigest(os.Stdout, os.Stderr, initialConfigBase64)
	}

	if *showVersion {
		if _, err := fmt.Fprintln(os.Stdout, versionLine()); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "write version: %v\n", err)
			return 1
		}
		return 0
	}

	logger := newLogger(*logLevel)

	// Register only the decoders selected by the compiled plugin families.
	reg := ports.NewRegistry()
	if err := registerAllDecoders(reg); err != nil {
		logger.Error("failed to register plugin decoders", "error", err)
		return 1
	}
	logStartup(logger, reg)

	fileSource := fileconfig.NewSource(*configPath, reg)
	loader := fileSource

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

	initial, err := base64.StdEncoding.DecodeString(initialConfigBase64)
	if err != nil {
		logger.Error("invalid embedded initial configuration encoding")
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	control := ports.HTTPConfig{AdminAddr: *adminAddr, MonitorAddr: *monitorAddr, TLSCertFile: *tlsCert, TLSKeyFile: *tlsKey}
	if control.AdminAddr == "" {
		// Legacy listener settings require reading the boot file. Explicit
		// process settings avoid any repository I/O before listeners are up.
		boot, loadErr := loader.Load(ctx)
		if loadErr == nil && boot.HTTP != nil {
			control = *boot.HTTP
		}
		if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) && !errors.Is(loadErr, shared.ErrNotFound) {
			logger.Error("cannot read boot HTTP settings; use -admin-addr for repository-independent startup")
			return 1
		}
	}
	control.AdminAddr = orDefault(control.AdminAddr, defaultAdminAddr)
	control.MonitorAddr = orDefault(control.MonitorAddr, defaultMonitorAddr)
	if err := runObservedConfig(ctx, *configPath, *credentialsDir, reg, control, string(initial), logger, clock.System); err != nil {
		logger.Error("bridge stopped", "error", err)
		return 1
	}
	return 0
}

func logStartup(logger *slog.Logger, reg *ports.Registry) {
	kinds := reg.Kinds()
	logger.Info(versionLine(), "kinds", kinds)
	if len(compiledFamilies) == 0 && len(kinds) == 0 {
		logger.Warn("no transports or stores linked; every routed config will be rejected; " +
			"rebuild with -tags gobridge_<family>")
	}
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
