//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// Asserting on alarms the emulator will never evaluate.
//
// The emulator has no alarm evaluation state machine, so a deployed alarm never
// changes state on its own and "did the alarm fire" is not a question a local
// run can ask. Two things it CAN ask, and both are what the gap would otherwise
// hide: whether the alarm's own query, replayed verbatim through GetMetricData
// against real datapoints, produces a value that crosses the threshold the
// deployment configured; and whether the action wired to the alarm actually
// reaches its subscriber when the alarm is put into ALARM by hand.
//
// What stays AWS's: the evaluation state machine that decides when to make that
// transition. No claim here rests on it.

// deployedAlarms is every alarm the deployment created.
func deployedAlarms(t *testing.T, ctx context.Context) []cwtypes.MetricAlarm {
	t.Helper()
	out, err := cloudwatch.NewFromConfig(localAWSConfig(t)).DescribeAlarms(ctx,
		&cloudwatch.DescribeAlarmsInput{MaxRecords: aws.Int32(100)})
	if err != nil {
		t.Fatalf("describe the deployed alarms: %v", err)
	}
	if len(out.MetricAlarms) == 0 {
		t.Fatal("the deployment created no alarms, so nothing would page an operator")
	}
	return out.MetricAlarms
}

// alarmOnMetric returns the deployed alarm watching one metric directly.
func alarmOnMetric(t *testing.T, alarms []cwtypes.MetricAlarm, namespace, metric string) cwtypes.MetricAlarm {
	t.Helper()
	for _, alarm := range alarms {
		if aws.ToString(alarm.Namespace) == namespace && aws.ToString(alarm.MetricName) == metric {
			return alarm
		}
	}
	t.Fatalf("no deployed alarm watches %s/%s, so that failure would be silent", namespace, metric)
	return cwtypes.MetricAlarm{}
}

// mathAlarm returns the deployed alarm whose name contains fragment and which is
// defined by a metric-math expression rather than a single metric.
func mathAlarm(t *testing.T, alarms []cwtypes.MetricAlarm, fragment string) cwtypes.MetricAlarm {
	t.Helper()
	for _, alarm := range alarms {
		if len(alarm.Metrics) == 0 {
			continue
		}
		if strings.Contains(aws.ToString(alarm.AlarmName), fragment) {
			return alarm
		}
	}
	t.Fatalf("no deployed metric-math alarm is named for %q", fragment)
	return cwtypes.MetricAlarm{}
}

// alarmNamed returns the deployed alarm whose name contains fragment.
func alarmNamed(t *testing.T, alarms []cwtypes.MetricAlarm, fragment string) cwtypes.MetricAlarm {
	t.Helper()
	for _, alarm := range alarms {
		if strings.Contains(aws.ToString(alarm.AlarmName), fragment) {
			return alarm
		}
	}
	t.Fatalf("no deployed alarm is named for %q", fragment)
	return cwtypes.MetricAlarm{}
}

// waitAlarmBreaches polls the alarm's own replayed query until it crosses the
// alarm's threshold, and fails with what the namespace actually holds when it
// never does.
//
// The wait is not politeness: the runtime's CloudWatch exporter batches and
// flushes on an interval, so a datapoint produced by an event a second ago is
// legitimately not visible yet. What is asserted is that it eventually is, and
// that the alarm's own definition crosses on it.
func waitAlarmBreaches(
	t *testing.T,
	ctx context.Context,
	alarm cwtypes.MetricAlarm,
	window, timeout time.Duration,
) float64 {
	t.Helper()
	value, found := 0.0, false
	err := pollUntil(ctx, 5*time.Second, timeout, func() (bool, error) {
		observed, ok := evaluateAlarm(t, ctx, alarm, window)
		if !ok {
			return false, nil
		}
		value, found = observed, true
		return alarmBreaches(alarm, observed), nil
	})
	if err != nil {
		logNamespaceMetrics(t, ctx, aws.ToString(alarm.Namespace))
		if !found {
			t.Fatalf("replaying the query of alarm %s produced no datapoints at all, so the alarm "+
				"could never leave INSUFFICIENT_DATA on this deployment",
				aws.ToString(alarm.AlarmName))
		}
		t.Fatalf("replaying the query of alarm %s gave %v, which does not cross its threshold %v",
			aws.ToString(alarm.AlarmName), value, aws.ToFloat64(alarm.Threshold))
	}
	return value
}

// logNamespaceMetrics prints what a namespace actually holds, so a replay that
// found nothing says whether the metric is absent or merely unmatched.
func logNamespaceMetrics(t *testing.T, ctx context.Context, namespace string) {
	t.Helper()
	out, err := cloudwatch.NewFromConfig(localAWSConfig(t)).ListMetrics(ctx,
		&cloudwatch.ListMetricsInput{Namespace: aws.String(namespace)})
	if err != nil {
		t.Logf("cannot list the metrics in %s: %v", namespace, err)
		return
	}
	names := make([]string, 0, len(out.Metrics))
	for _, metric := range out.Metrics {
		names = append(names, aws.ToString(metric.MetricName))
	}
	sort.Strings(names)
	t.Logf("%s holds %d metrics: %v", namespace, len(names), names)
}

// replayAlarm evaluates the alarm's OWN query through GetMetricData over the
// last window and returns the most recent value it produced.
//
// The queries are the alarm's, verbatim — including its math expression, its
// statistic, its period and its dimensions — so what is evaluated is what
// CloudWatch would evaluate, against datapoints that really exist.
func evaluateAlarm(
	t *testing.T,
	ctx context.Context,
	alarm cwtypes.MetricAlarm,
	window time.Duration,
) (float64, bool) {
	t.Helper()
	period := aws.ToInt32(alarm.Period)
	if period <= 0 {
		period = 60
	}
	queries := alarm.Metrics
	if len(queries) == 0 {
		queries = []cwtypes.MetricDataQuery{{
			Id:         aws.String("m1"),
			ReturnData: aws.Bool(true),
			MetricStat: &cwtypes.MetricStat{
				Metric: &cwtypes.Metric{
					Namespace:  alarm.Namespace,
					MetricName: alarm.MetricName,
					Dimensions: alarm.Dimensions,
				},
				Period: aws.Int32(period),
				Stat:   aws.String(string(alarm.Statistic)),
			},
		}}
	}
	now := time.Now().UTC()
	out, err := cloudwatch.NewFromConfig(localAWSConfig(t)).GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
		StartTime:         aws.Time(now.Add(-window)),
		EndTime:           aws.Time(now.Add(time.Minute)),
		MetricDataQueries: queries,
	})
	if err != nil {
		t.Fatalf("replay the query of alarm %s: %v", aws.ToString(alarm.AlarmName), err)
	}
	for _, result := range out.MetricDataResults {
		if len(result.Values) == 0 {
			continue
		}
		// Any period that crosses is the answer: an alarm evaluates each period,
		// so a single breaching one is a breach. Otherwise report the first value
		// so the failure says what the query actually produced.
		for _, value := range result.Values {
			if alarmBreaches(alarm, value) {
				return value, true
			}
		}
		return result.Values[0], true
	}
	return 0, false
}

// alarmBreaches reports whether a value crosses the alarm's own threshold in the
// alarm's own direction.
func alarmBreaches(alarm cwtypes.MetricAlarm, value float64) bool {
	threshold := aws.ToFloat64(alarm.Threshold)
	switch alarm.ComparisonOperator {
	case cwtypes.ComparisonOperatorGreaterThanThreshold:
		return value > threshold
	case cwtypes.ComparisonOperatorGreaterThanOrEqualToThreshold:
		return value >= threshold
	case cwtypes.ComparisonOperatorLessThanThreshold:
		return value < threshold
	case cwtypes.ComparisonOperatorLessThanOrEqualToThreshold:
		return value <= threshold
	default:
		return false
	}
}

// putDatapoint publishes one value for one metric, with the dimensions a query
// needs to find it.
func putDatapoint(
	t *testing.T,
	ctx context.Context,
	namespace, metric string,
	dimensions []cwtypes.Dimension,
	value float64,
) {
	t.Helper()
	if _, err := cloudwatch.NewFromConfig(localAWSConfig(t)).PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(namespace),
		MetricData: []cwtypes.MetricDatum{{
			MetricName: aws.String(metric),
			Dimensions: dimensions,
			Timestamp:  aws.Time(time.Now().UTC()),
			Value:      aws.Float64(value),
		}},
	}); err != nil {
		t.Fatalf("publish %s/%s=%v: %v", namespace, metric, value, err)
	}
}

// setAlarmState drives one alarm into a state by hand, which is the only way to
// exercise its ACTION on an emulator that never evaluates it.
func setAlarmState(t *testing.T, ctx context.Context, name, state, reason string) {
	t.Helper()
	if _, err := cloudwatch.NewFromConfig(localAWSConfig(t)).SetAlarmState(ctx, &cloudwatch.SetAlarmStateInput{
		AlarmName: aws.String(name), StateValue: cwtypes.StateValue(state),
		StateReason: aws.String(reason),
	}); err != nil {
		t.Fatalf("set alarm %s to %s: %v", name, state, err)
	}
}

// publishToTopic sends one message through an SNS topic, so a proof can tell a
// subscription that does not work from an alarm action that does not fire.
func publishToTopic(t *testing.T, ctx context.Context, topicARN, message string) {
	t.Helper()
	if _, err := sns.NewFromConfig(localAWSConfig(t)).Publish(ctx, &sns.PublishInput{
		TopicArn: aws.String(topicARN), Message: aws.String(message),
	}); err != nil {
		t.Fatalf("publish to %s: %v", topicARN, err)
	}
}

// subscribeQueueToTopic points an SNS topic at a queue so an alarm action is
// observable, and returns the queue's URL.
func subscribeQueueToTopic(t *testing.T, ctx context.Context, topicARN, queueName string) string {
	t.Helper()
	queueURL := createQueueOutsideTheStack(t, ctx, queueName)
	client := sqs.NewFromConfig(localAWSConfig(t))
	attributes, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(queueURL),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
	})
	if err != nil {
		t.Fatalf("read the ARN of the alarm sink queue: %v", err)
	}
	queueARN := attributes.Attributes[string(sqstypes.QueueAttributeNameQueueArn)]
	if _, err := sns.NewFromConfig(localAWSConfig(t)).Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: aws.String(topicARN), Protocol: aws.String("sqs"),
		Endpoint: aws.String(queueARN), ReturnSubscriptionArn: true,
	}); err != nil {
		t.Fatalf("subscribe the alarm sink queue to %s: %v", topicARN, err)
	}
	return queueURL
}
