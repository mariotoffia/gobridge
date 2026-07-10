package config

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// clusteredEndpointsConfig returns an otherwise-valid clustered blueprint whose
// only interesting axis is cluster.endpoints, so the cluster-endpoint validator
// (finding HIGH-1) is what a test toggles.
func clusteredEndpointsConfig(endpoints map[string]string) *ports.BridgeConfig {
	cfg := minimalValidBridgeConfig()
	cfg.Bridge.DeploymentMode = "clustered"
	if endpoints != nil {
		cfg.Bridge.Cluster = &ports.ClusterConfig{Endpoints: endpoints}
	}
	return cfg
}

// TestValidateClusterEndpoints_PeerMapRejected proves the copied-from-docs
// peer-membership shape (instance-id keys, no "http" key) is rejected at load
// time (HIGH-1). Reverting the validator makes this fail — the peer map would
// pass and later 502 at forward time.
func TestValidateClusterEndpoints_PeerMapRejected(t *testing.T) {
	cfg := clusteredEndpointsConfig(map[string]string{
		"instance-01": "10.0.1.10:8080",
		"instance-02": "10.0.1.11:8080",
	})

	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected peer-map cluster.endpoints to be rejected")
	}
	if !strings.Contains(err.Error(), "cluster.endpoints") ||
		!strings.Contains(err.Error(), "http") {
		t.Fatalf("error should name cluster.endpoints and the missing http key, got: %v", err)
	}
}

// TestValidateClusterEndpoints_CapabilityShapePasses proves the correct
// capability-keyed shape ({http: "http://host:port"}) passes validation.
func TestValidateClusterEndpoints_CapabilityShapePasses(t *testing.T) {
	cfg := clusteredEndpointsConfig(map[string]string{
		"http": "http://10.0.1.10:8080",
	})

	if err := Validate(cfg); err != nil {
		t.Fatalf("capability-keyed endpoints should pass, got: %v", err)
	}
}

// TestValidateClusterEndpoints_BareHostPortRejected proves a bare host:port
// value under the http key (not an absolute URL) is rejected: the forwarder
// POSTs to this value directly and needs a full URL.
func TestValidateClusterEndpoints_BareHostPortRejected(t *testing.T) {
	cfg := clusteredEndpointsConfig(map[string]string{
		"http": "10.0.1.10:8080",
	})

	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected bare host:port http endpoint to be rejected")
	}
	if !strings.Contains(err.Error(), "cluster.endpoints.http") {
		t.Fatalf("error should name cluster.endpoints.http, got: %v", err)
	}
}

// TestValidateClusterEndpoints_AutoDiscoveryPasses proves an unset endpoints
// override (auto-discovery) is NOT forced to carry an http key.
func TestValidateClusterEndpoints_AutoDiscoveryPasses(t *testing.T) {
	cfg := clusteredEndpointsConfig(nil) // no cluster block at all

	if err := Validate(cfg); err != nil {
		t.Fatalf("clustered auto-discovery (no static endpoints) should pass, got: %v", err)
	}
}

// TestValidateClusterEndpoints_NonClusteredPeerMapPasses proves the rule is
// scoped to clustered deployments: a peer map with cluster.endpoints unset AND
// deployment_mode standalone is not clustered, so nothing is validated. (A peer
// map WITH endpoints set makes the deployment clustered by definition, so that
// case is covered by the reject test above.)
func TestValidateClusterEndpoints_NonClusteredNoEndpointsPasses(t *testing.T) {
	cfg := minimalValidBridgeConfig() // standalone, no cluster block

	if err := Validate(cfg); err != nil {
		t.Fatalf("standalone config without endpoints should pass, got: %v", err)
	}
}

// httpExclusiveRouteConfig returns an otherwise-valid blueprint with a single
// exclusive route (inline session block) whose ingress is the HTTP transport.
// clustered and delivery mode are set by the caller so the direct_hold rule
// (finding HIGH-4) is the axis under test.
func httpExclusiveRouteConfig(clustered bool, deliveryMode string) *ports.BridgeConfig {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test"},
		Sessions: []ports.SessionDef{
			{ID: "sess-1", Transport: "http", SessionMode: "exclusive"},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "http-rx", Transport: "http", SessionID: "sess-1"},
		},
		Senders: []ports.SenderDef{
			{ID: "s1", Transport: "sqs"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "s1", Address: "q1"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "exclusive-http",
				ReceiverID:   "http-rx",
				DeliveryMode: deliveryMode,
				Bindings:     []string{"b1"},
				Session:      &ports.RouteSessionDef{SessionID: "sess-1", SenderID: "s1"},
			},
		},
	}
	if clustered {
		cfg.Bridge.DeploymentMode = "clustered"
	}
	cfg.Stores.Lease = &ports.StoreConfig{Type: "memory"} // exclusive session requires a lease store
	if deliveryMode == "shared_outbox" {
		cfg.Stores.Outbox = &ports.StoreConfig{Type: "memory"}
	}
	return cfg
}

func errContains(err error, sub string) bool {
	return err != nil && strings.Contains(err.Error(), sub)
}

// TestValidateClusteredExclusiveHTTPDirectHold covers the HIGH-4 rule:
// clustered + exclusive + HTTP ingress must use shared_outbox, not direct_hold.
func TestValidateClusteredExclusiveHTTPDirectHold(t *testing.T) {
	tests := []struct {
		name         string
		clustered    bool
		deliveryMode string
		wantRejected bool
	}{
		{"clustered explicit direct_hold rejected", true, "direct_hold", true},
		{"clustered default (empty) direct_hold rejected", true, "", true},
		{"clustered shared_outbox passes", true, "shared_outbox", false},
		{"non-clustered direct_hold passes", false, "direct_hold", false},
		{"non-clustered default passes", false, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := httpExclusiveRouteConfig(tt.clustered, tt.deliveryMode)
			err := Validate(cfg)
			if tt.wantRejected {
				if !errContains(err, "exclusive-http") || !errContains(err, "shared_outbox") {
					t.Fatalf("expected rejection naming the route and shared_outbox, got: %v", err)
				}
				return
			}
			// Must not be rejected for the HIGH-4 reason (other validation must
			// also pass for these minimal configs).
			if errContains(err, "not fencing-safe across failover") {
				t.Fatalf("route should not be rejected by the direct_hold HTTP rule, got: %v", err)
			}
			if err != nil {
				t.Fatalf("config should validate cleanly, got: %v", err)
			}
		})
	}
}

// TestValidateClusteredDirectHold_NonHTTPIngressPasses proves the rule is scoped
// to HTTP ingress: a clustered exclusive route with a non-HTTP receiver keeps
// direct_hold without a hard rejection (only the generic fencing warning).
func TestValidateClusteredDirectHold_NonHTTPIngressPasses(t *testing.T) {
	cfg := httpExclusiveRouteConfig(true, "direct_hold")
	cfg.Receivers[0].Transport = "sqs" // non-HTTP ingress
	cfg.Sessions[0].Transport = "sqs"  // session and its receiver must share a transport

	err := Validate(cfg)
	if errContains(err, "not fencing-safe across failover") {
		t.Fatalf("non-HTTP ingress must not trigger the direct_hold HTTP rule, got: %v", err)
	}
	if err != nil {
		t.Fatalf("config should validate cleanly, got: %v", err)
	}
}
