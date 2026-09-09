package parser

import (
	"errors"
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
	_, err := writeFile(path, cfg, false)
	return err
}

func writeFile(path string, cfg *ports.BridgeConfig, createOnly bool) (created bool, err error) {
	var data []byte
	if createOnly && detectFormat(path) == FormatJSON {
		data, err = MarshalBridgeConfigJSON(cfg)
	} else {
		data, err = MarshalYAML(cfg)
	}
	if err != nil {
		return false, err
	}
	if createOnly && len(data) > MaxConfigBytes {
		return false, fmt.Errorf("config: serialized config exceeds maximum size of %d bytes", MaxConfigBytes)
	}

	dir := filepath.Dir(path)

	// New config files are created 0600, not world-readable 0644: a config can
	// embed secrets (HTTP admin/monitor API keys, plugin credentials). An
	// existing file keeps its current permissions so an operator-tightened or
	// deployment-managed mode is not clobbered.
	perm := os.FileMode(0600)
	if info, err := os.Stat(path); !createOnly && err == nil {
		perm = info.Mode().Perm()
	}

	f, err := os.CreateTemp(dir, ".gobridge-config-*.yaml.tmp")
	if err != nil {
		return false, fmt.Errorf("config: create temp file in %s: %w", dir, err)
	}
	tmpPath := f.Name()

	// Cleanup on any error after temp file creation.
	ok := false
	defer func() {
		if !ok {
			if closeErr := f.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
				err = errors.Join(err, fmt.Errorf("config: close temporary file: %w", closeErr))
			}
			if removeErr := os.Remove(tmpPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("config: remove temporary file: %w", removeErr))
			}
		}
	}()

	if _, err := f.Write(data); err != nil {
		return false, fmt.Errorf("config: write temp file: %w", err)
	}
	if err := f.Chmod(perm); err != nil {
		return false, fmt.Errorf("config: chmod temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return false, fmt.Errorf("config: sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return false, fmt.Errorf("config: close temp file: %w", err)
	}

	if createOnly {
		if err := os.Link(tmpPath, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return false, nil
			}
			return false, fmt.Errorf("config: publish without replacement: %w", err)
		}
		if err := os.Remove(tmpPath); err != nil {
			return false, fmt.Errorf("config: remove published temporary name: %w", err)
		}
	} else if err := os.Rename(tmpPath, path); err != nil {
		return false, fmt.Errorf("config: rename %s -> %s: %w", tmpPath, path, err)
	}

	// fsync the parent directory so the rename (a directory-entry change) is
	// durable. Without it a crash right after the rename can leave the new
	// directory entry unpersisted and lose the just-committed config even
	// though the file data itself was fsynced above.
	if err := syncDir(dir); err != nil {
		return false, fmt.Errorf("config: sync dir %s: %w", dir, err)
	}

	ok = true
	return true, nil
}

// syncDir fsyncs a directory so a rename into it is durable across a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync dir: %w", err)
	}
	return nil
}
