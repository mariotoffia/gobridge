package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// A blueprint with a DLQ store, no lease store and a binding that names a
// session builds a standalone bridge whose plain /ready answers 200 once it is
// healthy. The builder registers the binding's session as exclusive with a
// deferred connect; with no lease store nothing grants it a lease, so treating
// it as lease-managed made the instance a standby whose readiness is capped
// below what /ready requires, and /ready answered 503 for good.
func TestBuilder_LeaselessBindingSessionIsStandaloneAndReady(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "leaseless-binding"},
		Stores:    ports.StoresConfig{DLQ: &ports.StoreConfig{Type: "memory"}},
		Sessions:  []ports.SessionDef{{ID: "dst-session", Transport: "fake"}},
		Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "fake"}},
		Senders:   []ports.SenderDef{{ID: "tx", Transport: "fake", SessionID: "dst-session"}},
		Bindings:  []ports.BindingDef{{ID: "b1", SenderID: "tx", SessionID: "dst-session", Address: "out/addr"}},
		Routes: []ports.RouteDef{{
			ID: "r1", ReceiverID: "rx", DeliveryMode: "direct_hold", Bindings: []string{"b1"},
			Policy: ports.PolicyDef{OnPermanentFailure: "dlq", OnExpired: "drop"},
		}},
	}
	rt, err := bridge.NewBuilder(cfg).
		RegisterTransportFactory("fake", &cfgFakeTransportFactory{}).
		RegisterStoreFactory("memory", &cfgFakeStoreFactory{}).
		Build(context.Background())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := rt.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	if role := rt.Role(); role != ports.RoleStandalone {
		t.Errorf("role = %q, want %q: without a lease store no session takes part in failover", role, ports.RoleStandalone)
	}
	base, _ := processHealthServer(t, rt)
	ready := wait.Poll(2*time.Second, func() bool {
		resp, _ := apiGet(t, base+"/api/v1/monitor/ready", "")
		return resp.StatusCode == http.StatusOK
	})
	if !ready {
		t.Fatalf("/ready never answered 200 for a healthy standalone bridge (level %s)", rt.ReadinessLevel(context.Background()))
	}
}
