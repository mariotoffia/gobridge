package parser_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
)

func FuzzInlineSource(f *testing.F) {
	f.Add("bridge: {id: inline}")
	f.Add(`{"bridge":{"id":"inline"}}`)
	f.Add("[")
	f.Fuzz(func(t *testing.T, contents string) {
		cfg, err := parser.NewInlineSource(contents, ports.NewRegistry()).Load(t.Context())
		if err == nil && cfg == nil {
			t.Fatal("successful inline load returned no blueprint")
		}
	})
}
