package infra

import (
	"encoding/json"
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
	assertEqual(t, ConfigSourceFile, c.ConfigSource)
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
		ContainerMemoryBytes:     DefaultContainerMemoryBytes,
		DynamoDBHALeaseTableName: "leases", DynamoDBHAOutboxTableName: "outbox",
		DynamoDBHAManagedSubscriptionsTableName: "history",
		DynamoDBHAConfigFingerprint:             "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("DynamoDB coordinated HA topology rejected: %v", err)
	}
}

func TestBootstrapConfig_Validate_DynamoDBCoordinatedHARequiresExpectations(t *testing.T) {
	c := BootstrapConfig{
		BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a",
		NodeRole: NodeRoleWorker, Topology: TopologyDynamoDBCoordinatedHA,
		ContainerMemoryBytes: DefaultContainerMemoryBytes,
	}
	if err := c.Validate(); err == nil {
		t.Fatal("DynamoDB coordinated HA without deployment-owned expectations must be rejected")
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

func TestValidate_ConfigSourceMatrix(t *testing.T) {
	tests := []struct {
		name    string
		fields  string
		wantErr string
	}{
		{"empty defaults to file", `{}`, ""},
		{"file", `{"config_source":"file"}`, ""},
		{"default file requires path", `{"config_file_path":""}`, "config_file_path is required"},
		{"file requires path", `{"config_source":"file","config_file_path":""}`, "config_file_path is required"},
		{"file rejects dynamodb settings", `{"config_source":"file","config_dynamodb":{}}`, "config_dynamodb must be absent"},
		{"default file rejects dynamodb settings", `{"config_dynamodb":{}}`, "config_dynamodb must be absent"},
		{"unknown source", `{"config_source":"s3"}`, "unsupported config_source"},
		{"dynamodb requires settings", `{"config_source":"dynamodb","config_file_path":""}`, "config_dynamodb.table_name is required"},
		{"dynamodb requires table", `{"config_source":"dynamodb","config_file_path":"","config_dynamodb":{}}`, "config_dynamodb.table_name is required"},
		{"dynamodb rejects file path", `{"config_source":"dynamodb","config_dynamodb":{"table_name":"config"}}`, "config_file_path must be empty"},
		{"dynamodb rejects filesystem topology", `{"config_source":"dynamodb","config_file_path":"","config_dynamodb":{"table_name":"config"},"topology":"filesystem_replicated"}`, "filesystem_replicated"},
		{"dynamodb single", `{"config_source":"dynamodb","config_file_path":"","config_dynamodb":{"table_name":"config"}}`, ""},
		{"dynamodb ha", `{"config_source":"dynamodb","config_file_path":"","config_dynamodb":{"table_name":"config"},"topology":"dynamodb_coordinated_ha"}`, ""},
		{"dynamodb requires bridge id", `{"config_source":"dynamodb","config_file_path":"","config_dynamodb":{"table_name":"config"},"bridge_id":""}`, "bridge_id is required"},
		{"dynamodb poll", `{"config_source":"dynamodb","config_file_path":"","config_dynamodb":{"table_name":"config","watch_mode":"poll"}}`, ""},
		{"dynamodb streams", `{"config_source":"dynamodb","config_file_path":"","config_dynamodb":{"table_name":"config","watch_mode":"streams","stream_poll_interval":"250ms"}}`, ""},
		{"dynamodb unknown watch mode", `{"config_source":"dynamodb","config_file_path":"","config_dynamodb":{"table_name":"config","watch_mode":"invalid"}}`, "unsupported config_dynamodb.watch_mode"},
		{"dynamodb invalid stream interval", `{"config_source":"dynamodb","config_file_path":"","config_dynamodb":{"table_name":"config","watch_mode":"streams","stream_poll_interval":"invalid"}}`, "config_dynamodb.stream_poll_interval must be a positive duration"},
		{"dynamodb zero stream interval", `{"config_source":"dynamodb","config_file_path":"","config_dynamodb":{"table_name":"config","watch_mode":"streams","stream_poll_interval":"0s"}}`, "config_dynamodb.stream_poll_interval must be a positive duration"},
		{"dynamodb negative stream interval", `{"config_source":"dynamodb","config_file_path":"","config_dynamodb":{"table_name":"config","watch_mode":"streams","stream_poll_interval":"-1s"}}`, "config_dynamodb.stream_poll_interval must be a positive duration"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := BootstrapConfig{
				BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a",
				DynamoDBHALeaseTableName: "leases", DynamoDBHAOutboxTableName: "outbox",
				DynamoDBHAManagedSubscriptionsTableName: "history",
				DynamoDBHAConfigFingerprint:             "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			}
			if err := json.Unmarshal([]byte(tc.fields), &cfg); err != nil {
				t.Fatal(err)
			}
			err := cfg.Normalized().Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected validation error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error containing %q", tc.wantErr)
				}
				assertContains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestEffectivePollInterval_DynamoDBDefault(t *testing.T) {
	assertEqual(t, 30*time.Second, DefaultDynamoDBPollInterval)
	for _, interval := range []string{"", "invalid", "0s", "-1s", "5s"} {
		t.Run(interval, func(t *testing.T) {
			cfg := BootstrapConfig{PollInterval: interval}
			if err := json.Unmarshal([]byte(`{"config_source":"dynamodb"}`), &cfg); err != nil {
				t.Fatal(err)
			}
			assertEqual(t, ConfigSourceDynamoDB, cfg.Normalized().ConfigSource)
			want := 30 * time.Second
			if interval == "5s" {
				want = 5 * time.Second
			}
			assertEqual(t, want, cfg.EffectivePollInterval())
			assertEqual(t, want, cfg.Normalized().EffectivePollInterval())
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
