package config

import "github.com/mariotoffia/gobridge/ports"

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
// uses errors.Is to detect first-write semantics.
func (s *FileStore) Load() (*ports.BridgeConfig, error) {
	return ParseFile(s.Path, FormatAuto, s.Registry)
}

// Save writes cfg to Path atomically (via temp-file + rename).
func (s *FileStore) Save(cfg *ports.BridgeConfig) error {
	return WriteFile(s.Path, cfg)
}

// Validate runs the in-process validator against cfg.
func (s *FileStore) Validate(cfg *ports.BridgeConfig) ([]string, error) {
	return ValidateWithWarnings(cfg)
}

// Merge combines an overlay on top of base. Inputs are not mutated.
func (s *FileStore) Merge(base, overlay *ports.BridgeConfig) (*ports.BridgeConfig, error) {
	return DefaultMerge(base, overlay)
}
