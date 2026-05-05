package ports_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/mariotoffia/gobridge/ports"
)

type fakeBlueprintConfig struct {
	KindName string `json:"kind" yaml:"kind"`
	URL      string `json:"queue_url,omitempty" yaml:"queue_url,omitempty"`
}

func (f fakeBlueprintConfig) Kind() string    { return f.KindName }
func (f fakeBlueprintConfig) Validate() error { return nil }

// Round-2 reviewer (anti-patterns) flagged that yaml.v3 ignores
// json.Marshaler. These tests lock in the symmetric MarshalYAML so the
// FileStore and admin PATCH write paths do not silently drop typed
// PluginConfig payloads.
func TestSessionDef_MarshalYAML_ProjectsConfigToOptions(t *testing.T) {
	def := ports.SessionDef{
		ID:        "sess-1",
		Transport: "sqs",
		Config:    fakeBlueprintConfig{KindName: "sqs", URL: "https://example/q"},
	}

	data, err := yaml.Marshal(def)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, yaml.Unmarshal(data, &m))

	assert.Equal(t, "sess-1", m["id"])
	assert.Equal(t, "sqs", m["transport"])
	opts, ok := m["options"].(map[string]any)
	require.True(t, ok, "options must be projected: %s", string(data))
	assert.Equal(t, "sqs", opts["kind"])
	assert.Equal(t, "https://example/q", opts["queue_url"])
}

func TestSessionDef_MarshalJSON_ProjectsConfigToOptions(t *testing.T) {
	def := ports.SessionDef{
		ID:        "sess-1",
		Transport: "sqs",
		Config:    fakeBlueprintConfig{KindName: "sqs", URL: "https://example/q"},
	}

	data, err := json.Marshal(def)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(data, &m))

	opts, ok := m["options"].(map[string]any)
	require.True(t, ok, "options must be projected: %s", string(data))
	assert.Equal(t, "https://example/q", opts["queue_url"])
}

func TestBlueprintTypes_MarshalYAML_NilConfig_NoOptionsKey(t *testing.T) {
	cases := []struct {
		name string
		v    any
	}{
		{"SessionDef", ports.SessionDef{ID: "s"}},
		{"ReceiverDef", ports.ReceiverDef{ID: "r"}},
		{"SenderDef", ports.SenderDef{ID: "x"}},
		{"BindingDef", ports.BindingDef{ID: "b"}},
		{"SubscriptionDef", ports.SubscriptionDef{Topic: "t"}},
		{"StoreConfig", ports.StoreConfig{Type: "memory"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			data, err := yaml.Marshal(tc.v)
			require.NoError(t, err)
			var m map[string]any
			require.NoError(t, yaml.Unmarshal(data, &m))
			_, has := m["options"]
			assert.False(t, has, "options must be omitted when Config is nil: %s", string(data))
		})
	}
}

func TestBlueprintTypes_MarshalYAML_AllProjectConfig(t *testing.T) {
	cfg := fakeBlueprintConfig{KindName: "x", URL: "u"}
	cases := []struct {
		name string
		v    any
	}{
		{"SessionDef", ports.SessionDef{ID: "s", Config: cfg}},
		{"ReceiverDef", ports.ReceiverDef{ID: "r", Config: cfg}},
		{"SenderDef", ports.SenderDef{ID: "x", Config: cfg}},
		{"BindingDef", ports.BindingDef{ID: "b", Config: cfg}},
		{"SubscriptionDef", ports.SubscriptionDef{Topic: "t", Config: cfg}},
		{"StoreConfig", ports.StoreConfig{Type: "x", Config: cfg}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			data, err := yaml.Marshal(tc.v)
			require.NoError(t, err)
			var m map[string]any
			require.NoError(t, yaml.Unmarshal(data, &m))
			opts, ok := m["options"].(map[string]any)
			require.True(t, ok, "options must be projected: %s", string(data))
			assert.Equal(t, "u", opts["queue_url"])
		})
	}
}
