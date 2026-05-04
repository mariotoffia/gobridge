package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	"github.com/mariotoffia/gobridge/config"
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
	fallback func() *ports.BridgeConfig
}

func newOptionalFileSource(path string, fallback func() *ports.BridgeConfig) ports.Loader {
	return &optionalFileSource{path: path, fallback: fallback}
}

func (s *optionalFileSource) Load(_ context.Context) (*ports.BridgeConfig, error) {
	cfg, err := config.ParseFile(s.path, config.FormatAuto)
	if err == nil {
		return cfg, nil
	}
	if errors.Is(err, os.ErrNotExist) || os.IsNotExist(err) {
		return s.fallback(), nil
	}
	return nil, err
}

func newPollWatcher(cfg deployinfra.BootstrapConfig, logger *slog.Logger) ports.Watcher {
	var opts []fileconfig.WatcherOption
	opts = append(opts,
		fileconfig.WithMode(fileconfig.ModePoll),
		fileconfig.WithPollInterval(cfg.EffectivePollInterval()),
	)
	if logger != nil {
		opts = append(opts, fileconfig.WithLogger(logger))
	}
	return fileconfig.NewWatcher(cfg.ConfigFilePath, opts...)
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

func cloneBridgeConfig(cfg *ports.BridgeConfig) (*ports.BridgeConfig, error) {
	if cfg == nil {
		return nil, nil
	}
	data, err := config.MarshalYAML(cfg)
	if err != nil {
		return nil, err
	}
	return config.Parse(bytes.NewReader(data), config.FormatYAML)
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
	// distributed coordination are not supported — use the HA/DynamoDB
	// config profile instead. Clustered deployment_mode itself is allowed;
	// only features that need cross-instance state are restricted.
	for _, route := range logical.Routes {
		if route.DeliveryMode == "shared_outbox" {
			return fmt.Errorf("bootstrap: route %q uses shared_outbox, which requires the HA/DynamoDB profile", route.ID)
		}
		if route.Session != nil {
			return fmt.Errorf("bootstrap: route %q uses route.session lease coordination, which requires the HA/DynamoDB profile", route.ID)
		}
	}

	return nil
}
