package parser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"

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

// Load returns the parsed blueprint from Path. Only an absent final entry with
// an existing parent maps to shared.ErrNotFound (wrapping fs.ErrNotExist).
// Missing parents and dangling symlinks are not classified as document absence.
// ctx is honoured before the synchronous filesystem read begins.
func (s *FileStore) Load(ctx context.Context) (*ports.BridgeConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, err := ParseFile(s.Path, FormatAuto, s.Registry)
	if errors.Is(err, fs.ErrNotExist) {
		_, entryErr := os.Lstat(s.Path)
		if _, parentErr := os.Stat(filepath.Dir(s.Path)); parentErr == nil && errors.Is(entryErr, fs.ErrNotExist) {
			return nil, shared.ErrNotFound.WithMessage("config document missing").Wrap(err)
		}
	}
	if errors.Is(err, shared.ErrNotFound) {
		return nil, shared.ErrInvalidConfig.WithMessage("config document could not be decoded").Wrap(err)
	}
	return cfg, err
}

// CreateIfAbsent publishes a complete version-1 document using a same-filesystem
// hard link. The filesystem must support atomic no-clobber links and directory
// sync; errors never fall back to an overwriting rename. cfg is not retained.
func (s *FileStore) CreateIfAbsent(ctx context.Context, cfg *ports.BridgeConfig) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if cfg == nil {
		return false, shared.ErrInvalidConfig
	}
	next := *cfg
	next.Version = 1
	created, err := writeFile(s.Path, &next, true)
	if created && err == nil {
		cfg.Version = 1
	}
	return created, err
}

var _ ports.ConfigInitializer = (*FileStore)(nil)

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
