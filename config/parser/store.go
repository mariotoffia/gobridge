package parser

import (
	"context"

	"github.com/mariotoffia/gobridge/config"
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

// Save writes cfg to Path atomically (via temp-file + rename). ctx is
// honoured for cancellation before the write begins.
func (s *FileStore) Save(ctx context.Context, cfg *ports.BridgeConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return WriteFile(s.Path, cfg)
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
