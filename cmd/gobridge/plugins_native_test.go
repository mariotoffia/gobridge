//go:build gobridge_native || gobridge_all

package main

import (
	"context"
	"slices"
	"testing"
	"time"

	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func init() { expectedFamilies = append(expectedFamilies, "native") }

// TestNativeFamily_RegistersDecoders verifies native kinds and family membership.
func TestNativeFamily_RegistersDecoders(t *testing.T) {
	reg := ports.NewRegistry()
	if err := registerNativeDecoders(reg); err != nil {
		t.Fatal(err)
	}
	if got := reg.Kinds(); !slices.Equal(got, []string{"memory", "sqlite"}) {
		t.Fatalf("native kinds = %v", got)
	}
	if !slices.Contains(compiledFamilies, "native") {
		t.Fatalf("compiled families = %v; missing native", compiledFamilies)
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

// TestNativeFamily_SeedStoresMatchWiredStores verifies both composition paths
// resolve memory and sqlite, using real DLQ stores without transport connections.
//
// seedAllStores -> Builder.Plan -> store opened -> Close
// wireAllFactories -> Supervisor.Run -> same store kind opened -> shutdown
func TestNativeFamily_SeedStoresMatchWiredStores(t *testing.T) {
	for _, plugin := range []ports.PluginConfig{
		nativestore.MemoryConfig{AcknowledgeVolatile: true},
		nativestore.SQLiteConfig{Path: ":memory:"},
	} {
		t.Run(plugin.Kind(), func(t *testing.T) {
			cfg := &ports.BridgeConfig{
				Bridge: ports.BridgeSettings{ID: "native-stores"},
				Stores: ports.StoresConfig{DLQ: &ports.StoreConfig{Type: plugin.Kind(), Config: plugin}},
			}
			b := bridge.NewBuilder(cfg, bridge.WithLogger(discardLogger()))
			if err := seedAllStores(t.Context(), b); err != nil {
				t.Fatal(err)
			}
			plan, err := b.Plan(t.Context())
			if err != nil {
				t.Fatalf("seed Builder cannot resolve %s: %v", plugin.Kind(), err)
			}
			plan.Close()

			sup := bridge.NewSupervisor(bridge.WithSupervisorLogger(discardLogger()))
			if err := wireAllFactories(t.Context(), sup, discardLogger(), nil); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			var runErr error
			t.Cleanup(func() {
				cancel()
				wait.RequireClosed(t, done, 2*time.Second)
				if runErr != nil {
					t.Errorf("Supervisor cannot resolve %s: %v", plugin.Kind(), runErr)
				}
			})
			go func() {
				runErr = sup.Run(ctx, cfg, nil)
				close(done)
			}()
			wait.Until(t, 2*time.Second, "Supervisor publishes a runtime with the native store", func() bool {
				select {
				case <-done:
					return true
				default:
					return sup.Runtime() != nil
				}
			})
			if sup.Runtime() == nil {
				t.Fatal("Supervisor did not build a runtime")
			}
		})
	}
}
