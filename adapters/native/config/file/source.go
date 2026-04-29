package file

import (
	"context"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
)

// Source implements ports.Loader by reading a configuration file from disk.
type Source struct {
	path   string
	format config.Format
}

// SourceOption configures a Source.
type SourceOption func(*Source)

// WithSourceFormat overrides format auto-detection for the file source.
func WithSourceFormat(f config.Format) SourceOption {
	return func(s *Source) { s.format = f }
}

// NewSource creates a file-backed config source for the given path.
func NewSource(path string, opts ...SourceOption) *Source {
	s := &Source{
		path:   path,
		format: config.FormatAuto,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Load reads and parses the configuration file.
func (s *Source) Load(_ context.Context) (*ports.BridgeConfig, error) {
	return config.ParseFile(s.path, s.format)
}
