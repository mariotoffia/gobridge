package model

import (
	"fmt"
	"time"
)

type NodeRole string

const (
	NodeRoleControl NodeRole = "control"
	NodeRoleWorker  NodeRole = "worker"
)

type Topology string

const (
	TopologySingle               Topology = "single"
	TopologyFilesystemReplicated Topology = "filesystem_replicated"
)

const (
	DefaultAdminAddr         = ":8080"
	DefaultMonitorAddr       = ":8081"
	DefaultTransportHTTPAddr = ":8082"
	DefaultPollInterval      = time.Second
)

// DefaultMountPath mirrors infra.DefaultMountPath: the single canonical EFS
// mount root for the file-based profile. Kept in sync with the CDK task-def
// mount and the Phase-1 store-path validator.
const DefaultMountPath = "/var/lib/gobridge"

// BootstrapConfig is deployment-owned runtime configuration for the
// file-based AWS deployment profile. It is separate from ports.BridgeConfig
// and is typically supplied via environment or a small bootstrap JSON file.
type BootstrapConfig struct {
	BridgeID string `json:"bridge_id"`

	NodeRole NodeRole `json:"node_role,omitempty"`
	Topology Topology `json:"topology,omitempty"`

	ConfigFilePath string `json:"config_file_path"`
	PollInterval   string `json:"poll_interval,omitempty"`

	AdminAddr         string `json:"admin_addr,omitempty"`
	MonitorAddr       string `json:"monitor_addr,omitempty"`
	CORSOrigins       string `json:"cors_origins,omitempty"`
	TransportHTTPAddr string `json:"transport_http_addr,omitempty"`

	AdminAPIKeyParam         string            `json:"admin_api_key_param,omitempty"`
	MonitorAPIKeyParam       string            `json:"monitor_api_key_param,omitempty"`
	HTTPReceiverAPIKeyParams map[string]string `json:"http_receiver_api_key_params,omitempty"`
	HTTPSenderAPIKeyParams   map[string]string `json:"http_sender_api_key_params,omitempty"`

	// Optional AWS overrides, primarily useful for tests and local emulation.
	AWSRegion   string `json:"aws_region,omitempty"`
	SSMEndpoint string `json:"ssm_endpoint,omitempty"`

	// DevMode enables local development features such as static test
	// credentials when SSMEndpoint is set. SSMEndpoint without DevMode
	// causes Validate to return an error to prevent accidental use of
	// custom endpoints in production.
	DevMode bool `json:"dev_mode,omitempty"`
}

func (c BootstrapConfig) Normalized() BootstrapConfig {
	out := c
	if out.NodeRole == "" {
		out.NodeRole = NodeRoleControl
	}
	if out.Topology == "" {
		out.Topology = TopologySingle
	}
	if out.AdminAddr == "" {
		out.AdminAddr = DefaultAdminAddr
	}
	if out.MonitorAddr == "" {
		out.MonitorAddr = DefaultMonitorAddr
	}
	if out.TransportHTTPAddr == "" {
		out.TransportHTTPAddr = DefaultTransportHTTPAddr
	}
	if out.HTTPReceiverAPIKeyParams == nil {
		out.HTTPReceiverAPIKeyParams = map[string]string{}
	}
	if out.HTTPSenderAPIKeyParams == nil {
		out.HTTPSenderAPIKeyParams = map[string]string{}
	}
	return out
}

func (c BootstrapConfig) EffectivePollInterval() time.Duration {
	if c.PollInterval == "" {
		return DefaultPollInterval
	}
	d, err := time.ParseDuration(c.PollInterval)
	if err != nil || d <= 0 {
		return DefaultPollInterval
	}
	return d
}

func (c BootstrapConfig) Validate() error {
	switch c.NodeRole {
	case NodeRoleControl, NodeRoleWorker:
	default:
		return fmt.Errorf("bootstrap: unsupported node_role %q (call Normalized() first)", c.NodeRole)
	}

	switch c.Topology {
	case TopologySingle, TopologyFilesystemReplicated:
	default:
		return fmt.Errorf("bootstrap: unsupported topology %q (call Normalized() first)", c.Topology)
	}

	if c.BridgeID == "" {
		return fmt.Errorf("bootstrap: bridge_id is required")
	}
	if c.ConfigFilePath == "" {
		return fmt.Errorf("bootstrap: config_file_path is required")
	}
	if c.AdminAPIKeyParam == "" {
		return fmt.Errorf("bootstrap: admin_api_key_param is required")
	}
	if c.SSMEndpoint != "" && !c.DevMode {
		return fmt.Errorf("bootstrap: ssm_endpoint requires dev_mode to be true; refusing to use a custom SSM endpoint without explicit dev_mode flag")
	}
	return nil
}
