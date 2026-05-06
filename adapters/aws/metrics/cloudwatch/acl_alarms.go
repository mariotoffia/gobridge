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
}

// DefaultAlarms returns the alarm definitions specified in ARCHITECTURE_NEW-STORES.md.
// snsTopicARN is optional; when non-empty, alarm actions are set to publish
// to the given SNS topic.
func DefaultAlarms(namespace, snsTopicARN string) []AlarmDefinition {
	if namespace == "" {
		namespace = shared.MetricNamespace
	}
	return []AlarmDefinition{
		{
			Name:        "GoBridge-OutboxDepth-Warning",
			MetricName:  shared.MetricOutboxDepth,
			Namespace:   namespace,
			Threshold:   1000,
			Period:      300,
			EvalPeriods: 1,
			Statistic:   cwtypes.StatisticMaximum,
			Comparison:  cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:    SeverityWarning,
			SNSTopicARN: snsTopicARN,
		},
		{
			Name:        "GoBridge-OutboxDepth-Critical",
			MetricName:  shared.MetricOutboxDepth,
			Namespace:   namespace,
			Threshold:   10000,
			Period:      300,
			EvalPeriods: 1,
			Statistic:   cwtypes.StatisticMaximum,
			Comparison:  cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:    SeverityCritical,
			SNSTopicARN: snsTopicARN,
		},
		{
			Name:        "GoBridge-LeaseExpiries-Warning",
			MetricName:  shared.MetricLeaseExpiries,
			Namespace:   namespace,
			Threshold:   0,
			Period:      300,
			EvalPeriods: 1,
			Statistic:   cwtypes.StatisticSum,
			Comparison:  cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:    SeverityWarning,
			SNSTopicARN: snsTopicARN,
		},
		{
			Name:        "GoBridge-DLQEntries-Warning",
			MetricName:  shared.MetricDLQEntries,
			Namespace:   namespace,
			Threshold:   0,
			Period:      300,
			EvalPeriods: 1,
			Statistic:   cwtypes.StatisticSum,
			Comparison:  cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:    SeverityWarning,
			SNSTopicARN: snsTopicARN,
		},
		{
			Name:        "GoBridge-LeaseAcquireFailures-Critical",
			MetricName:  shared.MetricLeaseAcquireFailures,
			Namespace:   namespace,
			Threshold:   3,
			Period:      300,
			EvalPeriods: 1,
			Statistic:   cwtypes.StatisticSum,
			Comparison:  cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:    SeverityCritical,
			SNSTopicARN: snsTopicARN,
		},
		{
			Name:        "GoBridge-SQSVisibilityExtensions-Warning",
			MetricName:  shared.MetricSQSVisibilityExtensions,
			Namespace:   namespace,
			Threshold:   100,
			Period:      300,
			EvalPeriods: 1,
			Statistic:   cwtypes.StatisticSum,
			Comparison:  cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:    SeverityWarning,
			SNSTopicARN: snsTopicARN,
		},
	}
}

// EnsureAlarms creates or updates CloudWatch alarms matching the given
// definitions. Existing alarms with the same name are updated in place.
func EnsureAlarms(ctx context.Context, client cloudWatchAPI, alarms []AlarmDefinition) error {
	for _, a := range alarms {
		input := &cloudwatch.PutMetricAlarmInput{
			AlarmName:          aws.String(a.Name),
			MetricName:         aws.String(a.MetricName),
			Namespace:          aws.String(a.Namespace),
			Threshold:          aws.Float64(a.Threshold),
			Period:             aws.Int32(a.Period),
			EvaluationPeriods:  aws.Int32(a.EvalPeriods),
			Statistic:          a.Statistic,
			ComparisonOperator: a.Comparison,
			TreatMissingData:   aws.String("notBreaching"),
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
