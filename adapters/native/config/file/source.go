package file

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Loader = (*Source)(nil)

// Source implements ports.Loader by reading a configuration file from disk.
type Source struct {
	path     string
	format   parser.Format
	registry *ports.Registry

	// hash is the sha256 of the exact bytes parsed by the most recent
	// successful Load; hasHash reports whether a Load has ever succeeded. A
	// watcher baselines its change detection from this via WithBaselineHash so
	// an edit between Load and Watch is not silently absorbed. Guarded because
	// Load and LoadHash may be called from different goroutines.
	mu      sync.Mutex
	hash    [sha256.Size]byte
	hasHash bool
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

// Load reads and parses the configuration file. A cancelled ctx short-circuits
// before any filesystem work (I6); a missing file maps to shared.ErrNotFound
// so callers can classify it, while parse errors pass through from the parser.
//
// Load reads the file itself (rather than delegating to parser.ParseFile) so it
// can capture the content hash of the exact bytes it parsed. A watcher can then
// baseline from that hash via WithBaselineHash, closing the Load↔Watch window
// where a between-the-two edit is absorbed into the baseline and never emitted.
func (s *Source) Load(ctx context.Context) (*ports.BridgeConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, shared.ErrNotFound.WithMessage("config file not found").Wrap(err)
		}
		return nil, fmt.Errorf("config: read %s: %w", s.path, err)
	}

	format := s.format
	if format == parser.FormatAuto || format == "" {
		format = detectSourceFormat(s.path)
	}

	cfg, err := parser.Parse(bytes.NewReader(data), format, s.registry)
	if err != nil {
		return nil, err //nolint:wrapcheck // parser already annotates with stage context.
	}

	sum := sha256.Sum256(data)
	s.mu.Lock()
	s.hash = sum
	s.hasHash = true
	s.mu.Unlock()

	return cfg, nil
}

// LoadHash returns the sha256 content hash of the bytes parsed by the most
// recent successful Load, and whether a Load has succeeded. Passing this to a
// watcher's WithBaselineHash makes the watcher's change-detection baseline the
// exact content the caller loaded, not a possibly-newer disk read taken at
// Watch time.
func (s *Source) LoadHash() ([sha256.Size]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hash, s.hasHash
}

// detectSourceFormat mirrors the parser's unexported extension detection:
// .json → JSON; .yaml/.yml and everything else → YAML.
func detectSourceFormat(path string) parser.Format {
	if strings.ToLower(filepath.Ext(path)) == ".json" {
		return parser.FormatJSON
	}
	return parser.FormatYAML
}
