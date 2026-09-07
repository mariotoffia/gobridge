package parser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// FileStore is a ports.ConfigStore backed by a single YAML/JSON file
// on disk. Composition roots that wire the admin HTTP layer to a
// file-based config supply &FileStore{Path: cfgPath, Registry: reg}
// as httpapi.Config.ConfigStore so the admin layer can validate,
// merge, load, and save without depending on this parser package.
//
// Registry MUST be non-nil — it carries the PluginConfig decoders
// the two-stage parser uses on Load.
type FileStore struct {
	Path     string
	Registry *ports.Registry
}

var _ ports.ConfigStore = (*FileStore)(nil)

// Load returns the parsed blueprint from Path. Returns a wrapped
// fs.ErrNotExist when the file does not yet exist; the txn manager
// uses errors.Is to detect first-write semantics. ctx is honoured for
// cancellation before the (synchronous, local) read begins.
func (s *FileStore) Load(ctx context.Context) (*ports.BridgeConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return ParseFile(s.Path, FormatAuto, s.Registry)
}

// Save writes cfg atomically with the stored version plus one, then updates
// cfg.Version. The read and write are not a CAS: callers must enforce a single
// writer. ctx is honoured before filesystem work begins.
func (s *FileStore) Save(ctx context.Context, cfg *ports.BridgeConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg == nil {
		return shared.ErrInvalidConfig.WithMessage("config save: config must not be nil")
	}
	current, err := s.Load(ctx)
	var version int
	switch {
	case err == nil:
		version = current.Version
	case errors.Is(err, fs.ErrNotExist):
	default:
		return fmt.Errorf("config save: read current version: %w", err)
	}
	if version < 0 || version == math.MaxInt {
		return shared.ErrInvalidConfig.WithMessage("config save: stored version cannot be incremented")
	}
	next := *cfg
	next.Version = version + 1
	if err := WriteFile(s.Path, &next); err != nil {
		return err
	}
	cfg.Version = next.Version
	return nil
}

// Validate runs the in-process validator against cfg.
func (s *FileStore) Validate(ctx context.Context, cfg *ports.BridgeConfig) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return config.ValidateWithWarnings(cfg)
}

// Merge combines an overlay on top of base. Inputs are not mutated.
func (s *FileStore) Merge(ctx context.Context, base, overlay *ports.BridgeConfig) (*ports.BridgeConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return config.DefaultMerge(base, overlay)
}
