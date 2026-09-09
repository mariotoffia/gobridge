package parser

import (
	"context"
	"strings"

	"github.com/mariotoffia/gobridge/ports"
)

type inlineSource struct {
	contents string
	registry *ports.Registry
}

// NewInlineSource supplies declarative YAML or JSON through the typed parser.
// Every Load returns a fresh logical configuration; no credentials are resolved.
func NewInlineSource(contents string, registry *ports.Registry) *inlineSource {
	return &inlineSource{contents: contents, registry: registry}
}

func (s *inlineSource) Load(ctx context.Context) (*ports.BridgeConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return Parse(strings.NewReader(s.contents), FormatYAML, s.registry)
}

var _ ports.Loader = (*inlineSource)(nil)
