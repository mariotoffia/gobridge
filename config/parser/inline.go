package parser

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/mariotoffia/gobridge/ports"
)

type inlineSource struct {
	contents string
	registry *ports.Registry
}

// NewInlineSource supplies declarative YAML or JSON through the typed parser.
// JSON documents use the strict JSON decoder; other inputs retain YAML syntax,
// including flow mappings. Every Load returns a fresh logical configuration.
// No credentials are resolved.
func NewInlineSource(contents string, registry *ports.Registry) *inlineSource {
	return &inlineSource{contents: contents, registry: registry}
}

func (s *inlineSource) Load(ctx context.Context) (*ports.BridgeConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	format := FormatYAML
	if len(s.contents) <= MaxConfigBytes && json.Valid([]byte(s.contents)) {
		format = FormatJSON
	}
	return Parse(strings.NewReader(s.contents), format, s.registry)
}

var _ ports.Loader = (*inlineSource)(nil)
