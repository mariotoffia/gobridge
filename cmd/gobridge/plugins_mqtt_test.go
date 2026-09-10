//go:build gobridge_mqtt || gobridge_all

package main

import (
	"slices"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

func init() { expectedFamilies = append(expectedFamilies, "mqtt") }

// TestMQTTFamily_RegistersDecoders verifies both aliases and family membership.
func TestMQTTFamily_RegistersDecoders(t *testing.T) {
	reg := ports.NewRegistry()
	if err := registerMQTTDecoders(reg); err != nil {
		t.Fatal(err)
	}
	if got := reg.Kinds(); !slices.Equal(got, []string{"mqtt", "mqtt.paho"}) {
		t.Fatalf("MQTT kinds = %v", got)
	}
	if !slices.Contains(compiledFamilies, "mqtt") {
		t.Fatalf("compiled families = %v; missing mqtt", compiledFamilies)
	}
	all := ports.NewRegistry()
	if err := registerAllDecoders(all); err != nil {
		t.Fatal(err)
	}
	for _, kind := range reg.Kinds() {
		if !slices.Contains(all.Kinds(), kind) {
			t.Errorf("aggregate registry missing %q", kind)
		}
	}
}
