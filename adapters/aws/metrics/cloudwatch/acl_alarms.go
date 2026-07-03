package cloudwatch

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// AlarmSeverity indicates the severity of an alarm.
type AlarmSeverity string

const (
	SeverityWarning  AlarmSeverity = "WARNING"
	SeverityCritical AlarmSeverity = "CRITICAL"
)

// metricSQSVisibilityExtensions mirrors
// adapters/aws/transport/sqs.MetricSQSVisibilityExtensions. Kept as a
// local constant because this module must not import a sibling adapter
// (TESTS.md §3.1 / .go-arch-lint.yml).
const metricSQSVisibilityExtensions = "SQSVisibilityExtensions"

// AlarmDefinition describes a CloudWatch alarm that EnsureAlarms will create.
type AlarmDefinition struct {
	Name        string
	MetricName  string
	Namespace   string
	Threshold   float64
	Period      int32
	EvalPeriods int32
	Statistic   cwtypes.Statistic
	Comparison  cwtypes.ComparisonOperator
	Severity    AlarmSeverity
	SNSTopicARN string
	// TreatMissingData controls how the alarm evaluates periods with no
	// datapoints ("breaching", "notBreaching", "ignore", "missing").
	// Empty defaults to "notBreaching" — correct for event counters
	// whose absence is the healthy state, WRONG for continuously
	// emitted gauges whose absence means the emitter died (MF-4).
	TreatMissingData string
}

// DefaultRollupMetrics returns the metric names targeted by
// DefaultAlarms. Every one of them is emitted WITH dimensions by the
// runtime (DLQEntries → route_id/category, lease metrics → lease_id,
// SQSVisibilityExtensions → queue_url, OutboxDepth → partition), and a
// CloudWatch alarm without dimensions NEVER matches dimensioned data —
// so the exporter must double-publish a zero-dimension rollup copy of
// these metrics for the default alarms to fire (MF-4):
//
//	exporter, err := cloudwatch.New(ctx, shared.MetricNamespace,
//	    cloudwatch.WithRollupMetrics(cloudwatch.DefaultRollupMetrics()...),
//	)
func DefaultRollupMetrics() []string {
	return []string{
		shared.MetricOutboxDepth,
		shared.MetricLeaseExpiries,
		shared.MetricDLQEntries,
		shared.MetricLeaseAcquireFailures,
		metricSQSVisibilityExtensions,
	}
}

// DefaultAlarms returns the alarm definitions specified in ARCHITECTURE_NEW-STORES.md.
// snsTopicARN is optional; when non-empty, alarm actions are set to publish
// to the given SNS topic.
//
// The alarms carry no dimensions, so they only match the
// zero-dimension rollup series produced by
// WithRollupMetrics(DefaultRollupMetrics()...) — configure that option
// on the exporter or these alarms will never leave INSUFFICIENT_DATA
// silently (MF-4). To alarm per dimension value instead (e.g. one
// alarm per route_id), create per-dimension alarms via your deployment
// tooling; AlarmDefinition deliberately models the fleet-rollup shape.
//
// TreatMissingData: the OutboxDepth alarms use "breaching" because the
// runtime emits OutboxDepth continuously while an outbox is configured
// — silence there means the drainer (or the bridge) is dead, which is
// exactly what the alarm exists to catch. Do not install the
// OutboxDepth alarms on deployments without an outbox. The event
// counters (LeaseExpiries, DLQEntries, LeaseAcquireFailures,
// SQSVisibilityExtensions) use "notBreaching": no events is the
// healthy state.
func DefaultAlarms(namespace, snsTopicARN string) []AlarmDefinition {
	if namespace == "" {
		namespace = shared.MetricNamespace
	}
	return []AlarmDefinition{
		{
			Name:             "GoBridge-OutboxDepth-Warning",
			MetricName:       shared.MetricOutboxDepth,
			Namespace:        namespace,
			Threshold:        1000,
			Period:           300,
			EvalPeriods:      1,
			Statistic:        cwtypes.StatisticMaximum,
			Comparison:       cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:         SeverityWarning,
			SNSTopicARN:      snsTopicARN,
			TreatMissingData: "breaching",
		},
		{
			Name:             "GoBridge-OutboxDepth-Critical",
			MetricName:       shared.MetricOutboxDepth,
			Namespace:        namespace,
			Threshold:        10000,
			Period:           300,
			EvalPeriods:      1,
			Statistic:        cwtypes.StatisticMaximum,
			Comparison:       cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:         SeverityCritical,
			SNSTopicARN:      snsTopicARN,
			TreatMissingData: "breaching",
		},
		{
			Name:             "GoBridge-LeaseExpiries-Warning",
			MetricName:       shared.MetricLeaseExpiries,
			Namespace:        namespace,
			Threshold:        0,
			Period:           300,
			EvalPeriods:      1,
			Statistic:        cwtypes.StatisticSum,
			Comparison:       cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:         SeverityWarning,
			SNSTopicARN:      snsTopicARN,
			TreatMissingData: "notBreaching",
		},
		{
			Name:             "GoBridge-DLQEntries-Warning",
			MetricName:       shared.MetricDLQEntries,
			Namespace:        namespace,
			Threshold:        0,
			Period:           300,
			EvalPeriods:      1,
			Statistic:        cwtypes.StatisticSum,
			Comparison:       cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:         SeverityWarning,
			SNSTopicARN:      snsTopicARN,
			TreatMissingData: "notBreaching",
		},
		{
			Name:             "GoBridge-LeaseAcquireFailures-Critical",
			MetricName:       shared.MetricLeaseAcquireFailures,
			Namespace:        namespace,
			Threshold:        3,
			Period:           300,
			EvalPeriods:      1,
			Statistic:        cwtypes.StatisticSum,
			Comparison:       cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:         SeverityCritical,
			SNSTopicARN:      snsTopicARN,
			TreatMissingData: "notBreaching",
		},
		{
			Name:             "GoBridge-SQSVisibilityExtensions-Warning",
			MetricName:       metricSQSVisibilityExtensions,
			Namespace:        namespace,
			Threshold:        100,
			Period:           300,
			EvalPeriods:      1,
			Statistic:        cwtypes.StatisticSum,
			Comparison:       cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:         SeverityWarning,
			SNSTopicARN:      snsTopicARN,
			TreatMissingData: "notBreaching",
		},
	}
}

// EnsureAlarms creates or updates CloudWatch alarms matching the given
// definitions. Existing alarms with the same name are updated in place.
//
// Deployment wiring note: nothing in the runtime calls EnsureAlarms —
// it is a provisioning-time API. Call it from your bootstrap/CDK-glue
// alongside exporter construction, with the SAME namespace the
// exporter publishes to and WithRollupMetrics(DefaultRollupMetrics()...)
// configured on the exporter (MF-4).
func EnsureAlarms(ctx context.Context, client cloudWatchAPI, alarms []AlarmDefinition) error {
	for _, a := range alarms {
		treatMissing := a.TreatMissingData
		if treatMissing == "" {
			treatMissing = "notBreaching"
		}
		input := &cloudwatch.PutMetricAlarmInput{
			AlarmName:          aws.String(a.Name),
			MetricName:         aws.String(a.MetricName),
			Namespace:          aws.String(a.Namespace),
			Threshold:          aws.Float64(a.Threshold),
			Period:             aws.Int32(a.Period),
			EvaluationPeriods:  aws.Int32(a.EvalPeriods),
			Statistic:          a.Statistic,
			ComparisonOperator: a.Comparison,
			TreatMissingData:   aws.String(treatMissing),
			AlarmDescription:   aws.String(fmt.Sprintf("[%s] %s alarm for %s", a.Severity, a.MetricName, a.Namespace)),
		}

		if a.SNSTopicARN != "" {
			input.AlarmActions = []string{a.SNSTopicARN}
			input.OKActions = []string{a.SNSTopicARN}
		}

		if _, err := client.PutMetricAlarm(ctx, input); err != nil {
			return fmt.Errorf("cloudwatch: put metric alarm: %w", err)
		}
	}
	return nil
}
