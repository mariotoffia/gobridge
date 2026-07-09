package parser

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParse_RejectsTrailingContent covers the finding that trailing content
// after the first document/object was silently accepted: the parser decoded
// only document 1 while the file visibly held more (a torn, duplicated, or
// multi-doc write). Both the YAML and JSON paths must now reject it so a
// malformed/partial file fails loudly instead of deploying a truncated view.
func TestParse_RejectsTrailingContent(t *testing.T) {
	tests := []struct {
		name   string
		format Format
		input  string
	}{
		{
			name:   "yaml second document after --- separator",
			format: FormatYAML,
			input:  "bridge:\n  id: bridge-1\n---\nbridge:\n  id: bridge-2\n",
		},
		{
			name:   "yaml trailing garbage after document",
			format: FormatYAML,
			input:  "bridge:\n  id: bridge-1\n---\n: : :\n",
		},
		{
			name:   "json second top-level object",
			format: FormatJSON,
			input:  `{"bridge":{"id":"bridge-1"}}{"bridge":{"id":"bridge-2"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.input), tt.format, passthroughRegistry())
			require.Error(t, err, "trailing content after the first document must be rejected")
			require.Contains(t, err.Error(), "trailing")
		})
	}
}

// TestParse_AcceptsSingleDocument is the negative control: a single, well-formed
// document (with or without a trailing newline / trailing whitespace) must still
// parse successfully — the trailing-content guard must not reject a lone doc.
func TestParse_AcceptsSingleDocument(t *testing.T) {
	tests := []struct {
		name   string
		format Format
		input  string
	}{
		{name: "yaml single doc", format: FormatYAML, input: "bridge:\n  id: bridge-1\n"},
		{name: "yaml single doc trailing newlines", format: FormatYAML, input: "bridge:\n  id: bridge-1\n\n\n"},
		{name: "yaml explicit doc-start marker", format: FormatYAML, input: "---\nbridge:\n  id: bridge-1\n"},
		{name: "json single object", format: FormatJSON, input: `{"bridge":{"id":"bridge-1"}}`},
		{name: "json single object trailing ws", format: FormatJSON, input: `{"bridge":{"id":"bridge-1"}}` + "\n  \n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse(strings.NewReader(tt.input), tt.format, passthroughRegistry())
			require.NoError(t, err)
			require.Equal(t, "bridge-1", cfg.Bridge.ID)
		})
	}
}
