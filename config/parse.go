package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Format specifies the configuration file format.
type Format string

const (
	FormatAuto Format = "auto"
	FormatYAML Format = "yaml"
	FormatJSON Format = "json"
)

// ParseFile loads and parses a configuration file. The format is detected
// from the file extension unless overridden by format. Supported extensions:
// .yaml, .yml (YAML), .json (JSON).
func ParseFile(path string, format Format) (*BridgeConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if format == FormatAuto || format == "" {
		format = detectFormat(path)
	}

	return Parse(f, format)
}

// Parse reads configuration from r using the specified format.
func Parse(r io.Reader, format Format) (*BridgeConfig, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("config: read: %w", err)
	}

	var cfg BridgeConfig
	switch format {
	case FormatJSON:
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("config: json parse: %w", err)
		}
	case FormatYAML, FormatAuto, "":
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("config: yaml parse: %w", err)
		}
	default:
		return nil, fmt.Errorf("config: unsupported format %q", format)
	}

	return &cfg, nil
}

func detectFormat(path string) Format {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".json":
		return FormatJSON
	case ".yaml", ".yml":
		return FormatYAML
	default:
		return FormatYAML
	}
}
