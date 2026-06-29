package parser

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Wire-format marshalling for *ports.BridgeConfig that projects each
// typed PluginConfig back into the canonical `options` map.
//
// The blueprint TYPES live in `ports/`. The Config field on
// SessionDef/ReceiverDef/SenderDef/SubscriptionDef/BindingDef/StoreConfig
// is tagged `json:"-" yaml:"-"` so reflection-based marshalling drops it.
// That is correct for the parser's wire format (which uses an `options:`
// map) but it loses data when callers round-trip a fully decoded
// *BridgeConfig through JSON or YAML (e.g. the DynamoDB config loader's
// Save → DynamoDB → Load path, the file-config WriteFile path, or admin
// tooling that re-emits a parsed config).
//
// The functions below project Config back into the canonical `options`
// key alongside the inner ring fields. They live in `config/` (not in
// `ports/`) because the inner ring is dependency-neutral, not tag-free
// (M-11): ports has no
// runtime dependency on yaml.v3 or encoding/json. Adapters that need to
// emit a wire-format BridgeConfig (file store, DynamoDB store, admin
// tooling) call these helpers explicitly via the config shared kernel.
//
// Unmarshal is not provided here: stage-1 parsing is owned by parse.go
// via wire types that already understand the `options` field.

// MarshalBridgeConfigJSON serialises cfg to JSON, projecting each typed
// PluginConfig into the canonical `options` map. Callers that need the
// wire-format JSON of a fully decoded BridgeConfig MUST use this rather
// than json.Marshal directly.
func MarshalBridgeConfigJSON(cfg *ports.BridgeConfig) ([]byte, error) {
	if cfg == nil {
		out, err := json.Marshal(nil)
		if err != nil {
			return nil, fmt.Errorf("config: marshal bridge config json: %w", err)
		}
		return out, nil
	}
	m, err := bridgeConfigToWireMap(cfg, jsonCodec{})
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("config: marshal bridge config json: %w", err)
	}
	return out, nil
}

// marshalBridgeConfigYAML serialises cfg to YAML with the same options
// projection. Exposed via MarshalYAML in write.go.
func marshalBridgeConfigYAML(cfg *ports.BridgeConfig) ([]byte, error) {
	if cfg == nil {
		out, err := yaml.Marshal(nil)
		if err != nil {
			return nil, fmt.Errorf("config: marshal bridge config yaml: %w", err)
		}
		return out, nil
	}
	m, err := bridgeConfigToWireMap(cfg, yamlCodec{})
	if err != nil {
		return nil, err
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("config: marshal bridge config yaml: %w", err)
	}
	return out, nil
}

// codec abstracts the marshal/unmarshal pair so the projection walker
// can be shared between JSON and YAML output. JSON honours `json:"..."`
// struct tags, YAML honours `yaml:"..."` -- the `options` key is
// lower-case in both wire formats so the walker is identical once the
// initial Marshal-then-Unmarshal has produced a map[string]any.
//
// The codec method bodies deliberately pass underlying errors through
// without wrapping: every caller (bridgeConfigToWireMap, pluginOptions)
// adds the contextual %w wrap. Wrapping twice would double-prefix the
// error message.
type codec interface {
	marshal(any) ([]byte, error)
	unmarshal([]byte, any) error
}

// Both codecs reveal shared.Secret values on marshal: these codecs are used
// ONLY by the authoritative config persistence path (DynamoDB Save, file
// WriteFile, CDK synthesis), so the durable artifact must carry the REAL secret
// (HTTP admin keys, plugin-config passwords/API keys) rather than "[REDACTED]".
// revealSecrets carries the reveal intent on a per-value copy, so concurrent
// default (redacting) json.Marshal/yaml.Marshal calls elsewhere are unaffected.
type jsonCodec struct{}

func (jsonCodec) marshal(v any) ([]byte, error)   { return json.Marshal(shared.RevealSecrets(v)) } //nolint:wrapcheck // wrapped by caller
func (jsonCodec) unmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }                  //nolint:wrapcheck // wrapped by caller

type yamlCodec struct{}

func (yamlCodec) marshal(v any) ([]byte, error)   { return yaml.Marshal(shared.RevealSecrets(v)) } //nolint:wrapcheck // wrapped by caller
func (yamlCodec) unmarshal(b []byte, v any) error { return yaml.Unmarshal(b, v) }                  //nolint:wrapcheck // wrapped by caller

// bridgeConfigToWireMap performs the round-trip: marshal cfg with
// reflection (dropping all Config fields), unmarshal back into a
// map[string]any, then walk the BridgeConfig in tandem and splice each
// typed Config payload into an `options` key.
func bridgeConfigToWireMap(cfg *ports.BridgeConfig, c codec) (map[string]any, error) {
	raw, err := c.marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("config: marshal bridge config: %w", err)
	}
	var m map[string]any
	if err := c.unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("config: marshal bridge config: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}

	// Stores -- nested under `stores`.
	if storesAny, ok := m["stores"]; ok {
		if storesMap, ok := storesAny.(map[string]any); ok {
			injectStoreOptions(storesMap, "lease", cfg.Stores.Lease, c)
			injectStoreOptions(storesMap, "outbox", cfg.Stores.Outbox, c)
			injectStoreOptions(storesMap, "dlq", cfg.Stores.DLQ, c)
		}
	}

	// Top-level slices: sessions, receivers (with nested topics), senders,
	// bindings.
	if err := injectSliceOptions(m, "sessions", len(cfg.Sessions), func(i int) ports.PluginConfig {
		return cfg.Sessions[i].Config
	}, c); err != nil {
		return nil, err
	}
	if err := injectReceiverSliceOptions(m, cfg.Receivers, c); err != nil {
		return nil, err
	}
	if err := injectSliceOptions(m, "senders", len(cfg.Senders), func(i int) ports.PluginConfig {
		return cfg.Senders[i].Config
	}, c); err != nil {
		return nil, err
	}
	if err := injectSliceOptions(m, "bindings", len(cfg.Bindings), func(i int) ports.PluginConfig {
		return cfg.Bindings[i].Config
	}, c); err != nil {
		return nil, err
	}

	return m, nil
}

func injectStoreOptions(stores map[string]any, key string, sc *ports.StoreConfig, c codec) {
	if sc == nil {
		return
	}
	entryAny, ok := stores[key]
	if !ok {
		return
	}
	entry, ok := entryAny.(map[string]any)
	if !ok {
		return
	}
	opts, err := pluginOptions(sc.Config, c)
	if err != nil || opts == nil {
		return
	}
	entry["options"] = opts
}

func injectSliceOptions(parent map[string]any, key string, n int, cfgAt func(int) ports.PluginConfig, c codec) error {
	if n == 0 {
		return nil
	}
	arrAny, ok := parent[key]
	if !ok {
		return nil
	}
	arr, ok := arrAny.([]any)
	if !ok || len(arr) != n {
		return nil
	}
	for i := 0; i < n; i++ {
		entry, ok := arr[i].(map[string]any)
		if !ok {
			continue
		}
		opts, err := pluginOptions(cfgAt(i), c)
		if err != nil {
			return err
		}
		if opts != nil {
			entry["options"] = opts
		}
	}
	return nil
}

func injectReceiverSliceOptions(parent map[string]any, receivers []ports.ReceiverDef, c codec) error {
	if len(receivers) == 0 {
		return nil
	}
	arrAny, ok := parent["receivers"]
	if !ok {
		return nil
	}
	arr, ok := arrAny.([]any)
	if !ok || len(arr) != len(receivers) {
		return nil
	}
	for i := range receivers {
		entry, ok := arr[i].(map[string]any)
		if !ok {
			continue
		}
		if opts, err := pluginOptions(receivers[i].Config, c); err != nil {
			return err
		} else if opts != nil {
			entry["options"] = opts
		}
		// Nested topics (SubscriptionDef) carry their own typed Config.
		if topicsAny, ok := entry["topics"]; ok {
			topics, ok := topicsAny.([]any)
			if ok && len(topics) == len(receivers[i].Topics) {
				for j := range receivers[i].Topics {
					tEntry, ok := topics[j].(map[string]any)
					if !ok {
						continue
					}
					if opts, err := pluginOptions(receivers[i].Topics[j].Config, c); err != nil {
						return err
					} else if opts != nil {
						tEntry["options"] = opts
					}
				}
			}
		}
	}
	return nil
}

// pluginOptions converts a typed PluginConfig into a generic
// map[string]any using the supplied codec (so json struct tags drive
// JSON output and yaml tags drive YAML output). A nil Config or one
// that marshals to a non-object payload yields nil, signalling "do not
// emit an options key".
func pluginOptions(cfg ports.PluginConfig, c codec) (map[string]any, error) {
	if cfg == nil {
		return nil, nil
	}
	data, err := c.marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("config: marshal plugin config: %w", err)
	}
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	var opts map[string]any
	if err := c.unmarshal(data, &opts); err != nil {
		// PluginConfig marshaled to a non-object (rare); drop silently
		// rather than emitting an unparsable file.
		return nil, nil
	}
	if len(opts) == 0 {
		return nil, nil
	}
	return opts, nil
}
