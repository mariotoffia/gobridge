package config_test

import (
	"reflect"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// TestTypedPluginConfig_OnAllAttachmentPoints locks in the FIX-003
// invariant that every plugin attachment point in ports/blueprint.go
// carries a typed `Config ports.PluginConfig` instead of an untyped
// `Options map[string]any`. Phase 1 inverts the previous
// pluggability constraint: the inner ring must remain free of raw
// option maps so the two-stage parser owns kind discrimination.
//
// This regression test fails if a future change reintroduces an
// `Options map[string]any` shape on any of the listed core *Def
// types or removes the typed `Config` field.
func TestTypedPluginConfig_OnAllAttachmentPoints(t *testing.T) {
	pluginConfigType := reflect.TypeOf((*ports.PluginConfig)(nil)).Elem()
	mapStringAny := reflect.TypeFor[map[string]any]()

	cases := []struct {
		name      string
		container any
	}{
		{"ports.StoreConfig", ports.StoreConfig{}},
		{"ports.SessionDef", ports.SessionDef{}},
		{"ports.ReceiverDef", ports.ReceiverDef{}},
		{"ports.SubscriptionDef", ports.SubscriptionDef{}},
		{"ports.SenderDef", ports.SenderDef{}},
		{"ports.BindingDef", ports.BindingDef{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := reflect.TypeOf(tc.container)

			cfgField, ok := rt.FieldByName("Config")
			if !ok {
				t.Fatalf("%s: missing Config field; every plugin attachment point must expose a typed PluginConfig carrier", rt.Name())
			}
			if cfgField.Type != pluginConfigType {
				t.Fatalf("%s.Config must be ports.PluginConfig, got %s", rt.Name(), cfgField.Type)
			}

			if _, ok := rt.FieldByName("Options"); ok {
				t.Fatalf("%s.Options must not exist; raw map[string]any options are forbidden after FIX-003 phase 1", rt.Name())
			}

			for i := 0; i < rt.NumField(); i++ {
				f := rt.Field(i)
				if f.Type == mapStringAny {
					t.Fatalf("%s.%s: map[string]any field is forbidden in plugin attachment points", rt.Name(), f.Name)
				}
			}
		})
	}
}
