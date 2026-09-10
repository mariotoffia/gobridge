// Package parser is the wire-format adapter for the GoBridge
// configuration shared kernel. It owns:
//
//   - YAML / JSON parsing (Parse, ParseFile) using a two-stage
//     decode that routes each plugin attachment point's `options`
//     block through a *ports.Registry;
//   - typed RawConfig.Decode based on
//     `github.com/go-viper/mapstructure/v2`;
//   - wire-format marshalling (MarshalYAML,
//     MarshalBridgeConfigJSON) that re-projects each typed
//     PluginConfig into the canonical `options` map;
//   - atomic on-disk writes (WriteFile) and the FileStore
//     ports.ConfigStore implementation that combines load / save /
//     validate / merge for file-backed config.
//
// This package is the only Layer-2 location permitted to import
// `gopkg.in/yaml.v3` and `github.com/go-viper/mapstructure/v2`.
// They used to sit in the pure model package
// `github.com/mariotoffia/gobridge/config`, which made the inner ring
// carry a vendor dependency. Moving them here keeps that package
// stdlib-only: the model stays free of any wire format, and every
// yaml/mapstructure decision lives behind this one boundary.
//
// Allowed importers: the config-source adapters
// (`adapters/native/config/file`, `adapters/aws/config/dynamodb`)
// and the composition root (`cmd`, `deployment`).
//
// Validation / merge / Manager continue to live in `config/`; this
// package depends on `config/` for FileStore.Validate /
// FileStore.Merge.
package parser
