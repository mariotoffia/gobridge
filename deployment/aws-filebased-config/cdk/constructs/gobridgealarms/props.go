package gobridgealarms

import (
	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscloudwatch"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	"github.com/aws/constructs-go/constructs/v10"

	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
)

// AlarmsProps configures the GoBridgeAlarms bundle. Exactly one of
// Single, Cluster or DynamoDBHA MUST be supplied. AlarmTopic is required;
// Efs is required only when the facade uses a filesystem. Attachment is optional — when nil the ALB-related
// alarms are skipped (Single deployments without an ALB still get
// cluster + EFS alarms).
type AlarmsProps struct {
	Single     *gobridgesingle.GoBridgeSingle
	Cluster    *gobridgecluster.GoBridgeCluster
	DynamoDBHA *gobridgedynamodbha.GoBridgeDynamoDBHA

	Efs        *cdkconstructs.GoBridgeEfsConfig
	Attachment *gobridgealbattachment.GoBridgeALBAttachment

	AlarmTopic awssns.ITopic

	Period      awscdk.Duration
	Evaluations *float64

	EfsPercentIOLimitThreshold *float64
	Alb5xxThreshold            *float64

	DisableControlAbsence bool
	DisableWorkerDegraded bool
	DisableEfsIO          bool
	DisableAlbUnhealthy   bool
	DisableAlb5xx         bool

	// EnableRollupAlarms opts in to alarms on the custom runtime rollup
	// metrics (OutboxDepth, DLQEntries, LeaseExpiries, LeaseAcquireFailures)
	// published by the cloudwatch metrics exporter when
	// BootstrapConfig.MetricsExporter=cloudwatch with
	// WithRollupMetrics(DefaultRollupMetrics()...). OFF by default: a
	// deployment without that exporter emits no such metrics and the alarms
	// would sit in INSUFFICIENT_DATA. The alarms carry NO dimensions and so
	// only match the zero-dimension rollup series the exporter
	// double-publishes. They publish to AlarmTopic like every other
	// alarm in the bundle.
	EnableRollupAlarms bool

	// RollupMetricsNamespace overrides the CloudWatch namespace the rollup
	// alarms read. Empty defaults to rollupNamespaceDefault; it MUST equal
	// BootstrapConfig.EffectiveMetricsNamespace() (the namespace the exporter
	// publishes to) or the rollup alarms never leave INSUFFICIENT_DATA.
	RollupMetricsNamespace *string

	// OutboxDepthThreshold overrides the OutboxDepth alarm threshold
	// (default 1000). LeaseAcquireFailuresThreshold overrides the
	// LeaseAcquireFailures alarm threshold (default 3).
	OutboxDepthThreshold          *float64
	LeaseAcquireFailuresThreshold *float64

	// EnableClusterRolloutAlarms opts in to the fleet convergence alarms for a
	// cohort running bridge.cluster.rollout: coordinated. OFF by default, because
	// a deployment without the barrier emits none of these series. It is
	// independent of the deployment shape — any composition root can drive the
	// barrier, so these are not tied to one facade.
	//
	// They are the alarms the rollout contract requires. The barrier is atomic
	// BEFORE the commit and per-member AFTER it, so the cohort's shared rollout
	// row reads "committed" identically on a member that swapped and on one whose
	// swap failed — no signal derived from that row can tell them apart. These
	// three read the PER-MEMBER series instead, rolled up to the fleet, and answer
	// the three questions the post-commit window raises: is anyone not running the
	// decided generation, can anyone no longer repair itself, and is anyone no
	// longer able to see the row at all.
	//
	// Like every other rollup alarm here they carry NO dimensions, so the exporter
	// must be configured with WithRollupMetrics(DefaultRollupMetrics()...) —
	// otherwise they sit in INSUFFICIENT_DATA on a fleet with instance tagging.
	EnableClusterRolloutAlarms bool
}

// GoBridgeAlarms is the bundle construct exposing each generated
// CloudWatch alarm. Accessors return nil when the corresponding
// alarm was skipped (disabled or not applicable for the deployment
// shape).
type GoBridgeAlarms struct {
	constructs.Construct

	controlAbsence       awscloudwatch.IAlarm
	workerDegraded       awscloudwatch.IAlarm
	workerDegradedAlarms []awscloudwatch.IAlarm
	efsIO                awscloudwatch.IAlarm
	albUnhealthyCtrl     awscloudwatch.IAlarm
	albUnhealthyWrk      awscloudwatch.IAlarm
	alb5xxCtrl           awscloudwatch.IAlarm
	alb5xxWrk            awscloudwatch.IAlarm

	outboxDepth          awscloudwatch.IAlarm
	dlqEntries           awscloudwatch.IAlarm
	leaseExpiries        awscloudwatch.IAlarm
	leaseAcquireFailures awscloudwatch.IAlarm

	warmStandbyUnavailable awscloudwatch.IAlarm
	failureToFullDuration  awscloudwatch.IAlarm
	dynamoThrottles        []awscloudwatch.IAlarm
	dynamoSystemErrors     []awscloudwatch.IAlarm
	leaseTransfers         awscloudwatch.IAlarm
	outboxDrainLatency     awscloudwatch.IAlarm
	outboxDepthFailures    awscloudwatch.IAlarm
	outboxRecordFailures   awscloudwatch.IAlarm
	outboxDrainStalled     awscloudwatch.IAlarm
	dlqDepth               awscloudwatch.IAlarm
	dlqWriteFailures       awscloudwatch.IAlarm

	mqttIngressPoison   awscloudwatch.IAlarm
	reconcileFailures   awscloudwatch.IAlarm
	mqttSessionTakeover awscloudwatch.IAlarm
	mqttQoSDowngraded   awscloudwatch.IAlarm

	clusterRolloutDiverged       awscloudwatch.IAlarm
	clusterRolloutTerminal       awscloudwatch.IAlarm
	clusterRolloutObservationAge awscloudwatch.IAlarm
}

const (
	// rollupNamespaceDefault mirrors infra.DefaultMetricsNamespace /
	// domain/shared.MetricNamespace: the namespace the runtime exporter
	// publishes to. Duplicated as a literal to avoid a dependency edge from
	// the CDK constructs onto the runtime domain module.
	rollupNamespaceDefault = "GoBridge/Runtime"

	// Rollup metric names mirror domain/shared.Metric* (the strings the
	// exporter emits). The rollup alarms match the zero-dimension copies.
	metricOutboxDepth          = "OutboxDepth"
	metricDLQEntries           = "DLQEntries"
	metricLeaseExpiries        = "LeaseExpiries"
	metricLeaseAcquireFailures = "LeaseAcquireFailures"
	metricLeaseTransfers       = "LeaseTransfers"
	metricOutboxDrainLatency   = "OutboxDrainLatency"
	metricOutboxDepthFailures  = "OutboxDepthFailures"
	metricOutboxRecordFailures = "OutboxRecordFailures"
	metricOutboxDrainStalled   = "OutboxDrainStalled"
	metricDLQDepth             = "DLQDepth"
	metricDLQWriteFailures     = "DLQWriteFailures"
	// MQTT rollup metric names (mirror adapters/mqtt/.../metrics.go). Kept as
	// literals because the CDK constructs must not depend on an adapter module.
	// The MQTT docs instruct operators to alert on all four.
	metricMQTTIngressPoisonDropped = "MQTTIngressPoisonDropped"
	metricReconcileFailures        = "ReconcileFailures"
	metricMQTTSessionTakeover      = "MQTTSessionTakeover"
	metricMQTTQoSDowngraded        = "MQTTQoSDowngraded"
)

// FailureToFullMetricName is emitted only by the credentialed external failover probe.
const FailureToFullMetricName = "FailureToFullDuration"
