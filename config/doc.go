// Package config is the inner-ring shared kernel for GoBridge
// configuration. It carries the pure (vendor-free) behaviour over
// *ports.BridgeConfig that every consumer of a parsed configuration
// needs:
//
//   - structural validation (Validate / ValidateWithWarnings);
//   - layered merge of overlay configs on top of a base
//     (DefaultMerge);
//   - layered loading and watching coordination across multiple
//     sources (Manager / Layer).
//
// The blueprint TYPE definitions themselves live in `ports/`. The
// YAML / JSON parser, the wire-format marshallers and the on-disk
// FileStore live in the sibling package `config/parser`, which is
// the only place permitted to import `gopkg.in/yaml.v3` or
// `github.com/go-viper/mapstructure/v2`. Splitting the shared
// kernel this way removes the historical inner-ring vendor
// concession (W-9): this package is now stdlib-only and fits the
// Clean-Architecture inner-ring rule without exception.
//
// Allowed importers: `config/parser` (sibling), `bridge`,
// `httpapi`, the config-source adapters (`adapters/*/config/*`)
// and the composition root (`cmd`, `deployment`).
package config
