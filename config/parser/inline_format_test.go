package parser_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/require"
)

func TestInlineSource_UsesStrictJSONTypes(t *testing.T) {
	_, err := parser.NewInlineSource(`{"bridge":{"id":123}}`, ports.NewRegistry()).Load(t.Context())
	require.ErrorContains(t, err, "json parse")
}

func TestInlineSource_RejectsUnknownFieldsAndTrailingContent(t *testing.T) {
	for _, contents := range []string{
		`{"bridge":{"id":"inline"},"unknown":true}`,
		`{"bridge":{"id":"inline","unknown":true}}`,
		`{"bridge":{"id":"inline"}}{"bridge":{"id":"second"}}`,
		`{"bridge":{"id":"inline"}} trailing`,
	} {
		t.Run(contents, func(t *testing.T) {
			_, err := parser.NewInlineSource(contents, ports.NewRegistry()).Load(t.Context())
			require.Error(t, err)
		})
	}
}

func TestInlineSource_PreservesYAMLFlowMappings(t *testing.T) {
	for _, contents := range []string{
		`{bridge: {id: inline}}`,
		`{"bridge": {id: inline}}`,
		" \n{\"bridge\":{\"id\":\"inline\"}}\n ",
	} {
		t.Run(contents, func(t *testing.T) {
			cfg, err := parser.NewInlineSource(contents, ports.NewRegistry()).Load(t.Context())
			require.NoError(t, err)
			require.Equal(t, "inline", cfg.Bridge.ID)
		})
	}
}
