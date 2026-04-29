package config_test

import (
	"reflect"
	"testing"

	"github.com/mariotoffia/gobridge/config"
)

// TestPluggableOptions_GenericMap locks in the architectural constraint
// that core configuration definitions must use a generic
// map[string]any for plugin-specific options. Plugin packages own
// their typed option shapes and parse them via *FromOptions helpers.
//
// This regression test fails if a future change ever introduces a
// plugin-specific typed shape into one of the core *Def types.
func TestPluggableOptions_GenericMap(t *testing.T) {
	mapStringAny := reflect.TypeFor[map[string]any]()

	cases := []struct {
		name      string
		container any
		field     string
	}{
		{"SessionDef.Options", config.SessionDef{}, "Options"},
		{"ReceiverDef.Options", config.ReceiverDef{}, "Options"},
		{"SubscriptionDef.Options", config.SubscriptionDef{}, "Options"},
		{"SenderDef.Options", config.SenderDef{}, "Options"},
		{"StoreConfig.Options", config.StoreConfig{}, "Options"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := reflect.TypeOf(tc.container)
			f, ok := rt.FieldByName(tc.field)
			if !ok {
				t.Fatalf("%s: missing %s field; core config must keep a generic option container", rt.Name(), tc.field)
			}
			if f.Type != mapStringAny {
				t.Fatalf("%s.%s must be map[string]any to preserve plugin pluggability, got %s", rt.Name(), tc.field, f.Type)
			}
		})
	}
}
