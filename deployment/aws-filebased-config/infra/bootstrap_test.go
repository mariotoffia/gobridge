package infra

import (
	"testing"
	"time"
)

func TestBootstrapConfig_Normalized_AppliesDefaults(t *testing.T) {
	c := BootstrapConfig{
		BridgeID:         "bridge-1",
		ConfigFilePath:   "/etc/bridge.yaml",
		AdminAPIKeyParam: "/admin",
	}.Normalized()

	assertEqual(t, NodeRoleControl, c.NodeRole)
	assertEqual(t, TopologySingle, c.Topology)
	assertEqual(t, DefaultAdminAddr, c.AdminAddr)
	assertEqual(t, DefaultMonitorAddr, c.MonitorAddr)
	assertEqual(t, DefaultTransportHTTPAddr, c.TransportHTTPAddr)
	assertEqual(t, DefaultContainerMemoryBytes, c.ContainerMemoryBytes)
	if c.HTTPReceiverAPIKeyParams == nil {
		t.Error("HTTPReceiverAPIKeyParams should not be nil after Normalized()")
	}

	if c.HTTPSenderAPIKeyParams == nil {
		t.Error("HTTPSenderAPIKeyParams should not be nil after Normalized()")
	}
}

func TestBootstrapConfig_MemoryHeadroomBoundary(t *testing.T) {
	const container = uint64(1000)
	base := BootstrapConfig{
		BridgeID:             "b",
		ConfigFilePath:       "/f",
		AdminAPIKeyParam:     "/a",
		NodeRole:             NodeRoleControl,
		Topology:             TopologySingle,
		ContainerMemoryBytes: container,
		ReservedMemoryBytes:  800,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("exact 20%% headroom must be valid: %v", err)
	}
	base.ReservedMemoryBytes++
	if err := base.Validate(); err == nil {
		t.Fatal("less than 20% headroom must be rejected")
	}
}

func TestBootstrapConfig_Normalized_PreservesExplicit(t *testing.T) {
	c := BootstrapConfig{
		BridgeID:          "b",
		ConfigFilePath:    "/f",
		AdminAPIKeyParam:  "/a",
		NodeRole:          NodeRoleWorker,
		Topology:          TopologyFilesystemReplicated,
		AdminAddr:         ":9090",
		MonitorAddr:       ":9091",
		TransportHTTPAddr: ":9092",
	}.Normalized()

	assertEqual(t, NodeRoleWorker, c.NodeRole)
	assertEqual(t, TopologyFilesystemReplicated, c.Topology)
	assertEqual(t, ":9090", c.AdminAddr)
}

func TestBootstrapConfig_Validate_RequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		cfg     BootstrapConfig
		wantErr string
	}{
		{"missing bridge_id", BootstrapConfig{ConfigFilePath: "/f", AdminAPIKeyParam: "/a", NodeRole: NodeRoleControl, Topology: TopologySingle}, "bridge_id"},
		{"missing config_file_path", BootstrapConfig{BridgeID: "b", AdminAPIKeyParam: "/a", NodeRole: NodeRoleControl, Topology: TopologySingle}, "config_file_path"},
		{"missing admin_api_key_param", BootstrapConfig{BridgeID: "b", ConfigFilePath: "/f", NodeRole: NodeRoleControl, Topology: TopologySingle}, "admin_api_key_param"},
		{"invalid node_role", BootstrapConfig{BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a", NodeRole: "bad", Topology: TopologySingle}, "unsupported node_role"},
		{"empty node_role rejected", BootstrapConfig{BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a", Topology: TopologySingle}, "unsupported node_role"},
		{"ssm_endpoint without dev_mode", BootstrapConfig{BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a", NodeRole: NodeRoleControl, Topology: TopologySingle, SSMEndpoint: "http://localhost:4566"}, "dev_mode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			assertContains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestBootstrapConfig_Validate_DynamoDBCoordinatedHA(t *testing.T) {
	c := BootstrapConfig{
		BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a",
		NodeRole: NodeRoleWorker, Topology: TopologyDynamoDBCoordinatedHA,
		ContainerMemoryBytes: DefaultContainerMemoryBytes,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("DynamoDB coordinated HA topology rejected: %v", err)
	}
}

func TestBootstrapConfig_Validate_OK(t *testing.T) {
	c := BootstrapConfig{
		BridgeID:         "b",
		ConfigFilePath:   "/f",
		AdminAPIKeyParam: "/a",
	}.Normalized()
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBootstrapConfig_Validate_DevModeWithSSMEndpoint(t *testing.T) {
	c := BootstrapConfig{
		BridgeID:         "b",
		ConfigFilePath:   "/f",
		AdminAPIKeyParam: "/a",
		SSMEndpoint:      "http://localhost:4566",
		DevMode:          true,
	}.Normalized()
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBootstrapConfig_EffectivePollInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval string
		want     time.Duration
	}{
		{"empty uses default", "", DefaultPollInterval},
		{"valid duration", "5s", 5 * time.Second},
		{"invalid falls back", "invalid", DefaultPollInterval},
		{"negative falls back", "-1s", DefaultPollInterval},
		{"zero falls back", "0s", DefaultPollInterval},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := BootstrapConfig{PollInterval: tc.interval}
			got := c.EffectivePollInterval()
			if got != tc.want {
				t.Errorf("EffectivePollInterval() = %v, want %v", got, tc.want)
			}
		})
	}
}

// helpers — no external test dependencies in the infra module
func assertEqual[T comparable](t *testing.T, want, got T) {
	t.Helper()
	if want != got {
		t.Errorf("got %v, want %v", got, want)
	}
}

func assertContains(t *testing.T, s, substr string) {
	t.Helper()
	if len(s) == 0 || len(substr) == 0 {
		t.Errorf("assertContains: empty string or substr")
		return
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return
		}
	}
	t.Errorf("string %q does not contain %q", s, substr)
}
