package file

import (
	"context"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
)

// Source implements ports.Loader by reading a configuration file from disk.
type Source struct {
	path     string
	format   parser.Format
	registry *ports.Registry
}

// SourceOption configures a Source.
type SourceOption func(*Source)

// WithSourceFormat overrides format auto-detection for the file source.
func WithSourceFormat(f parser.Format) SourceOption {
	return func(s *Source) { s.format = f }
}

// NewSource creates a file-backed config source for the given path
// and plugin registry. registry MUST be non-nil — it carries the
// PluginConfig decoders the two-stage parser needs. Composition
// roots build the registry by calling each adapter's exported
// Register function.
func NewSource(path string, registry *ports.Registry, opts ...SourceOption) *Source {
	s := &Source{
		path:     path,
		format:   parser.FormatAuto,
		registry: registry,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Load reads and parses the configuration file.
func (s *Source) Load(_ context.Context) (*ports.BridgeConfig, error) {
	return parser.ParseFile(s.path, s.format, s.registry)
}
