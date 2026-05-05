package ports

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// JSON marshaling for blueprint types that carry a typed PluginConfig.
//
// The Config field on SessionDef/ReceiverDef/SenderDef/StoreConfig/
// SubscriptionDef/BindingDef is tagged json:"-" so the default marshaler
// drops it. That is correct for the parser's wire format (which uses an
// `options:` map) but it loses data when callers round-trip a fully
// decoded *BridgeConfig through JSON (e.g. the DynamoDB config loader's
// Save → DynamoDB → Load path, or admin tooling that re-emits a parsed
// config). The custom MarshalJSON methods below project Config back into
// the canonical `options` map so the wire form parsers see the same
// payload that produced the typed Config.
//
// UnmarshalJSON is not defined here: the config/ package owns parsing
// via stage1 wire types that already understand the `options` field.

func pluginOptions(cfg PluginConfig) (map[string]any, error) {
	if cfg == nil {
		return nil, nil
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("ports: marshal plugin config: %w", err)
	}
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	var opts map[string]any
	if err := json.Unmarshal(data, &opts); err != nil {
		// Config marshaled to a non-object (rare for plugin configs);
		// drop silently rather than emitting an unparsable file.
		return nil, nil
	}
	if len(opts) == 0 {
		return nil, nil
	}
	return opts, nil
}

func marshalWithOptions(v any, cfg PluginConfig) ([]byte, error) {
	opts, err := pluginOptions(cfg)
	if err != nil {
		return nil, err
	}
	aux := struct {
		Inner   any            `json:"-"`
		Options map[string]any `json:"options,omitempty"`
	}{Inner: v, Options: opts}
	// Marshal inner separately so we can splice options into its object
	// representation without depending on type aliasing tricks per call site.
	innerBytes, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("ports: marshal blueprint: %w", err)
	}
	if opts == nil {
		return innerBytes, nil
	}
	// Decode inner into a map so we can add options without re-implementing
	// every base field.
	var m map[string]any
	if err := json.Unmarshal(innerBytes, &m); err != nil {
		return nil, fmt.Errorf("ports: marshal blueprint: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	m["options"] = aux.Options
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("ports: marshal blueprint: %w", err)
	}
	return out, nil
}

// marshalYAMLWithOptions builds a YAML node-friendly map representation of
// v (using yaml.v3 tag rules) with the typed Config projected into the
// canonical `options` key. The yaml package then encodes the returned map.
func marshalYAMLWithOptions(v any, cfg PluginConfig) (any, error) {
	data, err := yaml.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("ports: marshal blueprint yaml: %w", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("ports: marshal blueprint yaml: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	opts, err := pluginOptions(cfg)
	if err != nil {
		return nil, err
	}
	if opts != nil {
		m["options"] = opts
	}
	return m, nil
}

// MarshalJSON projects the typed Config into an `options` map for
// wire-format consumers (e.g. config.Parse).
func (s SessionDef) MarshalJSON() ([]byte, error) {
	type alias SessionDef
	return marshalWithOptions(alias(s), s.Config)
}

// MarshalYAML mirrors MarshalJSON for the YAML wire format. See package
// doc above for rationale (yaml.v3 does not honor json.Marshaler).
func (s SessionDef) MarshalYAML() (any, error) {
	type alias SessionDef
	return marshalYAMLWithOptions(alias(s), s.Config)
}

// MarshalJSON — see SessionDef.MarshalJSON.
func (r ReceiverDef) MarshalJSON() ([]byte, error) {
	type alias ReceiverDef
	return marshalWithOptions(alias(r), r.Config)
}

// MarshalYAML — see SessionDef.MarshalYAML.
func (r ReceiverDef) MarshalYAML() (any, error) {
	type alias ReceiverDef
	return marshalYAMLWithOptions(alias(r), r.Config)
}

// MarshalJSON — see SessionDef.MarshalJSON.
func (s SenderDef) MarshalJSON() ([]byte, error) {
	type alias SenderDef
	return marshalWithOptions(alias(s), s.Config)
}

// MarshalYAML — see SessionDef.MarshalYAML.
func (s SenderDef) MarshalYAML() (any, error) {
	type alias SenderDef
	return marshalYAMLWithOptions(alias(s), s.Config)
}

// MarshalJSON — see SessionDef.MarshalJSON.
func (b BindingDef) MarshalJSON() ([]byte, error) {
	type alias BindingDef
	return marshalWithOptions(alias(b), b.Config)
}

// MarshalYAML — see SessionDef.MarshalYAML.
func (b BindingDef) MarshalYAML() (any, error) {
	type alias BindingDef
	return marshalYAMLWithOptions(alias(b), b.Config)
}

// MarshalJSON — see SessionDef.MarshalJSON.
func (s SubscriptionDef) MarshalJSON() ([]byte, error) {
	type alias SubscriptionDef
	return marshalWithOptions(alias(s), s.Config)
}

// MarshalYAML — see SessionDef.MarshalYAML.
func (s SubscriptionDef) MarshalYAML() (any, error) {
	type alias SubscriptionDef
	return marshalYAMLWithOptions(alias(s), s.Config)
}

// MarshalJSON — see SessionDef.MarshalJSON.
func (s StoreConfig) MarshalJSON() ([]byte, error) {
	type alias StoreConfig
	return marshalWithOptions(alias(s), s.Config)
}

// MarshalYAML — see SessionDef.MarshalYAML.
func (s StoreConfig) MarshalYAML() (any, error) {
	type alias StoreConfig
	return marshalYAMLWithOptions(alias(s), s.Config)
}
