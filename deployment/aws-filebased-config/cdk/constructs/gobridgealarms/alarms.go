// Package gobridgealarms provides the GoBridgeAlarms bundle construct
// that wires CloudWatch alarms covering the gobridge ECS workloads,
// EFS file system and (optionally) ALB target groups to a single
// supplied SNS topic.
//
// The bundle is intentionally opinionated: it materialises the alarms
// listed in the design under "Alarms (GoBridgeAlarms)" with sensible
// defaults and per-alarm opt-out switches/threshold overrides.
//
// Dependency: the ControlAbsence and WorkerDegraded alarms read the
// Container Insights metrics RunningTaskCount / DesiredTaskCount from
// the ECS/ContainerInsights namespace. The GoBridgeSingle and
// GoBridgeCluster facades enable Container Insights on their
// auto-created clusters. When a user-supplied cluster is passed via
// SingleProps.Cluster / ClusterProps.Cluster the caller is
// responsible for enabling Container Insights, otherwise these
// alarms will sit in INSUFFICIENT_DATA / treated as breaching.
package gobridgealarms

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscloudwatch"
	cwactions "github.com/aws/aws-cdk-go/awscdk/v2/awscloudwatchactions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsdynamodb"
	awsecs "github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
)

// AlarmsProps configures the GoBridgeAlarms bundle. Exactly one of
// Single or Cluster MUST be supplied. Efs and AlarmTopic are
// required. Attachment is optional — when nil the ALB-related
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

	controlAbsence   awscloudwatch.IAlarm
	workerDegraded   awscloudwatch.IAlarm
	efsIO            awscloudwatch.IAlarm
	albUnhealthyCtrl awscloudwatch.IAlarm
	albUnhealthyWrk  awscloudwatch.IAlarm
	alb5xxCtrl       awscloudwatch.IAlarm
	alb5xxWrk        awscloudwatch.IAlarm

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
	// MQTT rollup metric names (mirror adapters/mqtt/.../metrics.go). The MQTT
	// docs instruct operators to alert on these; wiring them here closes the gap
	// where the bundle carried none of them (finding §8 alarms.go).
	metricMQTTIngressPoisonDropped = "MQTTIngressPoisonDropped"
	metricReconcileFailures        = "ReconcileFailures"
	metricMQTTSessionTakeover      = "MQTTSessionTakeover"
	metricMQTTQoSDowngraded        = "MQTTQoSDowngraded"
)

// FailureToFullMetricName is emitted only by the credentialed external failover probe.
const FailureToFullMetricName = "FailureToFullDuration"

// NewGoBridgeAlarms wires the alarm bundle into scope.
func NewGoBridgeAlarms(scope constructs.Construct, id *string, props *AlarmsProps) *GoBridgeAlarms {
	validateAlarmsProps(props)

	c := constructs.NewConstruct(scope, id)
	g := &GoBridgeAlarms{Construct: c}

	period := props.Period
	if period == nil {
		period = awscdk.Duration_Minutes(jsii.Number(1))
	}
	evals := props.Evaluations
	if evals == nil {
		evals = jsii.Number(5)
	}

	clusterName := resolveClusterName(props)
	controlServiceName, workerServiceName := resolveServiceNames(props)

	topicAction := cwactions.NewSnsAction(props.AlarmTopic)

	if !props.DisableControlAbsence {
		metric := awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace:  jsii.String("ECS/ContainerInsights"),
			MetricName: jsii.String("RunningTaskCount"),
			DimensionsMap: &map[string]*string{
				"ServiceName": controlServiceName,
				"ClusterName": clusterName,
			},
			Statistic: jsii.String("Maximum"),
			Period:    period,
		})
		alarm := awscloudwatch.NewAlarm(c, jsii.String("ControlAbsence"), &awscloudwatch.AlarmProps{
			Metric:             metric,
			Threshold:          jsii.Number(1),
			EvaluationPeriods:  evals,
			ComparisonOperator: awscloudwatch.ComparisonOperator_LESS_THAN_THRESHOLD,
			TreatMissingData:   awscloudwatch.TreatMissingData_BREACHING,
			AlarmDescription:   jsii.String("GoBridge control-plane has zero running tasks (control absence)."),
		})
		alarm.AddAlarmAction(topicAction)
		alarm.AddOkAction(topicAction)
		g.controlAbsence = alarm
	}

	if (props.Cluster != nil || props.DynamoDBHA != nil) && !props.DisableWorkerDegraded {
		running := awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace:  jsii.String("ECS/ContainerInsights"),
			MetricName: jsii.String("RunningTaskCount"),
			DimensionsMap: &map[string]*string{
				"ServiceName": workerServiceName,
				"ClusterName": clusterName,
			},
			Statistic: jsii.String("Minimum"),
			Period:    period,
		})
		desired := awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace:  jsii.String("ECS/ContainerInsights"),
			MetricName: jsii.String("DesiredTaskCount"),
			DimensionsMap: &map[string]*string{
				"ServiceName": workerServiceName,
				"ClusterName": clusterName,
			},
			Statistic: jsii.String("Maximum"),
			Period:    period,
		})
		expr := awscloudwatch.NewMathExpression(&awscloudwatch.MathExpressionProps{
			Expression: jsii.String("IF(running < desired, 1, 0)"),
			UsingMetrics: &map[string]awscloudwatch.IMetric{
				"running": running,
				"desired": desired,
			},
			Period: period,
			Label:  jsii.String("WorkerCapacityDegraded"),
		})
		alarm := awscloudwatch.NewAlarm(c, jsii.String("WorkerDegraded"), &awscloudwatch.AlarmProps{
			Metric:             expr,
			Threshold:          jsii.Number(1),
			EvaluationPeriods:  evals,
			ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_OR_EQUAL_TO_THRESHOLD,
			TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
			AlarmDescription:   jsii.String("GoBridge worker service running task count below desired count."),
		})
		alarm.AddAlarmAction(topicAction)
		alarm.AddOkAction(topicAction)
		g.workerDegraded = alarm
	}

	if props.DynamoDBHA != nil {
		controlRunning := awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace: jsii.String("ECS/ContainerInsights"), MetricName: jsii.String("RunningTaskCount"),
			DimensionsMap: &map[string]*string{"ServiceName": controlServiceName, "ClusterName": clusterName},
			Statistic:     jsii.String("Minimum"), Period: period,
		})
		workerRunning := awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace: jsii.String("ECS/ContainerInsights"), MetricName: jsii.String("RunningTaskCount"),
			DimensionsMap: &map[string]*string{"ServiceName": workerServiceName, "ClusterName": clusterName},
			Statistic:     jsii.String("Minimum"), Period: period,
		})
		warm := awscloudwatch.NewMathExpression(&awscloudwatch.MathExpressionProps{
			Expression:   jsii.String("IF(control + workers < 2, 1, 0)"),
			UsingMetrics: &map[string]awscloudwatch.IMetric{"control": controlRunning, "workers": workerRunning},
			Period:       period, Label: jsii.String("WarmStandbyUnavailable"),
		})
		alarm := awscloudwatch.NewAlarm(c, jsii.String("WarmStandbyUnavailable"), &awscloudwatch.AlarmProps{
			Metric: warm, Threshold: jsii.Number(1), EvaluationPeriods: evals,
			ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_OR_EQUAL_TO_THRESHOLD,
			TreatMissingData:   awscloudwatch.TreatMissingData_BREACHING,
			AlarmDescription:   jsii.String("GoBridge coordinated HA has fewer than two running tasks, so no warm standby is guaranteed."),
		})
		alarm.AddAlarmAction(topicAction)
		alarm.AddOkAction(topicAction)
		g.warmStandbyUnavailable = alarm
	}

	if !props.DisableEfsIO {
		threshold := jsii.Number(90)
		if props.EfsPercentIOLimitThreshold != nil {
			threshold = props.EfsPercentIOLimitThreshold
		}
		metric := awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace:  jsii.String("AWS/EFS"),
			MetricName: jsii.String("PercentIOLimit"),
			DimensionsMap: &map[string]*string{
				"FileSystemId": props.Efs.FileSystem().FileSystemId(),
			},
			Statistic: jsii.String("Average"),
			Period:    period,
		})
		alarm := awscloudwatch.NewAlarm(c, jsii.String("EfsPercentIOLimit"), &awscloudwatch.AlarmProps{
			Metric:             metric,
			Threshold:          threshold,
			EvaluationPeriods:  evals,
			ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
			TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
			AlarmDescription:   jsii.String("GoBridge EFS file system PercentIOLimit saturation."),
		})
		alarm.AddAlarmAction(topicAction)
		alarm.AddOkAction(topicAction)
		g.efsIO = alarm
	}

	if props.Attachment != nil {
		ctrlTG := props.Attachment.ControlTargetGroup()
		wrkTG := props.Attachment.WorkerTargetGroup()

		if !props.DisableAlbUnhealthy {
			g.albUnhealthyCtrl = newUnhealthyAlarm(
				c, "AlbUnhealthyControl", ctrlTG, period, evals, topicAction,
				"GoBridge ALB control target group has unhealthy hosts.",
			)
			g.albUnhealthyWrk = newUnhealthyAlarm(
				c, "AlbUnhealthyWorker", wrkTG, period, evals, topicAction,
				"GoBridge ALB worker target group has unhealthy hosts.",
			)
		}

		if !props.DisableAlb5xx {
			th := jsii.Number(5)
			if props.Alb5xxThreshold != nil {
				th = props.Alb5xxThreshold
			}
			g.alb5xxCtrl = new5xxAlarm(
				c, "Alb5xxControl", ctrlTG, period, evals, th, topicAction,
				"GoBridge ALB control target group 5xx response rate exceeded threshold.",
			)
			g.alb5xxWrk = new5xxAlarm(
				c, "Alb5xxWorker", wrkTG, period, evals, th, topicAction,
				"GoBridge ALB worker target group 5xx response rate exceeded threshold.",
			)
		}
	}

	if props.EnableRollupAlarms && props.DynamoDBHA == nil {
		ns := rollupNamespaceDefault
		if props.RollupMetricsNamespace != nil && *props.RollupMetricsNamespace != "" {
			ns = *props.RollupMetricsNamespace
		}
		outboxTh := jsii.Number(1000)
		if props.OutboxDepthThreshold != nil {
			outboxTh = props.OutboxDepthThreshold
		}
		leaseFailTh := jsii.Number(3)
		if props.LeaseAcquireFailuresThreshold != nil {
			leaseFailTh = props.LeaseAcquireFailuresThreshold
		}

		// OutboxDepth is emitted continuously while an outbox is configured,
		// so silence means the drainer/bridge is dead -> BREACHING.
		g.outboxDepth = newRollupAlarm(c, "OutboxDepth", ns, metricOutboxDepth,
			"Maximum", outboxTh, period, evals, topicAction,
			awscloudwatch.TreatMissingData_BREACHING,
			"GoBridge outbox depth (fleet rollup) exceeded threshold — drainer backlog or stalled bridge.")

		// Event counters: no events is the healthy state -> NOT_BREACHING.
		g.dlqEntries = newRollupAlarm(c, "DLQEntries", ns, metricDLQEntries,
			"Sum", jsii.Number(0), period, evals, topicAction,
			awscloudwatch.TreatMissingData_NOT_BREACHING,
			"GoBridge dead-letter entries (fleet rollup) observed.")

		g.leaseExpiries = newRollupAlarm(c, "LeaseExpiries", ns, metricLeaseExpiries,
			"Sum", jsii.Number(0), period, evals, topicAction,
			awscloudwatch.TreatMissingData_NOT_BREACHING,
			"GoBridge lease expiries (fleet rollup) observed — sessions lost their exclusive lease.")

		g.leaseAcquireFailures = newRollupAlarm(c, "LeaseAcquireFailures", ns, metricLeaseAcquireFailures,
			"Sum", leaseFailTh, period, evals, topicAction,
			awscloudwatch.TreatMissingData_NOT_BREACHING,
			"GoBridge lease-acquire failures (fleet rollup) exceeded threshold.")
	}

	if props.DynamoDBHA != nil {
		ns := props.DynamoDBHA.MetricsNamespace()
		objectiveMS := float64(props.DynamoDBHA.FailoverObjective().Milliseconds())

		// FailureToFullDuration is emitted by the external credentialed probe,
		// never by a task that may itself be dead. Missing samples stay healthy;
		// release proof separately requires the exact sample to exist.
		durationMetric := awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace: jsii.String(ns), MetricName: jsii.String(FailureToFullMetricName),
			Statistic: jsii.String("Maximum"), Period: period,
			Unit: awscloudwatch.Unit_MILLISECONDS,
		})
		durationAlarm := awscloudwatch.NewAlarm(c, jsii.String("FailureToFullDuration"), &awscloudwatch.AlarmProps{
			Metric: durationMetric, Threshold: jsii.Number(objectiveMS), EvaluationPeriods: jsii.Number(1),
			ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
			TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
			AlarmDescription:   jsii.String("External verified-holder failure-to-ServiceLevelFull duration exceeded the declared profile objective."),
		})
		durationAlarm.AddAlarmAction(topicAction)
		durationAlarm.AddOkAction(topicAction)
		g.failureToFullDuration = durationAlarm

		data := props.DynamoDBHA.Data()
		for _, table := range []struct {
			name  string
			table awsdynamodb.ITable
		}{
			{name: "Lease", table: data.LeaseTable()},
			{name: "Outbox", table: data.OutboxTable()},
			{name: "ManagedSubscriptions", table: data.ManagedSubscriptionsTable()},
		} {
			throttle, system := newDynamoDBAlarms(c, table.name, table.table, period, evals, topicAction)
			g.dynamoThrottles = append(g.dynamoThrottles, throttle)
			g.dynamoSystemErrors = append(g.dynamoSystemErrors, system)
		}

		g.outboxDepth = newRollupAlarm(c, "HAOutboxDepth", ns, metricOutboxDepth,
			"Maximum", jsii.Number(1000), period, evals, topicAction,
			awscloudwatch.TreatMissingData_BREACHING,
			"GoBridge shared-outbox pending backlog exceeded the HA threshold.")
		g.outboxDrainLatency = newRollupAlarm(c, "HAOutboxDrainLatency", ns, metricOutboxDrainLatency,
			"Maximum", jsii.Number(objectiveMS), period, evals, topicAction,
			awscloudwatch.TreatMissingData_NOT_BREACHING,
			"GoBridge shared-outbox drain latency exceeded the profile objective; inspect oldest pending records directly for backlog age.")
		g.outboxDepthFailures = newRollupAlarm(c, "HAOutboxDepthFailures", ns, metricOutboxDepthFailures,
			"Sum", jsii.Number(0), period, evals, topicAction,
			awscloudwatch.TreatMissingData_NOT_BREACHING, "GoBridge shared-outbox depth queries failed.")
		g.outboxRecordFailures = newRollupAlarm(c, "HAOutboxRecordFailures", ns, metricOutboxRecordFailures,
			"Sum", jsii.Number(0), period, evals, topicAction,
			awscloudwatch.TreatMissingData_NOT_BREACHING, "GoBridge shared-outbox records failed, including stale-fencing outcomes.")
		g.outboxDrainStalled = newRollupAlarm(c, "HAOutboxDrainStalled", ns, metricOutboxDrainStalled,
			"Sum", jsii.Number(0), period, evals, topicAction,
			awscloudwatch.TreatMissingData_NOT_BREACHING, "GoBridge shared-outbox drain stalled.")

		g.leaseExpiries = newRollupAlarm(c, "HALeaseExpiries", ns, metricLeaseExpiries,
			"Sum", jsii.Number(0), period, evals, topicAction,
			awscloudwatch.TreatMissingData_NOT_BREACHING, "GoBridge lease expiry or fail-closed step-down observed.")
		g.leaseTransfers = newRollupAlarm(c, "HALeaseTransfers", ns, metricLeaseTransfers,
			"Sum", jsii.Number(1), period, evals, topicAction,
			awscloudwatch.TreatMissingData_NOT_BREACHING, "More than one lease takeover in one evaluation window indicates flapping.")

		g.dlqDepth = newRollupAlarm(c, "HADLQDepth", ns, metricDLQDepth,
			"Maximum", jsii.Number(0), period, evals, topicAction,
			awscloudwatch.TreatMissingData_NOT_BREACHING, "GoBridge DLQ has outstanding entries.")
		g.dlqEntries = newRollupAlarm(c, "HADLQEntries", ns, metricDLQEntries,
			"Sum", jsii.Number(0), period, evals, topicAction,
			awscloudwatch.TreatMissingData_NOT_BREACHING, "GoBridge wrote a dead-letter entry.")
		g.dlqWriteFailures = newRollupAlarm(c, "HADLQWriteFailures", ns, metricDLQWriteFailures,
			"Sum", jsii.Number(0), period, evals, topicAction,
			awscloudwatch.TreatMissingData_NOT_BREACHING, "GoBridge failed to write a dead-letter entry.")

		// MQTT operational alarms the transport docs instruct operators to wire
		// (finding §8 alarms.go). Sum>0 over the window with NOT_BREACHING on missing
		// data (these are event counters, absent when healthy).
		g.mqttIngressPoison = newRollupAlarm(c, "HAMQTTIngressPoisonDropped", ns, metricMQTTIngressPoisonDropped,
			"Sum", jsii.Number(0), period, evals, topicAction,
			awscloudwatch.TreatMissingData_NOT_BREACHING,
			"GoBridge MQTT ingress dropped a poison message exceeding local payload/property caps.")
		g.reconcileFailures = newRollupAlarm(c, "HAReconcileFailures", ns, metricReconcileFailures,
			"Sum", jsii.Number(0), period, evals, topicAction,
			awscloudwatch.TreatMissingData_NOT_BREACHING,
			"GoBridge MQTT subscription reconcile failed (a permanent SUBACK rejection flaps the whole session).")
		g.mqttSessionTakeover = newRollupAlarm(c, "HAMQTTSessionTakeover", ns, metricMQTTSessionTakeover,
			"Sum", jsii.Number(0), period, evals, topicAction,
			awscloudwatch.TreatMissingData_NOT_BREACHING,
			"GoBridge MQTT session was taken over by another client on the same ClientID (identity collision or failover).")
		g.mqttQoSDowngraded = newRollupAlarm(c, "HAMQTTQoSDowngraded", ns, metricMQTTQoSDowngraded,
			"Sum", jsii.Number(0), period, evals, topicAction,
			awscloudwatch.TreatMissingData_NOT_BREACHING,
			"GoBridge MQTT broker granted a lower QoS than requested; delivery guarantees are weaker than configured.")

	}

	// Fleet convergence alarms for a coordinated cohort. Deliberately OUTSIDE the
	// deployment-shape branches above: the barrier runs wherever a composition
	// root drives it, and gating these on one facade would install them only where
	// they cannot fire.
	if props.EnableClusterRolloutAlarms {
		g.newClusterRolloutAlarms(c, rolloutMetricsNamespace(props), period, evals, topicAction)
	}

	return g
}

// rolloutMetricsNamespace is where a coordinated cohort publishes its runtime
// metrics: the HA construct's own namespace when the bundle is wired to one,
// otherwise the rollup namespace (overridable, defaulting to the runtime's).
func rolloutMetricsNamespace(props *AlarmsProps) string {
	if props.DynamoDBHA != nil {
		return props.DynamoDBHA.MetricsNamespace()
	}
	if props.RollupMetricsNamespace != nil && *props.RollupMetricsNamespace != "" {
		return *props.RollupMetricsNamespace
	}
	return rollupNamespaceDefault
}

func newDynamoDBAlarms(scope constructs.Construct, prefix string, table awsdynamodb.ITable,
	period awscdk.Duration, evals *float64, action awscloudwatch.IAlarmAction,
) (awscloudwatch.IAlarm, awscloudwatch.IAlarm) {
	operations := []awsdynamodb.Operation{
		awsdynamodb.Operation_GET_ITEM,
		awsdynamodb.Operation_PUT_ITEM,
		awsdynamodb.Operation_UPDATE_ITEM,
		awsdynamodb.Operation_DELETE_ITEM,
		awsdynamodb.Operation_QUERY,
		awsdynamodb.Operation_SCAN,
		awsdynamodb.Operation_TRANSACT_WRITE_ITEMS,
	}
	throttleMetric := table.MetricThrottledRequestsForOperations(&awsdynamodb.OperationsMetricOptions{
		Operations: &operations, Period: period, Statistic: jsii.String("Sum"),
	})
	throttle := awscloudwatch.NewAlarm(scope, jsii.String(prefix+"DynamoDBThrottles"), &awscloudwatch.AlarmProps{
		Metric: throttleMetric, Threshold: jsii.Number(0), EvaluationPeriods: evals,
		ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
		TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
		AlarmDescription:   jsii.String("GoBridge " + prefix + " DynamoDB table throttled runtime requests."),
	})
	throttle.AddAlarmAction(action)
	throttle.AddOkAction(action)

	systemMetric := table.MetricSystemErrorsForOperations(&awsdynamodb.SystemErrorsForOperationsMetricOptions{
		Operations: &operations, Period: period, Statistic: jsii.String("Sum"),
	})
	system := awscloudwatch.NewAlarm(scope, jsii.String(prefix+"DynamoDBSystemErrors"), &awscloudwatch.AlarmProps{
		Metric: systemMetric, Threshold: jsii.Number(0), EvaluationPeriods: evals,
		ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
		TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
		AlarmDescription:   jsii.String("GoBridge " + prefix + " DynamoDB table returned system errors."),
	})
	system.AddAlarmAction(action)
	system.AddOkAction(action)
	return throttle, system
}

// newRollupAlarm builds a dimensionless alarm on a custom runtime rollup
// metric. The alarm carries no DimensionsMap so it matches only the
// zero-dimension rollup series the exporter double-publishes.
func newRollupAlarm(scope constructs.Construct, id, namespace, metricName, statistic string,
	threshold *float64, period awscdk.Duration, evals *float64,
	action awscloudwatch.IAlarmAction, treatMissing awscloudwatch.TreatMissingData, desc string,
) awscloudwatch.IAlarm {
	metric := awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
		Namespace:  jsii.String(namespace),
		MetricName: jsii.String(metricName),
		Statistic:  jsii.String(statistic),
		Period:     period,
	})
	alarm := awscloudwatch.NewAlarm(scope, jsii.String(id), &awscloudwatch.AlarmProps{
		Metric:             metric,
		Threshold:          threshold,
		EvaluationPeriods:  evals,
		ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
		TreatMissingData:   treatMissing,
		AlarmDescription:   jsii.String(desc),
	})
	alarm.AddAlarmAction(action)
	alarm.AddOkAction(action)
	return alarm
}

func newUnhealthyAlarm(scope constructs.Construct, id string, tg elbv2.ApplicationTargetGroup,
	period awscdk.Duration, evals *float64, action awscloudwatch.IAlarmAction, desc string,
) awscloudwatch.IAlarm {
	metric := tg.Metrics().UnhealthyHostCount(&awscloudwatch.MetricOptions{
		Statistic: jsii.String("Maximum"),
		Period:    period,
	})
	alarm := awscloudwatch.NewAlarm(scope, jsii.String(id), &awscloudwatch.AlarmProps{
		Metric:             metric,
		Threshold:          jsii.Number(0),
		EvaluationPeriods:  evals,
		ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
		TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
		AlarmDescription:   jsii.String(desc),
	})
	alarm.AddAlarmAction(action)
	alarm.AddOkAction(action)
	return alarm
}

func new5xxAlarm(scope constructs.Construct, id string, tg elbv2.ApplicationTargetGroup,
	period awscdk.Duration, evals, threshold *float64, action awscloudwatch.IAlarmAction, desc string,
) awscloudwatch.IAlarm {
	metric := tg.Metrics().HttpCodeTarget(elbv2.HttpCodeTarget_TARGET_5XX_COUNT, &awscloudwatch.MetricOptions{
		Statistic: jsii.String("Sum"),
		Period:    period,
	})
	alarm := awscloudwatch.NewAlarm(scope, jsii.String(id), &awscloudwatch.AlarmProps{
		Metric:             metric,
		Threshold:          threshold,
		EvaluationPeriods:  evals,
		ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
		TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
		AlarmDescription:   jsii.String(desc),
	})
	alarm.AddAlarmAction(action)
	alarm.AddOkAction(action)
	return alarm
}

func (g *GoBridgeAlarms) ControlAbsenceAlarm() awscloudwatch.IAlarm      { return g.controlAbsence }
func (g *GoBridgeAlarms) WorkerDegradedAlarm() awscloudwatch.IAlarm      { return g.workerDegraded }
func (g *GoBridgeAlarms) EfsIOAlarm() awscloudwatch.IAlarm               { return g.efsIO }
func (g *GoBridgeAlarms) AlbUnhealthyControlAlarm() awscloudwatch.IAlarm { return g.albUnhealthyCtrl }
func (g *GoBridgeAlarms) AlbUnhealthyWorkerAlarm() awscloudwatch.IAlarm  { return g.albUnhealthyWrk }
func (g *GoBridgeAlarms) Alb5xxControlAlarm() awscloudwatch.IAlarm       { return g.alb5xxCtrl }
func (g *GoBridgeAlarms) Alb5xxWorkerAlarm() awscloudwatch.IAlarm        { return g.alb5xxWrk }

func (g *GoBridgeAlarms) OutboxDepthAlarm() awscloudwatch.IAlarm   { return g.outboxDepth }
func (g *GoBridgeAlarms) DLQEntriesAlarm() awscloudwatch.IAlarm    { return g.dlqEntries }
func (g *GoBridgeAlarms) LeaseExpiriesAlarm() awscloudwatch.IAlarm { return g.leaseExpiries }
func (g *GoBridgeAlarms) LeaseAcquireFailuresAlarm() awscloudwatch.IAlarm {
	return g.leaseAcquireFailures
}

func (g *GoBridgeAlarms) WarmStandbyUnavailableAlarm() awscloudwatch.IAlarm {
	return g.warmStandbyUnavailable
}
func (g *GoBridgeAlarms) FailureToFullDurationAlarm() awscloudwatch.IAlarm {
	return g.failureToFullDuration
}
func (g *GoBridgeAlarms) DynamoDBThrottleAlarms() []awscloudwatch.IAlarm {
	return append([]awscloudwatch.IAlarm(nil), g.dynamoThrottles...)
}
func (g *GoBridgeAlarms) DynamoDBSystemErrorAlarms() []awscloudwatch.IAlarm {
	return append([]awscloudwatch.IAlarm(nil), g.dynamoSystemErrors...)
}
func (g *GoBridgeAlarms) LeaseTransfersAlarm() awscloudwatch.IAlarm     { return g.leaseTransfers }
func (g *GoBridgeAlarms) OutboxDrainLatencyAlarm() awscloudwatch.IAlarm { return g.outboxDrainLatency }
func (g *GoBridgeAlarms) OutboxDepthFailuresAlarm() awscloudwatch.IAlarm {
	return g.outboxDepthFailures
}
func (g *GoBridgeAlarms) OutboxRecordFailuresAlarm() awscloudwatch.IAlarm {
	return g.outboxRecordFailures
}
func (g *GoBridgeAlarms) OutboxDrainStalledAlarm() awscloudwatch.IAlarm { return g.outboxDrainStalled }

func (g *GoBridgeAlarms) DLQDepthAlarm() awscloudwatch.IAlarm         { return g.dlqDepth }
func (g *GoBridgeAlarms) DLQWriteFailuresAlarm() awscloudwatch.IAlarm { return g.dlqWriteFailures }

func (g *GoBridgeAlarms) MQTTIngressPoisonAlarm() awscloudwatch.IAlarm { return g.mqttIngressPoison }
func (g *GoBridgeAlarms) ReconcileFailuresAlarm() awscloudwatch.IAlarm { return g.reconcileFailures }
func (g *GoBridgeAlarms) MQTTSessionTakeoverAlarm() awscloudwatch.IAlarm {
	return g.mqttSessionTakeover
}
func (g *GoBridgeAlarms) MQTTQoSDowngradedAlarm() awscloudwatch.IAlarm { return g.mqttQoSDowngraded }

func validateAlarmsProps(p *AlarmsProps) {
	if p == nil {
		panic("GoBridgeAlarms requires non-nil AlarmsProps.")
	}
	count := 0
	if p.Single != nil {
		count++
	}
	if p.Cluster != nil {
		count++
	}
	if p.DynamoDBHA != nil {
		count++
	}
	if count != 1 {
		panic(fmt.Sprintf(
			"GoBridgeAlarms requires exactly one of Single, Cluster, or DynamoDBHA (found %d). Pass the facade you instantiated.",
			count,
		))
	}
	if p.Efs == nil {
		panic("GoBridgeAlarms.Efs is required. Pass <facade>.EfsConfig().")
	}
	if p.Efs.FileSystem() == nil {
		panic("GoBridgeAlarms.Efs.FileSystem() returned nil. The EFS construct must be fully initialized before passing to GoBridgeAlarms.")
	}
	if p.AlarmTopic == nil {
		panic("GoBridgeAlarms.AlarmTopic is required.")
	}
}

func resolveClusterName(p *AlarmsProps) *string {
	if p.Cluster != nil {
		return p.Cluster.Cluster().ClusterName()
	}
	if p.DynamoDBHA != nil {
		return p.DynamoDBHA.Cluster().ClusterName()
	}
	return p.Single.Cluster().ClusterName()
}

func resolveServiceNames(p *AlarmsProps) (control, worker *string) {
	if p.Cluster != nil {
		return svcName(p.Cluster.ControlService()), svcName(p.Cluster.WorkerService())
	}
	if p.DynamoDBHA != nil {
		return svcName(p.DynamoDBHA.ControlService()), svcName(p.DynamoDBHA.WorkerService())
	}
	return svcName(p.Single.ControlService()), nil
}

func svcName(s awsecs.IService) *string { return s.ServiceName() }
