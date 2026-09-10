package bootstrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/bridge"
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

// applyLogLevel updates the wired slog.LevelVar from bridge.log_level so a
// hot-reloaded config can raise/lower log verbosity without a redeploy. It is
// a no-op when no LevelVar was wired or when log_level is empty/unknown
// (unknown values keep the current level rather than silently defaulting).
func (a *App) applyLogLevel(logical *ports.BridgeConfig) {
	if a.logLevelVar == nil || logical == nil {
		return
	}
	lvl, ok := ports.ParseLogLevel(logical.Bridge.LogLevel)
	if !ok {
		return
	}
	if a.logLevelVar.Level() != lvl {
		a.logLevelVar.Set(lvl)
		a.logger.Info("bootstrap: applied bridge.log_level", "level", logical.Bridge.LogLevel)
	}
}

// newDefaultPluginRegistry returns a *ports.Registry populated with
// the PluginConfig decoders for the adapters this binary bundles.
// Adding a new adapter to the file-based-config deployment means
// adding its Register call here; the optional families selected by
// build tag add theirs through registerOptionalDecoders. Registration
// errors are surfaced as panics: a duplicate kind in the bundled set
// is a programming error that must be caught at process start.
func newDefaultPluginRegistry() *ports.Registry {
	reg := ports.NewRegistry()
	if err := errors.Join(
		paho.Register(reg),
		sqsadapter.Register(reg),
		nativestore.Register(reg),
		awsstore.Register(reg),
		httptransport.Register(reg),
		registerOptionalDecoders(reg),
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

	// The IMMUTABLE deployment profile only (bridge.DeploymentProfileFingerprint):
	// topology, cohort shape and the deployment-owned store identities. Hashing the
	// whole logical config here used to reject every real config change after the
	// cohort committed it, because an operator changing a route legitimately makes
	// the running document differ from the one synth admitted. Operator content is
	// gated by config.Validate and the reload preflight, not by this check.
	if fingerprint := bridge.DeploymentProfileFingerprint(logical); fingerprint != cfg.DynamoDBHAConfigFingerprint {
		return fmt.Errorf("bootstrap: dynamodb_coordinated_ha logical config does not match the " +
			"deployment profile this deployment admitted (deployment_mode, bridge.cluster shape, or a " +
			"deployment-owned store identity was changed); those fields are provisioned by the deployment " +
			"and can only change through a redeploy")
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
