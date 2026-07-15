package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/config/parser"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
)

// maxBootstrapFileSize limits the bootstrap config file to 1 MiB to prevent
// accidental or malicious memory exhaustion.
const maxBootstrapFileSize = 1 << 20

const (
	EnvBootstrapJSON = "GOBRIDGE_FILEBASED_BOOTSTRAP_JSON"
	EnvBootstrapFile = "GOBRIDGE_FILEBASED_BOOTSTRAP_FILE"
)

func LoadBootstrapConfigFromEnv() (deployinfra.BootstrapConfig, error) {
	if inline := strings.TrimSpace(os.Getenv(EnvBootstrapJSON)); inline != "" {
		return LoadBootstrapConfigJSON([]byte(inline))
	}

	path := strings.TrimSpace(os.Getenv(EnvBootstrapFile))
	if path == "" {
		return deployinfra.BootstrapConfig{}, fmt.Errorf("bootstrap: neither %s nor %s is set", EnvBootstrapJSON, EnvBootstrapFile)
	}

	return LoadBootstrapConfigFile(path)
}

func LoadBootstrapConfigFile(path string) (deployinfra.BootstrapConfig, error) {
	data, err := readBoundedFile(path, maxBootstrapFileSize)
	if err != nil {
		return deployinfra.BootstrapConfig{}, fmt.Errorf("bootstrap: read %s: %w", path, err)
	}
	return LoadBootstrapConfigJSON(data)
}

func readBoundedFile(path string, maxSize int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Size() > maxSize {
		return nil, fmt.Errorf("file %s exceeds maximum size (%d > %d bytes)", path, info.Size(), maxSize)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}

func LoadBootstrapConfigJSON(data []byte) (deployinfra.BootstrapConfig, error) {
	var cfg deployinfra.BootstrapConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return deployinfra.BootstrapConfig{}, fmt.Errorf("bootstrap: decode bootstrap config: %w", err)
	}
	cfg = cfg.Normalized()
	if err := cfg.Validate(); err != nil {
		return deployinfra.BootstrapConfig{}, err
	}
	return cfg, nil
}

type optionalFileSource struct {
	path     string
	registry *ports.Registry
	fallback func() *ports.BridgeConfig
	logger   *slog.Logger
}

func newOptionalFileSource(path string, registry *ports.Registry, logger *slog.Logger, fallback func() *ports.BridgeConfig) ports.Loader {
	return &optionalFileSource{path: path, registry: registry, fallback: fallback, logger: logger}
}

func (s *optionalFileSource) Load(_ context.Context) (*ports.BridgeConfig, error) {
	cfg, err := parser.ParseFile(s.path, parser.FormatAuto, s.registry)
	if err == nil {
		return cfg, nil
	}
	if errors.Is(err, os.ErrNotExist) || os.IsNotExist(err) {
		// Loud fallback: a missing config file is almost always a
		// misconfiguration (config_file_path not pointing at the seeded EFS
		// target). The process would otherwise start, pass /health, and
		// bridge nothing — so warn on every fallback rather than degrade
		// silently.
		if s.logger != nil {
			s.logger.Warn(
				"bootstrap: config file not found; falling back to empty default config "+
					"(no routes will be bridged) — verify config_file_path matches the seeder EFS target",
				"config_file_path", s.path,
			)
		}
		return s.fallback(), nil
	}
	return nil, err
}

func newPollWatcher(ctx context.Context, cfg deployinfra.BootstrapConfig, registry *ports.Registry, logger *slog.Logger) ports.Watcher {
	var opts []fileconfig.WatcherOption
	opts = append(opts,
		fileconfig.WithMode(fileconfig.ModePoll),
		fileconfig.WithPollInterval(cfg.EffectivePollInterval()),
	)
	if logger != nil {
		opts = append(opts, fileconfig.WithLogger(logger))
	}
	// Baseline the watcher's change detection from the config content as of now
	// (before Watch starts), closing the window where an edit landing between
	// the initial Load and Watch -- which here spans the runtime build and the
	// transport/admin server starts -- would be absorbed into a Watch-time disk
	// re-read and never emitted, silently running stale config. A throwaway
	// file.Source computes the baseline via the same sha256(file-bytes) the
	// watcher compares against, without disturbing the optionalFileSource
	// fallback loader. An absent or unparseable file records no hash, so the
	// watcher keeps its disk-read baseline (unchanged behavior).
	if h, ok := baselineHash(ctx, cfg.ConfigFilePath, registry); ok {
		opts = append(opts, fileconfig.WithBaselineHash(h))
	}
	return fileconfig.NewWatcher(cfg.ConfigFilePath, registry, opts...)
}

// baselineHash returns the sha256 content hash the poll watcher should adopt as
// its change-detection baseline, by loading the file through a dedicated
// file.Source. The second return is false when the file is missing or does not
// parse, in which case the watcher falls back to its own Watch-time disk read.
func baselineHash(ctx context.Context, path string, registry *ports.Registry) ([sha256.Size]byte, bool) {
	src := fileconfig.NewSource(path, registry)
	if _, err := src.Load(ctx); err != nil {
		return [sha256.Size]byte{}, false
	}
	return src.LoadHash()
}

func defaultLogicalConfig(cfg deployinfra.BootstrapConfig) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:              cfg.BridgeID,
			DeploymentMode:  "standalone",
			ShutdownTimeout: "30s",
			DrainTimeout:    "30s",
		},
	}
}

// applyLogLevel updates the wired slog.LevelVar from bridge.log_level so a
// hot-reloaded config can raise/lower log verbosity without a redeploy. It is
// a no-op when no LevelVar was wired or when log_level is empty/unknown
// (unknown values keep the current level rather than silently defaulting).
func (a *App) applyLogLevel(logical *ports.BridgeConfig) {
	if a.logLevelVar == nil || logical == nil {
		return
	}
	lvl, ok := parseLogLevel(logical.Bridge.LogLevel)
	if !ok {
		return
	}
	if a.logLevelVar.Level() != lvl {
		a.logLevelVar.Set(lvl)
		a.logger.Info("bootstrap: applied bridge.log_level", "level", logical.Bridge.LogLevel)
	}
}

// parseLogLevel maps a bridge.log_level string to a slog.Level. The bool is
// false for empty or unrecognized input so callers can leave the level
// unchanged.
func parseLogLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// newDefaultPluginRegistry returns a *ports.Registry populated with
// the PluginConfig decoders for the adapters this binary bundles.
// Adding a new adapter to the file-based-config deployment means
// adding its Register call here. Registration errors are surfaced
// as panics: a duplicate kind in the bundled set is a programming
// error that must be caught at process start.
func newDefaultPluginRegistry() *ports.Registry {
	reg := ports.NewRegistry()
	if err := errors.Join(
		paho.Register(reg),
		sqsadapter.Register(reg),
		nativestore.Register(reg),
		awsstore.Register(reg),
		httptransport.Register(reg),
	); err != nil {
		panic("bootstrap: register bundled plugin decoders: " + err.Error())
	}
	return reg
}

func cloneBridgeConfig(cfg *ports.BridgeConfig, registry *ports.Registry) (*ports.BridgeConfig, error) {
	if cfg == nil {
		return nil, nil
	}
	data, err := parser.MarshalYAML(cfg)
	if err != nil {
		return nil, err
	}
	return parser.Parse(bytes.NewReader(data), parser.FormatYAML, registry)
}

func hasHTTPTransportEndpoints(cfg *ports.BridgeConfig) bool {
	if cfg == nil {
		return false
	}
	for _, recv := range cfg.Receivers {
		if recv.Transport == "http" {
			return true
		}
	}
	for _, sender := range cfg.Senders {
		if sender.Transport == "http" {
			return true
		}
	}
	return false
}

func validateFilesystemProfile(cfg deployinfra.BootstrapConfig, logical *ports.BridgeConfig) error {
	if logical == nil {
		return fmt.Errorf("bootstrap: logical config is nil")
	}

	if cfg.Topology != deployinfra.TopologyFilesystemReplicated {
		return nil
	}

	// Under the filesystem_replicated topology, features that require
	// distributed coordination are not supported: the file-based EFS profile
	// provisions no distributed lease/outbox store (e.g. DynamoDB), and
	// SQLite-over-EFS cannot serialize cross-instance writers safely.
	// Clustered deployment_mode itself is allowed; only features that need
	// cross-instance state are restricted.
	for _, route := range logical.Routes {
		if route.DeliveryMode == "shared_outbox" {
			return fmt.Errorf("bootstrap: route %q uses shared_outbox, which requires a distributed outbox store (e.g. DynamoDB) that the file-based EFS profile does not provision", route.ID)
		}
		if route.Session != nil {
			return fmt.Errorf("bootstrap: route %q uses route.session lease coordination, which requires a distributed lease store (e.g. DynamoDB) that the file-based EFS profile does not provision", route.ID)
		}
	}

	return nil
}

func validateDeploymentProfile(cfg deployinfra.BootstrapConfig, logical *ports.BridgeConfig) error {
	switch cfg.Topology {
	case deployinfra.TopologyFilesystemReplicated:
		return validateFilesystemProfile(cfg, logical)
	case deployinfra.TopologyDynamoDBCoordinatedHA:
		return validateDynamoDBHAProfile(cfg, logical)
	default:
		return nil
	}
}

func validateDynamoDBHAProfile(cfg deployinfra.BootstrapConfig, logical *ports.BridgeConfig) error {
	if logical == nil {
		return fmt.Errorf("bootstrap: dynamodb_coordinated_ha logical config is nil")
	}
	if logical.Bridge.DeploymentMode != "clustered" {
		return fmt.Errorf("bootstrap: dynamodb_coordinated_ha requires bridge.deployment_mode=clustered")
	}
	if logical.Bridge.Cluster != nil && len(logical.Bridge.Cluster.Endpoints) > 0 {
		return fmt.Errorf("bootstrap: dynamodb_coordinated_ha requires ECS-resolved endpoints; static bridge.cluster.endpoints is forbidden")
	}
	stores := []struct {
		role     string
		store    *ports.StoreConfig
		expected string
	}{
		{role: "lease", store: logical.Stores.Lease, expected: cfg.DynamoDBHALeaseTableName},
		{role: "outbox", store: logical.Stores.Outbox, expected: cfg.DynamoDBHAOutboxTableName},
		{role: "managed_subscriptions", store: logical.Stores.ManagedSubscriptions, expected: cfg.DynamoDBHAManagedSubscriptionsTableName},
	}
	for _, item := range stores {
		actual, err := dynamoDBHAStoreTable(item.role, item.store)
		if err != nil {
			return err
		}
		if actual != item.expected {
			return fmt.Errorf("bootstrap: dynamodb_coordinated_ha stores.%s table %q does not match deployment-owned expected table %q", item.role, actual, item.expected)
		}
	}

	fingerprint, err := configFingerprint(logical)
	if err != nil {
		return err
	}
	if fingerprint != cfg.DynamoDBHAConfigFingerprint {
		return fmt.Errorf("bootstrap: dynamodb_coordinated_ha logical config fingerprint does not match the deployment-owned admitted config")
	}
	return nil
}

func dynamoDBHAStoreTable(role string, store *ports.StoreConfig) (string, error) {
	if store == nil || store.Type != awsstore.DynamoDBKind {
		return "", fmt.Errorf("bootstrap: dynamodb_coordinated_ha stores.%s must use type=dynamodb", role)
	}
	config, ok := store.Config.(*awsstore.DynamoDBConfig)
	if !ok || config == nil {
		return "", fmt.Errorf("bootstrap: dynamodb_coordinated_ha stores.%s has incompatible DynamoDB config", role)
	}
	name, err := awsstore.ResolveDynamoDBTableName(role, config.TableName)
	if err != nil {
		return "", fmt.Errorf("bootstrap: resolve dynamodb_coordinated_ha stores.%s table: %w", role, err)
	}
	return name, nil
}
