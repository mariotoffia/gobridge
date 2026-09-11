package gobridgealarms

import (
	"fmt"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscloudwatch"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsdynamodb"
	awsecs "github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"
)

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

func (g *GoBridgeAlarms) ControlAbsenceAlarm() awscloudwatch.IAlarm { return g.controlAbsence }
func (g *GoBridgeAlarms) WorkerDegradedAlarm() awscloudwatch.IAlarm { return g.workerDegraded }

// WorkerDegradedAlarms returns one capacity alarm per worker-side service: one
// for the autoscaled profile, one per roster slot for the static member-slot
// profile. WorkerDegradedAlarm returns the first of them.
func (g *GoBridgeAlarms) WorkerDegradedAlarms() []awscloudwatch.IAlarm {
	return append([]awscloudwatch.IAlarm(nil), g.workerDegradedAlarms...)
}
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
	if p.Efs == nil && ((p.Single != nil && p.Single.EfsConfig() != nil) ||
		(p.Cluster != nil && p.Cluster.EfsConfig() != nil) ||
		(p.DynamoDBHA != nil && p.DynamoDBHA.EfsConfig() != nil)) {
		panic("GoBridgeAlarms.Efs is required. Pass <facade>.EfsConfig().")
	}
	if p.Efs != nil && p.Efs.FileSystem() == nil {
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

// resolveServiceNames returns the control service name and EVERY worker-side
// service name. The DynamoDB HA facade runs one autoscaled worker service, or one
// single-task service per static member slot; an alarm built on only the first
// would stay green while every other slot sat at zero tasks.
func resolveServiceNames(p *AlarmsProps) (control *string, workers []*string) {
	if p.Cluster != nil {
		return svcName(p.Cluster.ControlService()), []*string{svcName(p.Cluster.WorkerService())}
	}
	if p.DynamoDBHA != nil {
		names := make([]*string, 0, len(p.DynamoDBHA.WorkerServices()))
		for _, svc := range p.DynamoDBHA.WorkerServices() {
			names = append(names, svcName(svc))
		}
		return svcName(p.DynamoDBHA.ControlService()), names
	}
	return svcName(p.Single.ControlService()), nil
}

// serviceCountSum builds the metric-math term that sums one ECS Container
// Insights task-count metric across every named service, plus the metrics that
// term references. With a single service the term is that one metric id, so the
// autoscaled profile's expressions are unchanged in shape.
func serviceCountSum(
	serviceNames []*string,
	clusterName *string,
	metricName, statistic, idPrefix string,
	period awscdk.Duration,
) (string, map[string]awscloudwatch.IMetric) {
	if len(serviceNames) == 0 {
		// No service to observe. Return the literal 0 rather than an empty term,
		// which would splice into a syntactically invalid CloudWatch expression that
		// only fails when the alarm is evaluated in the account.
		return "0", map[string]awscloudwatch.IMetric{}
	}
	metrics := make(map[string]awscloudwatch.IMetric, len(serviceNames))
	terms := make([]string, 0, len(serviceNames))
	for i, name := range serviceNames {
		id := fmt.Sprintf("%s%d", idPrefix, i)
		metrics[id] = awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace:     jsii.String("ECS/ContainerInsights"),
			MetricName:    jsii.String(metricName),
			DimensionsMap: &map[string]*string{"ServiceName": name, "ClusterName": clusterName},
			Statistic:     jsii.String(statistic),
			Period:        period,
		})
		terms = append(terms, id)
	}
	return strings.Join(terms, " + "), metrics
}

func svcName(s awsecs.IService) *string { return s.ServiceName() }
