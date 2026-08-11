// Package infra contains shared deployment types used by both the CDK
// constructs (infrastructure-as-code) and the runtime bootstrap library.
// This module has zero external dependencies so CDK consumers can import
// it without pulling in the full runtime dependency tree.
package infra

import (
	"encoding/hex"
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
	TopologySingle                Topology = "single"
	TopologyFilesystemReplicated  Topology = "filesystem_replicated"
	TopologyDynamoDBCoordinatedHA Topology = "dynamodb_coordinated_ha"
)

const (
	DefaultAdminAddr                   = ":8080"
	DefaultMonitorAddr                 = ":8081"
	DefaultTransportHTTPAddr           = ":8082"
	DefaultPollInterval                = time.Second
	DefaultContainerMemoryBytes uint64 = 1 << 30
)

// DefaultCredentialPollInterval is the fallback poll cadence for the
// credential poll-based wrapper when CredentialPollInterval is unset. It
// MUST mirror runtime/credentials.DefaultCredentialPollInterval; the literal
// is duplicated because this package is intentionally zero-dependency and
// cannot import the runtime module (Finding 2).
const DefaultCredentialPollInterval = 5 * time.Minute

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
	// "worker"). The bootstrap App starts the transport, admin, and
	// monitor servers on every node regardless of this value, but it
	// DOES govern the admin config single-writer posture: only the
	// control (and the empty default, normalized to control) node
	// asserts httpapi.Config.ConfigSingleWriter, because it is the sole
	// durable writer of the config store (workers mount EFS read-only).
	// It is also consumed at deploy time by the CDK single/cluster
	// facades (per-service role + synth-time worker validation) and
	// surfaced to the container as the GOBRIDGE_NODE_ROLE env var.
	NodeRole NodeRole `json:"node_role,omitempty"`
	Topology Topology `json:"topology,omitempty"`

	// DynamoDBHA* fields are deployment-owned expectations stamped only by
	// GoBridgeDynamoDBHA. The runtime checks the EFS-loaded logical config
	// against these identities before planning stores or transports, preventing
	// a tampered/stale file from bypassing synth-time HA admission.
	DynamoDBHALeaseTableName                string `json:"dynamodb_ha_lease_table_name,omitempty"`
	DynamoDBHAOutboxTableName               string `json:"dynamodb_ha_outbox_table_name,omitempty"`
	DynamoDBHAManagedSubscriptionsTableName string `json:"dynamodb_ha_managed_subscriptions_table_name,omitempty"`
	DynamoDBHAConfigFingerprint             string `json:"dynamodb_ha_config_fingerprint,omitempty"`

	// DynamoDBHARolloutTableName names the DynamoDB table backing the coordinated
	// cluster rollout barrier's shared state (proposals, acks, the durable
	// last-committed config artifact). Empty selects the adapter default
	// ("gobridge-rollouts"). It is used only when the logical config opts into
	// bridge.cluster.rollout: coordinated.
	DynamoDBHARolloutTableName string `json:"dynamodb_ha_rollout_table_name,omitempty"`

	ConfigFilePath string `json:"config_file_path"`
	PollInterval   string `json:"poll_interval,omitempty"`

	// ContainerMemoryBytes is the hard memory limit of the runtime container.
	// CDK always overwrites it from the effective Fargate task memory; the
	// default mirrors the 1024 MiB task default for non-CDK bootstrap users.
	ContainerMemoryBytes uint64 `json:"container_memory_bytes,omitempty"`
	// ReservedMemoryBytes is non-MQTT memory the operator has committed to
	// other runtime components. MQTT ingress allocation plus this reservation
	// must leave at least 20 percent of ContainerMemoryBytes as headroom.
	ReservedMemoryBytes uint64 `json:"reserved_memory_bytes,omitempty"`

	// Credential poll-based wrapper knobs (Finding 2). These control how the
	// runtime lifts a pull-style credential store (SSM, file) into the
	// push-style rotation source that reaches long-lived transport sessions.
	// Exposing them here lets an operator shrink the auth-failure blast radius
	// of a hard credential rotation (default cadence is 5 minutes) without a
	// code change.
	//
	//   CredentialFilePath  - base directory backing file:// credential URIs.
	//                         Empty (default) registers NO file store; set it
	//                         to enable file:// credentials in this profile
	//                         (Finding 11). SSM (pms://) is always registered.
	//   CredentialPollInterval - poll cadence (e.g. "1m"); empty falls back to
	//                         DefaultCredentialPollInterval.
	//   CredentialPollJitter - +/- jitter applied per poll to de-synchronize a
	//                         fleet; empty defaults to ~10% of the interval so
	//                         many sessions do not stampede the backend on the
	//                         same tick (LOW-severity finding).
	//   CredentialEmitOnStart - when nil (default) the wrapper emits on start
	//                         so a rotation that landed in the build->watch
	//                         window is surfaced on the first tick rather than
	//                         silently baselined (Finding 1). Set to false only
	//                         to restore the legacy silent-baseline behaviour.
	CredentialFilePath     string `json:"credential_file_path,omitempty"`
	CredentialPollInterval string `json:"credential_poll_interval,omitempty"`
	CredentialPollJitter   string `json:"credential_poll_jitter,omitempty"`
	CredentialEmitOnStart  *bool  `json:"credential_emit_on_start,omitempty"`

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
	// per-task series in a fleet do not collide. Empty lets the
	// exporter derive a per-task identity ("<hostname>-<pid>"), which is
	// already unique per Fargate task; set it explicitly for a deterministic,
	// operator-chosen identity.
	InstanceID string `json:"instance_id,omitempty"`

	// MemberID is this node's STABLE identity within a coordinated cluster rollout
	// cohort (design cluster-config-rollout-protocol.md §6). It is required only
	// when the logical config opts into bridge.cluster.rollout: coordinated, and it
	// MUST appear verbatim in that config's bridge.cluster.members roster — the
	// barrier freezes the roster as its membership epoch and counts acknowledgements
	// against it, so a drifting or absent id aborts every rollout.
	//
	// Unlike InstanceID (a metrics dimension that may derive a per-task "<host>-<pid>"
	// value, which changes across restarts), MemberID MUST be restart-stable: it is
	// the cohort identity a restarted task rejoins under. The deployment supplies it
	// deterministically per node (e.g. a per-task ordinal or a stable DNS name); it
	// is empty for a non-coordinated deployment.
	MemberID string `json:"member_id,omitempty"`

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
	if out.ContainerMemoryBytes == 0 {
		out.ContainerMemoryBytes = DefaultContainerMemoryBytes
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

// EffectiveCredentialPollInterval returns the credential poll cadence as a
// time.Duration, falling back to DefaultCredentialPollInterval on parse error
// or non-positive values (Finding 2).
func (c BootstrapConfig) EffectiveCredentialPollInterval() time.Duration {
	if c.CredentialPollInterval == "" {
		return DefaultCredentialPollInterval
	}
	d, err := time.ParseDuration(c.CredentialPollInterval)
	if err != nil || d <= 0 {
		return DefaultCredentialPollInterval
	}
	return d
}

// EffectiveCredentialPollJitter returns the per-poll jitter as a
// time.Duration. When unset (or invalid) it defaults to ~10% of the effective
// credential poll interval so a fleet does not stampede the credential backend
// on the same tick (LOW-severity finding). A parseable zero disables jitter.
func (c BootstrapConfig) EffectiveCredentialPollJitter() time.Duration {
	if c.CredentialPollJitter == "" {
		return c.EffectiveCredentialPollInterval() / 10
	}
	d, err := time.ParseDuration(c.CredentialPollJitter)
	if err != nil || d < 0 {
		return c.EffectiveCredentialPollInterval() / 10
	}
	return d
}

// EffectiveCredentialEmitOnStart reports whether the credential poll-based
// wrapper should emit an initial rotation on start. It defaults to true so a
// rotation that landed in the build->watch window is surfaced on the first
// tick rather than silently adopted as the dedup baseline (Finding 1); set
// CredentialEmitOnStart to false to restore the legacy behaviour.
func (c BootstrapConfig) EffectiveCredentialEmitOnStart() bool {
	if c.CredentialEmitOnStart == nil {
		return true
	}
	return *c.CredentialEmitOnStart
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
	case TopologySingle, TopologyFilesystemReplicated, TopologyDynamoDBCoordinatedHA:
	default:
		return fmt.Errorf("infra: unsupported topology %q (call Normalized() first)", c.Topology)
	}

	if c.Topology == TopologyDynamoDBCoordinatedHA {
		if c.DynamoDBHALeaseTableName == "" || c.DynamoDBHAOutboxTableName == "" ||
			c.DynamoDBHAManagedSubscriptionsTableName == "" {
			return fmt.Errorf("infra: dynamodb_coordinated_ha requires deployment-owned lease, outbox, and managed-subscription table names")
		}
		if c.DynamoDBHALeaseTableName == c.DynamoDBHAOutboxTableName ||
			c.DynamoDBHALeaseTableName == c.DynamoDBHAManagedSubscriptionsTableName ||
			c.DynamoDBHAOutboxTableName == c.DynamoDBHAManagedSubscriptionsTableName {
			return fmt.Errorf("infra: dynamodb_coordinated_ha expected table names must be distinct")
		}
		fingerprint, err := hex.DecodeString(c.DynamoDBHAConfigFingerprint)
		if err != nil || len(fingerprint) != 32 {
			return fmt.Errorf("infra: dynamodb_coordinated_ha config fingerprint must be a 64-character SHA-256 hex value")
		}
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
	if c.ContainerMemoryBytes == 0 {
		return fmt.Errorf("infra: container_memory_bytes must be greater than zero (call Normalized() first)")
	}
	minimumHeadroom := c.ContainerMemoryBytes / 5
	if c.ContainerMemoryBytes%5 != 0 {
		minimumHeadroom++
	}
	if c.ReservedMemoryBytes > c.ContainerMemoryBytes-minimumHeadroom {
		return fmt.Errorf(
			"infra: reserved_memory_bytes %d leaves less than 20%% container headroom",
			c.ReservedMemoryBytes,
		)
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
