package parser_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
)

const hostKind = "hosttest"

// hostConfig is a plugin config whose decoder reads its options through
// RawConfig.Decode, the way every adapter decoder does.
type hostConfig struct {
	Host string `json:"host"`
}

func (c *hostConfig) Kind() string { return hostKind }
func (c *hostConfig) Validate() error {
	if c.Host == "" {
		return errors.New("hosttest: host is required")
	}
	return nil
}

func hostRegistry(t *testing.T) *ports.Registry {
	t.Helper()
	reg := ports.NewRegistry()
	require.NoError(t, reg.Register(hostKind, func(raw ports.RawConfig) (ports.PluginConfig, error) {
		var c hostConfig
		if err := raw.Decode(&c); err != nil {
			return nil, err
		}
		if err := c.Validate(); err != nil {
			return nil, err
		}
		return &c, nil
	}))
	return reg
}

// A document a newer release stored carries fields this release does not
// know: at the top level, inside a known section, and inside a plugin's
// options. Each case adds one of them to an otherwise valid document.
var documentsWithUnknownFields = []struct {
	name   string
	format parser.Format
	input  string
}{
	{
		name:   "json unknown top-level key",
		format: parser.FormatJSON,
		input: `{"bridge":{"id":"b1"},"future_section":{"on":true},
			"sessions":[{"id":"s1","transport":"hosttest","options":{"host":"h1"}}]}`,
	},
	{
		name:   "json unknown nested key",
		format: parser.FormatJSON,
		input: `{"bridge":{"id":"b1","future_setting":"x"},
			"sessions":[{"id":"s1","transport":"hosttest","future_flag":1,"options":{"host":"h1"}}]}`,
	},
	{
		name:   "json unknown plugin option",
		format: parser.FormatJSON,
		input: `{"bridge":{"id":"b1"},
			"sessions":[{"id":"s1","transport":"hosttest","options":{"host":"h1","future_option":"x"}}]}`,
	},
	{
		name:   "yaml unknown key at every level",
		format: parser.FormatYAML,
		input: `
bridge:
  id: b1
  future_setting: x
future_section:
  on: true
sessions:
  - id: s1
    transport: hosttest
    future_flag: 1
    options:
      host: h1
      future_option: x
`,
	},
}

func TestParse_UnknownFields_RefusedByDefault(t *testing.T) {
	for _, tc := range documentsWithUnknownFields {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parser.Parse(strings.NewReader(tc.input), tc.format, hostRegistry(t))
			require.Error(t, err, "a document a person writes is read strictly, so a typo is reported")
		})
	}
}

func TestParse_UnknownFields_IgnoredWithIgnoreUnknownFields(t *testing.T) {
	for _, tc := range documentsWithUnknownFields {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parser.Parse(strings.NewReader(tc.input), tc.format, hostRegistry(t),
				parser.WithIgnoreUnknownFields())
			require.NoError(t, err)
			assert.Equal(t, "b1", cfg.Bridge.ID)
			require.Len(t, cfg.Sessions, 1)
			got, ok := cfg.Sessions[0].Config.(*hostConfig)
			require.True(t, ok, "session config type %T", cfg.Sessions[0].Config)
			assert.Equal(t, "h1", got.Host, "the known option is still decoded")
		})
	}
}

func TestParse_UnknownPluginKind_RefusedWithAndWithoutIgnoreUnknownFields(t *testing.T) {
	doc := `{"bridge":{"id":"b1"},"sessions":[{"id":"s1","transport":"futurekind","options":{"host":"h1"}}]}`
	for name, opts := range map[string][]parser.ParseOption{
		"strict":                {},
		"ignore unknown fields": {parser.WithIgnoreUnknownFields()},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parser.Parse(strings.NewReader(doc), parser.FormatJSON, hostRegistry(t), opts...)
			require.Error(t, err, "a plugin kind outside the registered set is never ignored")
		})
	}
}

func TestParse_IgnoreUnknownFields_KeepsTrailingDataTypeAndSizeChecks(t *testing.T) {
	reject := []struct {
		name   string
		format parser.Format
		input  string
	}{
		{"json trailing document", parser.FormatJSON, `{"bridge":{"id":"b1"}}{"bridge":{"id":"b2"}}`},
		{"yaml trailing document", parser.FormatYAML, "bridge:\n  id: b1\n---\nbridge:\n  id: b2\n"},
		{"known field with the wrong type", parser.FormatJSON, `{"bridge":{"id":7}}`},
		{"yaml known field with the wrong type", parser.FormatYAML, "bridge:\n  id: [b1]\n"},
		{"larger than MaxConfigBytes", parser.FormatJSON,
			`{"bridge":{"id":"b1"}}` + strings.Repeat(" ", parser.MaxConfigBytes)},
		{"known plugin option with the wrong type", parser.FormatJSON,
			`{"bridge":{"id":"b1"},"sessions":[{"id":"s1","transport":"hosttest","options":{"host":["h1"]}}]}`},
	}
	for _, tc := range reject {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parser.Parse(strings.NewReader(tc.input), tc.format, hostRegistry(t),
				parser.WithIgnoreUnknownFields())
			require.Error(t, err)
		})
	}
}
