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
	awsecs "github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
)

// AlarmsProps configures the GoBridgeAlarms bundle. Exactly one of
// Single or Cluster MUST be supplied. Efs and AlarmTopic are
// required. Attachment is optional — when nil the ALB-related
// alarms are skipped (Single deployments without an ALB still get
// cluster + EFS alarms).
type AlarmsProps struct {
	Single  *gobridgesingle.GoBridgeSingle
	Cluster *gobridgecluster.GoBridgeCluster

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
}

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

	if props.Cluster != nil && !props.DisableWorkerDegraded {
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

	return g
}

func newUnhealthyAlarm(scope constructs.Construct, id string, tg elbv2.ApplicationTargetGroup,
	period awscdk.Duration, evals *float64, action awscloudwatch.IAlarmAction, desc string,
) awscloudwatch.IAlarm {
	metric := tg.MetricUnhealthyHostCount(&awscloudwatch.MetricOptions{
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
	metric := tg.MetricHttpCodeTarget(elbv2.HttpCodeTarget_TARGET_5XX_COUNT, &awscloudwatch.MetricOptions{
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
	if count != 1 {
		panic(fmt.Sprintf(
			"GoBridgeAlarms requires exactly one of Single or Cluster (found %d). Pass the facade you instantiated.",
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
	return p.Single.Cluster().ClusterName()
}

func resolveServiceNames(p *AlarmsProps) (control, worker *string) {
	if p.Cluster != nil {
		return svcName(p.Cluster.ControlService()), svcName(p.Cluster.WorkerService())
	}
	// worker is nil for Single
	return svcName(p.Single.ControlService()), nil
}

func svcName(s awsecs.IService) *string { return s.ServiceName() }
