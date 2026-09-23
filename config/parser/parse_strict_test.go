package parser

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// TestParse_StrictDecode_RejectsUnknownKeys pins that the stage-1 outer decode
// is strict: a typo like `shutdown_timout:` or a stray `config_wacth:` section
// must be reported, not silently discarded. Strict decoding (yaml KnownFields /
// json DisallowUnknownFields) must surface it.
func TestParse_StrictDecode_RejectsUnknownKeys(t *testing.T) {
	tests := []struct {
		name   string
		format Format
		input  string
	}{
		{
			name:   "yaml stray top-level section (typo config_watch)",
			format: FormatYAML,
			input: `
bridge:
  id: bridge-1
config_wacth:
  mode: poll
`,
		},
		{
			name:   "yaml nested typo (shutdown_timout)",
			format: FormatYAML,
			input: `
bridge:
  id: bridge-1
  shutdown_timout: 30s
`,
		},
		{
			name:   "json stray top-level key",
			format: FormatJSON,
			input:  `{"bridge":{"id":"bridge-1"},"config_wacth":{"mode":"poll"}}`,
		},
		{
			name:   "json nested typo",
			format: FormatJSON,
			input:  `{"bridge":{"id":"bridge-1","shutdown_timout":"30s"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.input), tt.format, passthroughRegistry())
			require.Error(t, err, "unknown/typo key must surface")
		})
	}
}

// TestParse_StrictDecode_AcceptsCanonicalRoundTrip guards the strict decode
// against a round-trip regression: the authoritative marshallers (DynamoDB
// Save / file WriteFile) emit only schema keys, so canonically-marshalled
// output must always re-parse cleanly under strict decoding.
func TestParse_StrictDecode_AcceptsCanonicalRoundTrip(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Version: 1,
		Bridge:  ports.BridgeSettings{ID: "bridge-1", ShutdownTimeout: "30s"},
		Receivers: []ports.ReceiverDef{
			{ID: "rx1", Transport: "sqs"},
		},
		Senders: []ports.SenderDef{
			{ID: "tx1", Transport: "sqs"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx1", Address: "queue://test"},
		},
		Routes: []ports.RouteDef{
			{ID: "r1", ReceiverID: "rx1", Bindings: []string{"b1"}},
		},
		HTTP: &ports.HTTPConfig{AdminAddr: ":8080"},
	}

	jsonBytes, err := MarshalBridgeConfigJSON(cfg)
	require.NoError(t, err)
	got, err := Parse(strings.NewReader(string(jsonBytes)), FormatJSON, passthroughRegistry("sqs"))
	require.NoError(t, err, "canonical JSON must re-parse under strict decode")
	assert.Equal(t, "bridge-1", got.Bridge.ID)
}

// TestParse_StrictDecode_RejectsTrailingJSON pins that the JSON stage-1 decode
// keeps json.Unmarshal's trailing-data rejection even though it uses
// json.NewDecoder (which stops at the first top-level value). A second
// document or trailing garbage after the config object must be rejected;
// insignificant trailing whitespace must still parse.
func TestParse_StrictDecode_RejectsTrailingJSON(t *testing.T) {
	reject := []struct {
		name  string
		input string
	}{
		{"second document", `{"bridge":{"id":"bridge-1"}}{"bridge":{"id":"other"}}`},
		{"trailing garbage", `{"bridge":{"id":"bridge-1"}} oops`},
		{"trailing array", `{"bridge":{"id":"bridge-1"}}[1,2,3]`},
	}
	for _, tt := range reject {
		t.Run("reject "+tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.input), FormatJSON, passthroughRegistry())
			require.Error(t, err, "trailing data after the top-level object must be rejected")
		})
	}

	// Insignificant trailing whitespace is not trailing data and must parse.
	_, err := Parse(strings.NewReader("{\"bridge\":{\"id\":\"bridge-1\"}}\n  \n"), FormatJSON, passthroughRegistry())
	require.NoError(t, err, "trailing whitespace must remain acceptable")
}
