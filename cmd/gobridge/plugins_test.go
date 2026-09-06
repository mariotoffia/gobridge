package main

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

// Tagged tests declare their presence independently of production family wiring,
// so an accidental untagged compiledFamilies append cannot skip the blank test.
var expectedFamilies []string

func requireBlankBuild(t *testing.T) {
	t.Helper()
	if len(expectedFamilies) != 0 {
		t.Skip("blank-root assertion requires a build without plugin families")
	}
}

// TestBlankRoot_RegistersNoKinds verifies the untagged aggregates are no-ops.
func TestBlankRoot_RegistersNoKinds(t *testing.T) {
	requireBlankBuild(t)
	reg := ports.NewRegistry()
	if err := registerAllDecoders(reg); err != nil {
		t.Fatal(err)
	}
	if got := reg.Kinds(); len(got) != 0 {
		t.Fatalf("blank root registered %v", got)
	}
	if len(compiledFamilies) != 0 {
		t.Fatalf("blank root compiled families %v", compiledFamilies)
	}
	if err := wireAllFactories(t.Context(), bridge.NewSupervisor(), discardLogger(), nil); err != nil {
		t.Fatal(err)
	}
	if err := seedAllStores(context.Background(), bridge.NewBuilder(&ports.BridgeConfig{})); err != nil {
		t.Fatal(err)
	}
}
