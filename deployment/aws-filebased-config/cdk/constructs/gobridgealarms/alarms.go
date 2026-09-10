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
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"
)

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
	controlServiceName, workerServiceNames := resolveServiceNames(props)

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
		// ONE alarm per worker-side service rather than one summed alarm over all of
		// them. Two reasons, and the second is the load-bearing one:
		//
		//   - the alarm names the slot that is short a task, which is the first thing
		//     an operator needs; and
		//   - a summed expression grows two metric inputs per slot, and a CloudWatch
		//     metric-math alarm has a hard cap on how many it may reference. A summed
		//     alarm would therefore turn a large enough roster into a DEPLOY-time
		//     failure, after the services and the retained rollout table already
		//     exist. Per-slot alarms are three inputs each, forever.
		for i, serviceName := range workerServiceNames {
			// The first alarm keeps the bare construct id so an existing single-worker
			// deployment does not see its alarm replaced.
			id := "WorkerDegraded"
			if i > 0 {
				id = fmt.Sprintf("WorkerDegraded%d", i)
			}
			running := awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
				Namespace:     jsii.String("ECS/ContainerInsights"),
				MetricName:    jsii.String("RunningTaskCount"),
				DimensionsMap: &map[string]*string{"ServiceName": serviceName, "ClusterName": clusterName},
				Statistic:     jsii.String("Minimum"),
				Period:        period,
			})
			desired := awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
				Namespace:     jsii.String("ECS/ContainerInsights"),
				MetricName:    jsii.String("DesiredTaskCount"),
				DimensionsMap: &map[string]*string{"ServiceName": serviceName, "ClusterName": clusterName},
				Statistic:     jsii.String("Maximum"),
				Period:        period,
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
			alarm := awscloudwatch.NewAlarm(c, jsii.String(id), &awscloudwatch.AlarmProps{
				Metric:             expr,
				Threshold:          jsii.Number(1),
				EvaluationPeriods:  evals,
				ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_OR_EQUAL_TO_THRESHOLD,
				TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
				AlarmDescription:   jsii.String("GoBridge worker service running task count below desired count."),
			})
			alarm.AddAlarmAction(topicAction)
			alarm.AddOkAction(topicAction)
			g.workerDegradedAlarms = append(g.workerDegradedAlarms, alarm)
			if g.workerDegraded == nil {
				g.workerDegraded = alarm
			}
		}
	}

	if props.DynamoDBHA != nil {
		controlRunning := awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace: jsii.String("ECS/ContainerInsights"), MetricName: jsii.String("RunningTaskCount"),
			DimensionsMap: &map[string]*string{"ServiceName": controlServiceName, "ClusterName": clusterName},
			Statistic:     jsii.String("Minimum"), Period: period,
		})
		workersExpr, workerMetrics := serviceCountSum(workerServiceNames, clusterName,
			"RunningTaskCount", "Minimum", "wr", period)
		using := map[string]awscloudwatch.IMetric{"control": controlRunning}
		for id, metric := range workerMetrics {
			using[id] = metric
		}
		warm := awscloudwatch.NewMathExpression(&awscloudwatch.MathExpressionProps{
			Expression:   jsii.String("IF(control + " + workersExpr + " < 2, 1, 0)"),
			UsingMetrics: &using,
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

	if props.Efs != nil && !props.DisableEfsIO {
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
			// The rollout table is nil on the autoscaled profile, which provisions
			// none. Where it exists it is the barrier's only coordination store AND
			// the boot-resolve gate, so throttling on it both stalls every rollout
			// and can stop a replaced task from starting at all — the one table whose
			// silence is least affordable.
			{name: "Rollout", table: data.RolloutTable()},
		} {
			if table.table == nil {
				continue
			}
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

		// MQTT operational alarms the transport docs instruct operators to wire.
		// Sum>0 over the window with NOT_BREACHING on missing data (these are event
		// counters, absent when healthy). Each is emitted per session_id, so these
		// dimensionless alarms match ONLY the rollup copies — the exporter must be
		// configured with the default rollup list or none of them can ever fire; see
		// docs/aws-deployment/alarms.md.
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
