package model

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBootstrapConfig_Normalized_AppliesDefaults(t *testing.T) {
	c := BootstrapConfig{
		BridgeID:         "bridge-1",
		ConfigFilePath:   "/etc/bridge.yaml",
		AdminAPIKeyParam: "/admin",
	}.Normalized()

	assert.Equal(t, NodeRoleControl, c.NodeRole)
	assert.Equal(t, TopologySingle, c.Topology)
	assert.Equal(t, DefaultAdminAddr, c.AdminAddr)
	assert.Equal(t, DefaultMonitorAddr, c.MonitorAddr)
	assert.Equal(t, DefaultTransportHTTPAddr, c.TransportHTTPAddr)
	assert.NotNil(t, c.HTTPReceiverAPIKeyParams)
	assert.NotNil(t, c.HTTPSenderAPIKeyParams)
}

func TestBootstrapConfig_Normalized_PreservesExplicitValues(t *testing.T) {
	c := BootstrapConfig{
		BridgeID:          "bridge-1",
		ConfigFilePath:    "/etc/bridge.yaml",
		AdminAPIKeyParam:  "/admin",
		NodeRole:          NodeRoleWorker,
		Topology:          TopologyFilesystemReplicated,
		AdminAddr:         ":9090",
		MonitorAddr:       ":9091",
		TransportHTTPAddr: ":9092",
	}.Normalized()

	assert.Equal(t, NodeRoleWorker, c.NodeRole)
	assert.Equal(t, TopologyFilesystemReplicated, c.Topology)
	assert.Equal(t, ":9090", c.AdminAddr)
	assert.Equal(t, ":9091", c.MonitorAddr)
	assert.Equal(t, ":9092", c.TransportHTTPAddr)
}

func TestBootstrapConfig_Validate_RequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		cfg     BootstrapConfig
		wantErr string
	}{
		{
			name:    "missing bridge_id",
			cfg:     BootstrapConfig{ConfigFilePath: "/f", AdminAPIKeyParam: "/a", NodeRole: NodeRoleControl, Topology: TopologySingle},
			wantErr: "bridge_id",
		},
		{
			name:    "missing config_file_path",
			cfg:     BootstrapConfig{BridgeID: "b", AdminAPIKeyParam: "/a", NodeRole: NodeRoleControl, Topology: TopologySingle},
			wantErr: "config_file_path",
		},
		{
			name:    "missing admin_api_key_param",
			cfg:     BootstrapConfig{BridgeID: "b", ConfigFilePath: "/f", NodeRole: NodeRoleControl, Topology: TopologySingle},
			wantErr: "admin_api_key_param",
		},
		{
			name:    "invalid node_role",
			cfg:     BootstrapConfig{BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a", NodeRole: "invalid", Topology: TopologySingle},
			wantErr: "unsupported node_role",
		},
		{
			name:    "invalid topology",
			cfg:     BootstrapConfig{BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a", NodeRole: NodeRoleControl, Topology: "invalid"},
			wantErr: "unsupported topology",
		},
		{
			name:    "empty node_role rejected",
			cfg:     BootstrapConfig{BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a", Topology: TopologySingle},
			wantErr: "unsupported node_role",
		},
		{
			name:    "empty topology rejected",
			cfg:     BootstrapConfig{BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a", NodeRole: NodeRoleControl},
			wantErr: "unsupported topology",
		},
		{
			name:    "ssm_endpoint without dev_mode rejected",
			cfg:     BootstrapConfig{BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a", NodeRole: NodeRoleControl, Topology: TopologySingle, SSMEndpoint: "http://localhost:4566"},
			wantErr: "dev_mode",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestBootstrapConfig_Validate_OK(t *testing.T) {
	c := BootstrapConfig{
		BridgeID:         "bridge-1",
		ConfigFilePath:   "/etc/bridge.yaml",
		AdminAPIKeyParam: "/admin",
	}.Normalized()
	require.NoError(t, c.Validate())
}

func TestBootstrapConfig_Validate_DevModeWithSSMEndpoint(t *testing.T) {
	c := BootstrapConfig{
		BridgeID:         "bridge-1",
		ConfigFilePath:   "/etc/bridge.yaml",
		AdminAPIKeyParam: "/admin",
		SSMEndpoint:      "http://localhost:4566",
		DevMode:          true,
	}.Normalized()
	require.NoError(t, c.Validate())
}

func TestBootstrapConfig_EffectivePollInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval string
		want     time.Duration
	}{
		{name: "empty uses default", interval: "", want: DefaultPollInterval},
		{name: "valid duration", interval: "5s", want: 5 * time.Second},
		{name: "milliseconds", interval: "500ms", want: 500 * time.Millisecond},
		{name: "invalid falls back to default", interval: "invalid", want: DefaultPollInterval},
		{name: "negative falls back to default", interval: "-1s", want: DefaultPollInterval},
		{name: "zero falls back to default", interval: "0s", want: DefaultPollInterval},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := BootstrapConfig{PollInterval: tc.interval}
			assert.Equal(t, tc.want, c.EffectivePollInterval())
		})
	}
}
