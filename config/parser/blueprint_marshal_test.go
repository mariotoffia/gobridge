package parser_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
)

type fakeBlueprintConfig struct {
	KindName string `json:"kind" yaml:"kind"`
	URL      string `json:"queue_url,omitempty" yaml:"queue_url,omitempty"`
}

func (f fakeBlueprintConfig) Kind() string    { return f.KindName }
func (f fakeBlueprintConfig) Validate() error { return nil }

// The blueprint marshallers live in config/ rather than ports/ so the
// inner ring stays format-neutral. These tests lock in the bridge-level
// options projection for both wire formats so the
// FileStore (config.WriteFile / parser.MarshalYAML) and DynamoDB Save
// (parser.MarshalBridgeConfigJSON) paths do not silently drop typed
// PluginConfig payloads.

func TestMarshalBridgeConfigJSON_ProjectsConfigToOptions(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b"},
		Sessions: []ports.SessionDef{
			{
				ID:        "sess-1",
				Transport: "sqs",
				Config:    fakeBlueprintConfig{KindName: "sqs", URL: "https://example/q"},
			},
		},
		Receivers: []ports.ReceiverDef{
			{
				ID:        "rx-1",
				Transport: "mqtt",
				Config:    fakeBlueprintConfig{KindName: "mqtt", URL: "tcp://h"},
				Topics: []ports.SubscriptionDef{
					{
						Topic:  "t",
						Config: fakeBlueprintConfig{KindName: "mqtt", URL: "sub-url"},
					},
				},
			},
		},
		Senders: []ports.SenderDef{
			{
				ID:        "tx-1",
				Transport: "mqtt",
				Config:    fakeBlueprintConfig{KindName: "mqtt", URL: "send-url"},
			},
		},
		Bindings: []ports.BindingDef{
			{
				ID:       "bind-1",
				SenderID: "tx-1",
				Address:  "topic/x",
				Config:   fakeBlueprintConfig{KindName: "mqtt", URL: "bind-url"},
			},
		},
		Stores: ports.StoresConfig{
			Lease: &ports.StoreConfig{
				Type:   "dynamodb",
				Config: fakeBlueprintConfig{KindName: "dynamodb", URL: "table"},
			},
		},
	}

	data, err := parser.MarshalBridgeConfigJSON(cfg)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(data, &m))

	sessions, _ := m["sessions"].([]any)
	require.Len(t, sessions, 1)
	sess := sessions[0].(map[string]any)
	assert.Equal(t, "sess-1", sess["id"])
	assert.Equal(t, "https://example/q", sess["options"].(map[string]any)["queue_url"])

	receivers, _ := m["receivers"].([]any)
	require.Len(t, receivers, 1)
	rx := receivers[0].(map[string]any)
	assert.Equal(t, "tcp://h", rx["options"].(map[string]any)["queue_url"])
	topics := rx["topics"].([]any)
	require.Len(t, topics, 1)
	assert.Equal(t, "sub-url", topics[0].(map[string]any)["options"].(map[string]any)["queue_url"])

	senders, _ := m["senders"].([]any)
	require.Len(t, senders, 1)
	assert.Equal(t, "send-url", senders[0].(map[string]any)["options"].(map[string]any)["queue_url"])

	bindings, _ := m["bindings"].([]any)
	require.Len(t, bindings, 1)
	assert.Equal(t, "bind-url", bindings[0].(map[string]any)["options"].(map[string]any)["queue_url"])

	stores := m["stores"].(map[string]any)
	lease := stores["lease"].(map[string]any)
	assert.Equal(t, "table", lease["options"].(map[string]any)["queue_url"])
}

func TestMarshalBridgeConfigJSON_NilCfg(t *testing.T) {
	data, err := parser.MarshalBridgeConfigJSON(nil)
	require.NoError(t, err)
	assert.Equal(t, "null", string(data))
}

func TestMarshalYAML_ProjectsConfigToOptions(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b"},
		Sessions: []ports.SessionDef{
			{
				ID:        "sess-1",
				Transport: "sqs",
				Config:    fakeBlueprintConfig{KindName: "sqs", URL: "https://example/q"},
			},
		},
	}

	data, err := parser.MarshalYAML(cfg)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, yaml.Unmarshal(data, &m))
	sessions := m["sessions"].([]any)
	sess := sessions[0].(map[string]any)
	opts, ok := sess["options"].(map[string]any)
	require.True(t, ok, "options must be projected: %s", string(data))
	assert.Equal(t, "https://example/q", opts["queue_url"])
}

func TestMarshalBridgeConfig_NilConfigOmitsOptions(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b"},
		Sessions: []ports.SessionDef{
			{ID: "s", Transport: "sqs"},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "r", Transport: "mqtt", Topics: []ports.SubscriptionDef{{Topic: "t"}}},
		},
		Senders:  []ports.SenderDef{{ID: "x", Transport: "mqtt"}},
		Bindings: []ports.BindingDef{{ID: "b", SenderID: "x", Address: "a"}},
		Stores: ports.StoresConfig{
			Lease: &ports.StoreConfig{Type: "memory"},
		},
	}

	for _, format := range []string{"json", "yaml"} {
		var (
			data []byte
			err  error
			m    map[string]any
		)
		if format == "json" {
			data, err = parser.MarshalBridgeConfigJSON(cfg)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &m))
		} else {
			data, err = parser.MarshalYAML(cfg)
			require.NoError(t, err)
			require.NoError(t, yaml.Unmarshal(data, &m))
		}

		sess := m["sessions"].([]any)[0].(map[string]any)
		_, has := sess["options"]
		assert.False(t, has, "%s: session options must be omitted: %s", format, string(data))

		rx := m["receivers"].([]any)[0].(map[string]any)
		_, has = rx["options"]
		assert.False(t, has, "%s: receiver options must be omitted: %s", format, string(data))
		topic := rx["topics"].([]any)[0].(map[string]any)
		_, has = topic["options"]
		assert.False(t, has, "%s: subscription options must be omitted", format)

		tx := m["senders"].([]any)[0].(map[string]any)
		_, has = tx["options"]
		assert.False(t, has, "%s: sender options must be omitted", format)

		b := m["bindings"].([]any)[0].(map[string]any)
		_, has = b["options"]
		assert.False(t, has, "%s: binding options must be omitted", format)

		lease := m["stores"].(map[string]any)["lease"].(map[string]any)
		_, has = lease["options"]
		assert.False(t, has, "%s: store options must be omitted", format)
	}
}
