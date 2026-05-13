package parser

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/mariotoffia/gobridge/ports"
)

// MarshalYAML serialises a ports.BridgeConfig to YAML bytes, projecting
// each typed PluginConfig into the canonical `options` map. See
// blueprint_marshal.go for the rationale.
func MarshalYAML(cfg *ports.BridgeConfig) ([]byte, error) {
	data, err := marshalBridgeConfigYAML(cfg)
	if err != nil {
		return nil, fmt.Errorf("config: marshal yaml: %w", err)
	}
	return data, nil
}

// WriteFile atomically writes a ports.BridgeConfig to the given path as YAML.
// It preserves the original file's permissions when the file already exists.
// The write uses a temporary file in the same directory followed by an
// atomic rename, so readers never see a partially written file.
func WriteFile(path string, cfg *ports.BridgeConfig) error {
	data, err := MarshalYAML(cfg)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)

	// Capture existing file permissions (default 0644 for new files).
	perm := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}

	f, err := os.CreateTemp(dir, ".gobridge-config-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("config: create temp file in %s: %w", dir, err)
	}
	tmpPath := f.Name()

	// Cleanup on any error after temp file creation.
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("config: write temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("config: sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("config: close temp file: %w", err)
	}

	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("config: chmod temp file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("config: rename %s -> %s: %w", tmpPath, path, err)
	}

	ok = true
	return nil
}
