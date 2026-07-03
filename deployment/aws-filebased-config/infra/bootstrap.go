// Package infra contains shared deployment types used by both the CDK
// constructs (infrastructure-as-code) and the runtime bootstrap library.
// This module has zero external dependencies so CDK consumers can import
// it without pulling in the full runtime dependency tree.
package infra

import (
	"fmt"
	"time"
)

// NodeRole identifies the role of a bridge node in a multi-node deployment.
type NodeRole string

const (
	NodeRoleControl NodeRole = "control"
	NodeRoleWorker  NodeRole = "worker"
)

// Topology describes how multiple bridge replicas share configuration.
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

// DefaultMountPath is the SINGLE canonical container directory where the
// deployment mounts EFS and where the runtime reads/writes bridge.yaml plus
// outbox/DLQ state. It is FHS-conformant for runtime state.
//
// This constant is the one source of truth shared by the CDK task-def mount
// (internal/gobridgebase), the Phase-1 store-path validator
// (internal/validation), and ServiceProps normalization. Keeping them
// byte-identical is REQUIRED: a validator/mount mismatch either rejects a
// correct config at synth or silently validates a store path that then writes
// to ephemeral Fargate storage (outbox/DLQ durability lost on task replace).
const DefaultMountPath = "/var/lib/gobridge"

// DefaultBridgeYamlName is the file the seeder writes onto EFS and the runtime
// watches. Stable so admin tooling can reference a well-known path.
const DefaultBridgeYamlName = "bridge.yaml"

// Metrics exporter selectors for BootstrapConfig.MetricsExporter. An empty
// value is equivalent to MetricsExporterNoop: the runtime emits no metrics
// (today's default behaviour). MetricsExporterCloudWatch selects the
// adapters/aws/metrics/cloudwatch exporter, wired by the bootstrap App.
const (
	MetricsExporterNoop       = "noop"
	MetricsExporterCloudWatch = "cloudwatch"
)

// DefaultMetricsNamespace is the CloudWatch namespace the runtime publishes
// to when MetricsExporter=cloudwatch and MetricsNamespace is unset. It MUST
// mirror domain/shared.MetricNamespace so the exporter, the IAM
// cloudwatch:namespace condition, and the declarative alarms all agree; the
// literal is duplicated here because this package is intentionally
// zero-dependency and cannot import domain/shared.
const DefaultMetricsNamespace = "GoBridge/Runtime"

// BootstrapConfig is deployment-owned runtime configuration for the
// file-based AWS deployment profile. It is separate from ports.BridgeConfig
// and is typically supplied via environment or a small bootstrap JSON file.
type BootstrapConfig struct {
	BridgeID string `json:"bridge_id"`

	// NodeRole is recorded and validated (must be "control" or
	// "worker") but is RESERVED / non-operative at runtime: the
	// bootstrap App starts the transport, admin, and monitor servers
	// on every node regardless of this value. It is consumed only at
	// deploy time by the CDK single/cluster facades (per-service role
	// + synth-time worker validation) and surfaced to the container as
	// the GOBRIDGE_NODE_ROLE env var. Reserved for future multi-node
	// coordination.
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

	// MetricsExporter selects the runtime metrics backend. "" or "noop"
	// (the default) emits nothing; "cloudwatch" publishes runtime metrics
	// to CloudWatch via the adapters/aws/metrics/cloudwatch exporter. Any
	// other value fails Validate (fail fast). When cloudwatch is selected
	// the CDK base grants cloudwatch:PutMetricData scoped to
	// EffectiveMetricsNamespace.
	MetricsExporter string `json:"metrics_exporter,omitempty"`
	// MetricsNamespace overrides the CloudWatch namespace used when
	// MetricsExporter=cloudwatch. Empty defaults to DefaultMetricsNamespace.
	MetricsNamespace string `json:"metrics_namespace,omitempty"`
	// InstanceID stamps the per-instance "instance_id" metric dimension so
	// per-task series in a fleet do not collide (MF-8). Empty lets the
	// exporter derive a per-task identity ("<hostname>-<pid>"), which is
	// already unique per Fargate task; set it explicitly for a deterministic,
	// operator-chosen identity.
	InstanceID string `json:"instance_id,omitempty"`

	// DevMode enables local development features such as static test
	// credentials when SSMEndpoint is set. SSMEndpoint without DevMode
	// causes Validate to return an error to prevent accidental use of
	// custom endpoints in production.
	DevMode bool `json:"dev_mode,omitempty"`
}

// Normalized returns a copy with defaults applied for any unset fields.
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

// EffectivePollInterval returns the config file poll interval as a
// time.Duration, falling back to DefaultPollInterval on parse error
// or non-positive values.
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

// Validate checks that all required fields are set and enum values are valid.
// Call Normalized() before Validate() to fill in defaults.
func (c BootstrapConfig) Validate() error {
	switch c.NodeRole {
	case NodeRoleControl, NodeRoleWorker:
	default:
		return fmt.Errorf("infra: unsupported node_role %q (call Normalized() first)", c.NodeRole)
	}

	switch c.Topology {
	case TopologySingle, TopologyFilesystemReplicated:
	default:
		return fmt.Errorf("infra: unsupported topology %q (call Normalized() first)", c.Topology)
	}

	if c.BridgeID == "" {
		return fmt.Errorf("infra: bridge_id is required")
	}
	if c.ConfigFilePath == "" {
		return fmt.Errorf("infra: config_file_path is required")
	}
	if c.AdminAPIKeyParam == "" {
		return fmt.Errorf("infra: admin_api_key_param is required")
	}
	if c.SSMEndpoint != "" && !c.DevMode {
		return fmt.Errorf("infra: ssm_endpoint requires dev_mode to be true; refusing to use a custom SSM endpoint without explicit dev_mode flag")
	}

	switch c.MetricsExporter {
	case "", MetricsExporterNoop, MetricsExporterCloudWatch:
	default:
		return fmt.Errorf("infra: unsupported metrics_exporter %q (want \"\", %q, or %q)", c.MetricsExporter, MetricsExporterNoop, MetricsExporterCloudWatch)
	}
	return nil
}

// MetricsExporterEnabled reports whether a real (non-noop) metrics exporter
// is selected. Used by the CDK base to condition the cloudwatch:PutMetricData
// grant (least privilege: no grant when the exporter is off).
func (c BootstrapConfig) MetricsExporterEnabled() bool {
	return c.MetricsExporter == MetricsExporterCloudWatch
}

// EffectiveMetricsNamespace returns the CloudWatch namespace the runtime
// publishes to (and that the IAM condition + declarative alarms must match),
// applying DefaultMetricsNamespace when MetricsNamespace is unset.
func (c BootstrapConfig) EffectiveMetricsNamespace() string {
	if c.MetricsNamespace != "" {
		return c.MetricsNamespace
	}
	return DefaultMetricsNamespace
}
